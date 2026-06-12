package service

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"neutrino/internal/repo"
	"neutrino/internal/templates"
	"neutrino/internal/usersync"
	"neutrino/internal/xraycfg"
)

// NodeStore is the narrow repo surface NodeService depends on, spanning both
// the admin-facing methods (nodes.go) and the agent-facing control-plane
// methods (nodes_controlplane.go). *repo.Store satisfies it.
type NodeStore interface {
	LifecycleSweeper

	// Node CRUD / enable
	ListNodes(ctx context.Context) ([]repo.Node, error)
	GetNode(ctx context.Context, nodeID int64) (repo.Node, error)
	CreateNode(ctx context.Context, in repo.CreateNodeInput) (repo.Node, error)
	UpdateNode(ctx context.Context, id int64, in repo.CreateNodeInput) (repo.Node, error)
	DeleteNode(ctx context.Context, id int64) error
	NodeDeleteWouldWidenAccess(ctx context.Context, id int64) (bool, error)
	SetNodeEnabled(ctx context.Context, nodeID int64, enabled bool) error
	DeleteStaleNodes(ctx context.Context, cutoff time.Time) ([]int64, error)
	ListEnabledNodes(ctx context.Context) ([]repo.Node, error)

	// Users sync
	PrepareUsersSync(ctx context.Context, nodeID int64, forceFull bool, reason string) (repo.PrepareUsersSyncResult, error)
	FinishUsersSyncJobForNode(ctx context.Context, nodeID, jobID int64, in repo.FinishNodeJobInput, appliedVersion string) (repo.FinishUsersSyncResult, error)
	MaterializeUsersSnapshotForAgent(ctx context.Context, nodeID int64) (string, []usersync.Item, error)
	ListNodesNeedingUsersSyncBackfill(ctx context.Context) ([]int64, error)
	SetNodeUsersSyncBackfillAt(ctx context.Context, nodeID int64, at time.Time) error

	// Node jobs / managed xray
	EnqueueNodeJob(ctx context.Context, nodeID int64, kind string, desiredVersion string, payloadJSON string, timeoutSec int, correlationID string) (jobID int64, enqueued bool, err error)
	ListNodeJobs(ctx context.Context, nodeID int64, limit int) ([]repo.NodeJob, error)
	HasPendingOrRunningNodeJob(ctx context.Context, nodeID int64, kind string) (bool, error)
	SweepTimedOutRunningJobs(ctx context.Context, timeoutByKind map[string]time.Duration, maxAttempts int) (int64, error)
	UpsertNodeDesiredState(ctx context.Context, nodeID int64, kind string, desiredVersion string, payloadJSON string) error
	GetNodeDesiredState(ctx context.Context, nodeID int64) (repo.NodeDesiredState, bool, error)
	SetNodeDesiredXrayVersion(ctx context.Context, nodeID int64, version string) error
	DeployManagedXray(ctx context.Context, nodeID int64, payloadJSON, desiredVersion string, timeoutSec int, correlationID string) (jobID int64, enqueued bool, err error)

	// Control plane: report / agent status (nodes_controlplane.go)
	ApplyNodeReportLatest(ctx context.Context, nodeID int64, in repo.NodeReportInput) (time.Time, error)
	UpdateNodeObservedIP(ctx context.Context, nodeID int64, ip string) error
	UpdateNodeAgentStatus(ctx context.Context, nodeID int64, lastSeenAt *time.Time, errMsg string) error

	// Control plane: job claim/finish
	ClaimNextNodeJobForNodeKinds(ctx context.Context, nodeID int64, allowedKinds []string) (repo.NodeJob, bool, error)
	FinishNodeJobForNode(ctx context.Context, nodeID int64, jobID int64, in repo.FinishNodeJobInput) (finalStatus string, err error)
	GetNodeJob(ctx context.Context, jobID int64) (repo.NodeJob, bool, error)
	SetNodeAppliedUsersVersion(ctx context.Context, nodeID int64, version string) error
	SetNodeAppliedXrayVersion(ctx context.Context, nodeID int64, version string) error
	MarkNodeXrayRolledBack(ctx context.Context, nodeID int64, appliedVersion string) error
	SetNodeJobError(ctx context.Context, nodeID int64, kind string, errKind string, errMsg string) error

	// Control plane: probes / metadata / metrics history
	InsertNodeProbeResult(ctx context.Context, nodeID int64, in repo.InsertNodeProbeResultInput) (int64, error)
	ListNodeProbeResults(ctx context.Context, nodeID int64, limit int) ([]repo.NodeProbeResult, error)
	GetNodeMetadata(ctx context.Context, nodeID int64) (repo.NodeMetadata, bool, error)
	UpsertNodeMetadata(ctx context.Context, nodeID int64, in repo.UpsertNodeMetadataInput) error
	ListNodeMetricSeries(ctx context.Context, nodeID int64, rangeName string, step string, now time.Time) ([]repo.NodeMetricSeriesPoint, error)
	GetLatestNodeMetricDetails(ctx context.Context, nodeID int64) (repo.NodeMetricDetails, bool, error)
	GetLatestNodeStaticFacts(ctx context.Context, nodeID int64) (repo.NodeStaticFacts, bool, error)
	ListNodeStaticFacts(ctx context.Context, nodeID int64, limit int) ([]repo.NodeStaticFacts, error)

	// Control plane: enrollment / cert pins
	CreateNodeEnrollCode(ctx context.Context, nodeID int64, ttl time.Duration) (repo.NodeEnrollCode, error)
	GetNodeEnrollCode(ctx context.Context, nodeID int64) (repo.NodeEnrollCode, bool, error)
	ValidateNodeEnrollCode(ctx context.Context, nodeID int64, code string) error
	CompleteNodeEnroll(ctx context.Context, nodeID int64, code string, cert *x509.Certificate, now time.Time) (int64, error)
	RenewNodeCertPin(ctx context.Context, nodeID int64, purpose string, cert *x509.Certificate, now time.Time) (int64, error)
	RevokeNodeCertPin(ctx context.Context, nodeID int64, certSHA256 string, reason string) (int64, error)
	ListNodeCertPins(ctx context.Context, nodeID int64, limit int) ([]repo.NodeCertPin, error)
}

var _ NodeStore = (*repo.Store)(nil)

type NodeService struct {
	store               NodeStore
	staleDeleteAfterSec int
	sync                SyncRequester
}

func NewNodeService(store NodeStore, staleDeleteSec int, sync SyncRequester) *NodeService {
	return &NodeService{store: store, staleDeleteAfterSec: staleDeleteSec, sync: sync}
}

type NodeExtra struct {
	Xray *NodeExtraXray `json:"xray,omitempty"`
}

type NodeExtraXray struct {
	// Template is an optional full Xray config template (JSON text).
	// Keep secrets node-local and reference them as env placeholders.
	Template       string            `json:"template"`
	Vars           map[string]string `json:"vars"`
	RollbackOnFail bool              `json:"rollback_on_fail"`
	// CustomOutbounds/CustomRoutes are the structured, allowlisted config
	// extension (module 4). Validated by xraycfg on save and again on the
	// agent; merged into the rendered config after var expansion.
	CustomOutbounds []xraycfg.Outbound `json:"custom_outbounds,omitempty"`
	CustomRoutes    []xraycfg.Route    `json:"custom_routes,omitempty"`
}

type XrayApplyPayload struct {
	Template        string             `json:"template"`
	Vars            map[string]string  `json:"vars"`
	RollbackOnFail  bool               `json:"rollback_on_fail,omitempty"`
	CustomOutbounds []xraycfg.Outbound `json:"custom_outbounds,omitempty"`
	CustomRoutes    []xraycfg.Route    `json:"custom_routes,omitempty"`
}

type XrayRollbackPayload struct {
	BackupName string `json:"backup_name,omitempty"`
}

type ManagedXrayResult struct {
	DesiredVersion string
	Enqueued       bool
	JobID          int64
}

type NodeUpdateResult struct {
	Before      repo.Node
	Node        repo.Node
	ManagedXray ManagedXrayResult
}

type NodeDeleteResult struct {
	Node              repo.Node
	PendingDelete     bool
	DisabledForDelete bool
	Deleted           bool
}

var (
	ErrInvalidManagedXrayConfig = errors.New("invalid managed xray config")
	ErrNodeNotManaged           = errors.New("node is not managed")
	ErrNodeCoreTypeNotXray      = errors.New("node core_type is not xray")
)

func (s *NodeService) CleanupStaleNodes(ctx context.Context) error {
	if s.staleDeleteAfterSec <= 0 {
		return nil
	}
	cutoff := time.Now().UTC().Add(-time.Duration(s.staleDeleteAfterSec) * time.Second)
	disabled, err := s.store.DeleteStaleNodes(ctx, cutoff)
	if err != nil {
		return err
	}
	if len(disabled) > 0 {
		log.Printf("disabled stale nodes pending cleanup: %v (cutoff=%s)", disabled, cutoff.Format(time.RFC3339))
		if s.sync != nil {
			for _, nodeID := range disabled {
				s.sync.RequestUsersSyncForNodeNow(ctx, nodeID)
			}
		}
	}
	return nil
}

func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func UsersDesiredVersion(users []repo.User) string {
	items := make([]usersync.Item, 0, len(users))
	for _, u := range users {
		it := usersync.Item{UserID: u.ID, Email: u.Username, Status: u.Status}
		if u.ActiveLink != nil {
			it.UUID = u.ActiveLink.UUID
		}
		items = append(items, it)
	}
	return usersync.HashItems(items)
}

func ParseNodeExtra(raw string) (NodeExtra, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return NodeExtra{}, nil
	}
	var ex NodeExtra
	if err := json.Unmarshal([]byte(raw), &ex); err != nil {
		return NodeExtra{}, err
	}
	if ex.Xray != nil {
		if err := xraycfg.Validate(ex.Xray.CustomOutbounds, ex.Xray.CustomRoutes); err != nil {
			return NodeExtra{}, err
		}
	}
	return ex, nil
}

func ValidateManagedXrayInput(in repo.CreateNodeInput) error {
	coreType := strings.TrimSpace(in.CoreType)
	if coreType == "" {
		coreType = "xray"
	}
	if in.Managed && coreType == "xray" {
		_, err := ParseNodeExtra(in.ExtraJSON)
		return err
	}
	return nil
}

func BuildManagedXrayApplyPayload(node repo.Node) (string, error) {
	ex, err := ParseNodeExtra(node.ExtraJSON)
	if err != nil {
		return "", err
	}
	cfg := ex.Xray
	if cfg == nil {
		cfg = &NodeExtraXray{RollbackOnFail: true}
	}
	tpl := strings.TrimSpace(cfg.Template)
	if tpl == "" {
		tpl = templates.XrayConfigTemplate
	}
	// With customs present, brand the template with the capability marker:
	// agents that understand custom config strip it before JSON validation;
	// older agents fail the apply loudly instead of silently shipping a
	// config with the custom routes missing.
	if len(cfg.CustomOutbounds) > 0 || len(cfg.CustomRoutes) > 0 {
		tpl = strings.TrimRight(tpl, "\n") + "\n" + xraycfg.Marker + "\n"
	}
	p := XrayApplyPayload{
		Template:        tpl,
		Vars:            cfg.Vars,
		RollbackOnFail:  cfg.RollbackOnFail,
		CustomOutbounds: cfg.CustomOutbounds,
		CustomRoutes:    cfg.CustomRoutes,
	}
	b, _ := json.Marshal(p)
	return string(b), nil
}

// PreviewManagedXrayConfig renders the final config the agent would install,
// for display before deploy. Panel-side vars are applied; node-local vars stay
// visible as ${VAR} placeholders — the panel never knows (and must never
// fabricate) node secrets. Numeric placeholders the template needs for JSON
// validity get the same fixed defaults the agent falls back to.
func PreviewManagedXrayConfig(node repo.Node) ([]byte, error) {
	ex, err := ParseNodeExtra(node.ExtraJSON)
	if err != nil {
		return nil, fmt.Errorf("%w: extra_json: %w", ErrInvalidManagedXrayConfig, err)
	}
	cfg := ex.Xray
	if cfg == nil {
		cfg = &NodeExtraXray{RollbackOnFail: true}
	}
	tpl := strings.TrimSpace(cfg.Template)
	if tpl == "" {
		tpl = templates.XrayConfigTemplate
	}
	rendered := os.Expand(tpl, func(k string) string {
		if cfg.Vars != nil {
			if v, ok := cfg.Vars[k]; ok {
				return v
			}
		}
		switch k {
		case "XRAY_VLESS_PORT":
			if node.Port > 0 {
				return strconv.Itoa(node.Port)
			}
			return "443"
		case "XRAY_API_PORT":
			return "10085"
		}
		// Keep the placeholder visible; string positions stay valid JSON.
		return "${" + k + "}"
	})
	raw := []byte(rendered)
	if stripped, had := xraycfg.StripMarker(rendered); had {
		raw = []byte(stripped)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%w: rendered template is not valid json (unresolved non-string vars?)", ErrInvalidManagedXrayConfig)
	}
	merged, err := xraycfg.MergeIntoRendered(raw, cfg.CustomOutbounds, cfg.CustomRoutes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidManagedXrayConfig, err)
	}
	if len(cfg.CustomOutbounds) == 0 && len(cfg.CustomRoutes) == 0 {
		// Normalize formatting so the preview is stable either way.
		var v any
		dec := json.NewDecoder(strings.NewReader(string(merged)))
		dec.UseNumber()
		if err := dec.Decode(&v); err == nil {
			if b, err := json.MarshalIndent(v, "", "  "); err == nil {
				return b, nil
			}
		}
	}
	return merged, nil
}

func BuildManagedXrayRollbackPayload(node repo.Node, backupName string) (string, error) {
	if _, err := ParseNodeExtra(node.ExtraJSON); err != nil {
		return "", err
	}
	p := XrayRollbackPayload{
		BackupName: strings.TrimSpace(backupName),
	}
	b, _ := json.Marshal(p)
	return string(b), nil
}

func (s *NodeService) List(ctx context.Context) ([]repo.Node, error) {
	return s.store.ListNodes(ctx)
}

func (s *NodeService) Get(ctx context.Context, nodeID int64) (repo.Node, error) {
	return s.store.GetNode(ctx, nodeID)
}

func (s *NodeService) ListJobs(ctx context.Context, nodeID int64, limit int) ([]repo.NodeJob, error) {
	return s.store.ListNodeJobs(ctx, nodeID, limit)
}

func (s *NodeService) Create(ctx context.Context, in repo.CreateNodeInput) (repo.Node, ManagedXrayResult, error) {
	if err := ValidateManagedXrayInput(in); err != nil {
		return repo.Node{}, ManagedXrayResult{}, err
	}
	node, err := s.store.CreateNode(ctx, in)
	if err != nil {
		return repo.Node{}, ManagedXrayResult{}, err
	}
	result, err := s.EnsureManagedXrayDesiredAndMaybeEnqueue(ctx, node)
	if err != nil {
		log.Printf("ensure managed xray desired failed node=%d: %v", node.ID, err)
		return node, ManagedXrayResult{}, nil
	}
	return node, result, nil
}

func (s *NodeService) Update(ctx context.Context, nodeID int64, in repo.CreateNodeInput) (NodeUpdateResult, error) {
	if err := ValidateManagedXrayInput(in); err != nil {
		return NodeUpdateResult{}, err
	}
	before, err := s.Get(ctx, nodeID)
	if err != nil {
		return NodeUpdateResult{}, err
	}
	node, err := s.store.UpdateNode(ctx, nodeID, in)
	if err != nil {
		return NodeUpdateResult{}, err
	}
	s.syncAfterEnabledTransition(ctx, nodeID, before.Enabled, node.Enabled)
	managed, err := s.EnsureManagedXrayDesiredAndMaybeEnqueue(ctx, node)
	if err != nil {
		log.Printf("ensure managed xray desired failed node=%d: %v", node.ID, err)
		return NodeUpdateResult{Before: before, Node: node}, nil
	}
	return NodeUpdateResult{Before: before, Node: node, ManagedXray: managed}, nil
}

func (s *NodeService) Delete(ctx context.Context, nodeID int64) (NodeDeleteResult, error) {
	node, err := s.Get(ctx, nodeID)
	if err != nil {
		return NodeDeleteResult{}, err
	}
	blocked, err := s.store.NodeDeleteWouldWidenAccess(ctx, nodeID)
	if err != nil {
		return NodeDeleteResult{}, err
	}
	if blocked {
		return NodeDeleteResult{}, repo.ErrNodeDeleteWouldWidenAccess
	}
	if node.Enabled {
		if err := s.store.SetNodeEnabled(ctx, nodeID, false); err != nil {
			return NodeDeleteResult{}, err
		}
		if s.sync != nil {
			s.sync.RequestUsersSyncForNodeNow(ctx, nodeID)
		}
		return NodeDeleteResult{Node: node, PendingDelete: true, DisabledForDelete: true}, nil
	}
	emptyUsersVersion := UsersDesiredVersion([]repo.User{})
	if strings.TrimSpace(node.DesiredUsersVersion) != emptyUsersVersion ||
		strings.TrimSpace(node.AppliedUsersVersion) != emptyUsersVersion {
		if s.sync != nil {
			s.sync.RequestUsersSyncForNodeNow(ctx, nodeID)
		}
		return NodeDeleteResult{Node: node, PendingDelete: true}, nil
	}
	if err := s.store.DeleteNode(ctx, nodeID); err != nil {
		return NodeDeleteResult{}, err
	}
	return NodeDeleteResult{Node: node, Deleted: true}, nil
}

func (s *NodeService) SetEnabled(ctx context.Context, nodeID int64, enabled bool) error {
	if err := s.store.SetNodeEnabled(ctx, nodeID, enabled); err != nil {
		return err
	}
	if s.sync == nil {
		return nil
	}
	if enabled {
		s.sync.RequestUsersSyncNow(ctx)
	} else {
		s.sync.RequestUsersSyncForNodeNow(ctx, nodeID)
	}
	return nil
}

// MaterializeUsersForAgent serves the versioned full snapshot for
// GET /agent/users: it materializes the current canonical DB users, persists
// the snapshot, and updates desired_users_version atomically — never an
// existing stale snapshot, and never enqueueing a job.
func (s *NodeService) MaterializeUsersForAgent(ctx context.Context, nodeID int64) (string, []usersync.Item, error) {
	return s.store.MaterializeUsersSnapshotForAgent(ctx, nodeID)
}

// FinishUsersSyncJob runs the users_sync terminal transition plus every
// follow-up decision (version validation, delta-ready marker, stale-delta
// cleanup, forced repair, follow-up reconcile) in one repo transaction.
func (s *NodeService) FinishUsersSyncJob(ctx context.Context, nodeID, jobID int64, in repo.FinishNodeJobInput, appliedVersion string) (repo.FinishUsersSyncResult, error) {
	return s.store.FinishUsersSyncJobForNode(ctx, nodeID, jobID, in, appliedVersion)
}

// refreshLifecycleNoSync runs the shared global lifecycle sweeps (see
// users.go). It must run before — never inside — the users-sync preparation
// transaction, and once per reconcile cycle.
func (s *NodeService) refreshLifecycleNoSync(ctx context.Context) (changed bool, err error) {
	res, err := refreshLifecycleNoSync(ctx, s.store)
	return res.Changed(), err
}

func (s *NodeService) EnqueueUsersSyncForEnabledNodes(ctx context.Context) error {
	// One lifecycle refresh per reconcile cycle. Changes are consumed by this
	// very cycle (every enabled node is prepared below), so no extra global
	// reconcile signal is needed here.
	if _, err := s.refreshLifecycleNoSync(ctx); err != nil {
		return err
	}
	nodes, err := s.store.ListEnabledNodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if _, err := s.store.PrepareUsersSync(ctx, n.ID, false, "reconcile"); err != nil {
			log.Printf("users_sync reconcile node=%d err=%v", n.ID, err)
		}
	}
	return nil
}

func (s *NodeService) EnqueueUsersSyncForNode(ctx context.Context, nodeID int64) error {
	changed, err := s.refreshLifecycleNoSync(ctx)
	if err != nil {
		return err
	}
	res, err := s.store.PrepareUsersSync(ctx, nodeID, false, "reconcile")
	if err != nil {
		return err
	}
	log.Printf("users_sync reconcile node=%d desired=%s job_id=%d enqueued=%t mode=%s", nodeID, res.TargetVersion, res.JobID, res.Enqueued, res.Mode)
	if changed && s.sync != nil {
		// Lifecycle sweeps are global mutations discovered while preparing a
		// single node; the other nodes must reconcile too. The Now variant is
		// not throttled, so the signal cannot be silently dropped.
		s.sync.RequestUsersSyncNow(ctx)
	}
	return nil
}

func (s *NodeService) EnqueueFullUsersSyncForNode(ctx context.Context, nodeID int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "repair"
	}
	res, err := s.store.PrepareUsersSync(ctx, nodeID, true, reason)
	if err != nil {
		return err
	}
	log.Printf("users_sync forced full node=%d desired=%s job_id=%d enqueued=%t reason=%s", nodeID, res.TargetVersion, res.JobID, res.Enqueued, reason)
	return nil
}

// EnqueueUsersSyncBaselineBackfill sends one forced full users sync to every
// enabled node that has never proven a schema-1 versioned baseline. It runs
// at startup (after migrations) and is idempotent via the per-node
// users_sync_baseline_backfill_at marker; a terminally failed backfill job
// clears that marker so the next startup retries. Nodes that stay schema-0
// (legacy agents) keep converging through ordinary full sync, and any later
// accepted schema-1 full success sets the delta-ready marker without another
// backfill.
func (s *NodeService) EnqueueUsersSyncBaselineBackfill(ctx context.Context) error {
	ids, err := s.store.ListNodesNeedingUsersSyncBackfill(ctx)
	if err != nil {
		return err
	}
	for _, nodeID := range ids {
		if _, err := s.store.PrepareUsersSync(ctx, nodeID, true, "baseline_backfill"); err != nil {
			log.Printf("users_sync baseline backfill node=%d err=%v", nodeID, err)
			continue
		}
		if err := s.store.SetNodeUsersSyncBackfillAt(ctx, nodeID, time.Now().UTC()); err != nil {
			log.Printf("users_sync baseline backfill mark node=%d err=%v", nodeID, err)
		}
	}
	return nil
}

func (s *NodeService) EnsureManagedXrayDesiredAndMaybeEnqueue(ctx context.Context, node repo.Node) (ManagedXrayResult, error) {
	if node.ID <= 0 {
		return ManagedXrayResult{}, fmt.Errorf("invalid node id")
	}
	if !node.Managed || node.CoreType != "xray" {
		return ManagedXrayResult{}, nil
	}
	payload, err := BuildManagedXrayApplyPayload(node)
	if err != nil {
		return ManagedXrayResult{}, fmt.Errorf("%w: extra_json: %w", ErrInvalidManagedXrayConfig, err)
	}
	desiredVersion := SHA256Hex(payload)
	if err := s.store.UpsertNodeDesiredState(ctx, node.ID, "xray_apply", desiredVersion, payload); err != nil {
		return ManagedXrayResult{}, err
	}
	if err := s.store.SetNodeDesiredXrayVersion(ctx, node.ID, desiredVersion); err != nil {
		return ManagedXrayResult{}, err
	}
	if !node.Enabled {
		return ManagedXrayResult{DesiredVersion: desiredVersion}, nil
	}
	jobID, enqueued, err := s.store.EnqueueNodeJob(ctx, node.ID, "xray_apply", desiredVersion, payload, 120, "xray_apply")
	if err != nil {
		return ManagedXrayResult{}, err
	}
	return ManagedXrayResult{DesiredVersion: desiredVersion, Enqueued: enqueued, JobID: jobID}, nil
}

func (s *NodeService) EnqueueManagedXrayReloadForEnabledNodes(ctx context.Context) error {
	nodes, err := s.store.ListEnabledNodes(ctx)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if !node.Managed || node.CoreType != "xray" {
			continue
		}
		payload, err := BuildManagedXrayApplyPayload(node)
		if err != nil {
			log.Printf("build managed xray payload for node=%d: %v", node.ID, err)
			continue
		}
		desiredVersion := SHA256Hex(payload)
		if err := s.store.UpsertNodeDesiredState(ctx, node.ID, "xray_apply", desiredVersion, payload); err != nil {
			log.Printf("upsert desired xray state for node=%d: %v", node.ID, err)
			continue
		}
		if err := s.store.SetNodeDesiredXrayVersion(ctx, node.ID, desiredVersion); err != nil {
			log.Printf("set desired xray version for node=%d: %v", node.ID, err)
			continue
		}
		if _, _, err := s.store.EnqueueNodeJob(ctx, node.ID, "xray_apply", desiredVersion, payload, 120, "user_disconnect"); err != nil {
			log.Printf("enqueue xray reload for node=%d: %v", node.ID, err)
		}
	}
	return nil
}

func (s *NodeService) ReconcileManagedXray(ctx context.Context) error {
	nodes, err := s.store.ListEnabledNodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if !n.Managed {
			log.Printf("skip xray reconcile node=%d reason=unmanaged", n.ID)
			continue
		}
		ds, ok, err := s.store.GetNodeDesiredState(ctx, n.ID)
		if err != nil {
			log.Printf("get desired xray state node=%d err=%v", n.ID, err)
			continue
		}
		if !ok {
			log.Printf("skip xray reconcile node=%d reason=no_desired_state", n.ID)
			continue
		}
		if err := s.store.SetNodeDesiredXrayVersion(ctx, n.ID, ds.DesiredVersion); err != nil {
			log.Printf("set desired xray version node=%d desired=%s err=%v", n.ID, ds.DesiredVersion, err)
			continue
		}
		if strings.TrimSpace(n.AppliedXrayVersion) == strings.TrimSpace(ds.DesiredVersion) {
			log.Printf("skip xray reconcile node=%d reason=already_applied desired=%s", n.ID, ds.DesiredVersion)
			continue
		}
		has, err := s.store.HasPendingOrRunningNodeJob(ctx, n.ID, "xray_apply")
		if err != nil {
			log.Printf("check pending xray job node=%d err=%v", n.ID, err)
			continue
		}
		if has {
			log.Printf("skip xray reconcile node=%d reason=job_inflight desired=%s", n.ID, ds.DesiredVersion)
			continue
		}
		jobID, enqueued, err := s.store.EnqueueNodeJob(ctx, n.ID, "xray_apply", ds.DesiredVersion, ds.PayloadJSON, 120, "xray_apply")
		if err != nil {
			log.Printf("enqueue xray_apply node=%d desired=%s err=%v", n.ID, ds.DesiredVersion, err)
			continue
		}
		log.Printf("xray reconcile node=%d desired=%s job_id=%d enqueued=%t", n.ID, ds.DesiredVersion, jobID, enqueued)
	}
	return nil
}

// PreviewManagedXray validates and renders the deploy preview for a node,
// optionally against a draft extra_json that has not been saved yet.
func (s *NodeService) PreviewManagedXray(ctx context.Context, nodeID int64, draftExtraJSON *string) ([]byte, error) {
	node, err := s.Get(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if !node.Managed {
		return nil, ErrNodeNotManaged
	}
	if node.CoreType != "xray" {
		return nil, ErrNodeCoreTypeNotXray
	}
	if draftExtraJSON != nil {
		node.ExtraJSON = *draftExtraJSON
	}
	return PreviewManagedXrayConfig(node)
}

func (s *NodeService) DeployManagedXray(ctx context.Context, nodeID int64) (ManagedXrayResult, error) {
	node, err := s.Get(ctx, nodeID)
	if err != nil {
		return ManagedXrayResult{}, err
	}
	if !node.Managed {
		return ManagedXrayResult{}, ErrNodeNotManaged
	}
	if node.CoreType != "xray" {
		return ManagedXrayResult{}, ErrNodeCoreTypeNotXray
	}
	payload, err := BuildManagedXrayApplyPayload(node)
	if err != nil {
		return ManagedXrayResult{}, fmt.Errorf("%w: extra_json: %w", ErrInvalidManagedXrayConfig, err)
	}
	desiredVersion := SHA256Hex(payload)
	jobID, enqueued, err := s.store.DeployManagedXray(ctx, nodeID, payload, desiredVersion, 120, "xray_apply")
	if err != nil {
		return ManagedXrayResult{}, err
	}
	return ManagedXrayResult{DesiredVersion: desiredVersion, Enqueued: enqueued, JobID: jobID}, nil
}

func (s *NodeService) RollbackManagedXray(ctx context.Context, nodeID int64, backupName string) (ManagedXrayResult, error) {
	node, err := s.Get(ctx, nodeID)
	if err != nil {
		return ManagedXrayResult{}, err
	}
	if !node.Managed {
		return ManagedXrayResult{}, ErrNodeNotManaged
	}
	if node.CoreType != "xray" {
		return ManagedXrayResult{}, ErrNodeCoreTypeNotXray
	}
	payload, err := BuildManagedXrayRollbackPayload(node, backupName)
	if err != nil {
		return ManagedXrayResult{}, fmt.Errorf("%w: extra_json: %w", ErrInvalidManagedXrayConfig, err)
	}
	desiredVersion := SHA256Hex(payload)
	jobID, enqueued, err := s.store.EnqueueNodeJob(ctx, nodeID, "xray_rollback", desiredVersion, payload, 60, "xray_rollback")
	if err != nil {
		return ManagedXrayResult{}, err
	}
	return ManagedXrayResult{DesiredVersion: desiredVersion, Enqueued: enqueued, JobID: jobID}, nil
}

func (s *NodeService) SweepTimedOutJobs(ctx context.Context, maxAttempts int) (int64, error) {
	// Kind defaults only. Jobs carrying their own timeout_sec (e.g. probes)
	// are swept at timeout_sec plus a grace period, and any running job with
	// neither a per-job timeout nor a kind default is bounded by the repo's
	// global fallback, so no kind can wedge a node's serial job pipeline.
	timeoutByKind := map[string]time.Duration{
		"users_sync":    20 * time.Second,
		"xray_apply":    120 * time.Second,
		"xray_rollback": 60 * time.Second,
	}
	return s.store.SweepTimedOutRunningJobs(ctx, timeoutByKind, maxAttempts)
}

func (s *NodeService) syncAfterEnabledTransition(ctx context.Context, nodeID int64, wasEnabled, isEnabled bool) {
	if s.sync == nil {
		return
	}
	switch {
	case wasEnabled && !isEnabled:
		s.sync.RequestUsersSyncForNodeNow(ctx, nodeID)
	case !wasEnabled && isEnabled:
		s.sync.RequestUsersSyncNow(ctx)
	}
}
