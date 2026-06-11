package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"neutrino/internal/repo"
	"neutrino/internal/templates"
)

type NodeService struct {
	store               *repo.Store
	staleDeleteAfterSec int
	sync                SyncRequester
}

func NewNodeService(store *repo.Store, staleDeleteSec int, sync SyncRequester) *NodeService {
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
}

type XrayApplyPayload struct {
	Template       string            `json:"template"`
	Vars           map[string]string `json:"vars"`
	RollbackOnFail bool              `json:"rollback_on_fail,omitempty"`
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
	type item struct {
		UserID int64  `json:"user_id"`
		Email  string `json:"email"`
		Status string `json:"status"`
		UUID   string `json:"uuid,omitempty"`
	}
	items := make([]item, 0, len(users))
	for _, u := range users {
		it := item{UserID: u.ID, Email: u.Username, Status: u.Status}
		if u.ActiveLink != nil {
			it.UUID = u.ActiveLink.UUID
		}
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UserID < items[j].UserID })
	b, _ := json.Marshal(items)
	return SHA256Hex(string(b))
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
	p := XrayApplyPayload{
		Template:       tpl,
		Vars:           cfg.Vars,
		RollbackOnFail: cfg.RollbackOnFail,
	}
	b, _ := json.Marshal(p)
	return string(b), nil
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

func (s *NodeService) UsersForAgent(ctx context.Context, nodeID int64) ([]map[string]any, error) {
	node, err := s.Get(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if !node.Enabled {
		return []map[string]any{}, nil
	}
	users, err := s.store.ListUsersForNode(ctx, node.ID)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(users))
	for _, u := range users {
		item := map[string]any{
			"user_id": u.ID,
			"email":   u.Username,
			"status":  u.Status,
		}
		if u.ActiveLink != nil {
			item["uuid"] = u.ActiveLink.UUID
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *NodeService) EnqueueUsersSyncForEnabledNodes(ctx context.Context) error {
	nodes, err := s.store.ListEnabledNodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if err := s.EnqueueUsersSyncForNode(ctx, n.ID); err != nil {
			log.Printf("users_sync reconcile node=%d err=%v", n.ID, err)
		}
	}
	return nil
}

func (s *NodeService) EnqueueUsersSyncForNode(ctx context.Context, nodeID int64) error {
	return s.enqueueUsersSyncForNode(ctx, nodeID, false, "users_sync")
}

func (s *NodeService) EnqueueFullUsersSyncForNode(ctx context.Context, nodeID int64, reason string) error {
	return s.enqueueUsersSyncForNode(ctx, nodeID, true, reason)
}

func (s *NodeService) enqueueUsersSyncForNode(ctx context.Context, nodeID int64, force bool, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "users_sync"
	}
	node, err := s.Get(ctx, nodeID)
	if err != nil {
		return err
	}
	users := make([]repo.User, 0)
	if node.Enabled {
		users, err = s.store.ListUsersForNode(ctx, nodeID)
		if err != nil {
			return err
		}
	}
	desiredVersion := UsersDesiredVersion(users)
	if err := s.store.SetNodeDesiredUsersVersion(ctx, nodeID, desiredVersion); err != nil {
		return err
	}
	if !force && node.Enabled && strings.TrimSpace(node.AppliedUsersVersion) == strings.TrimSpace(desiredVersion) {
		return nil
	}
	jobID, enqueued, err := s.store.EnqueueNodeJob(ctx, nodeID, "users_sync", desiredVersion, "{}", 20, reason)
	if err != nil {
		return err
	}
	log.Printf("users_sync reconcile node=%d desired=%s job_id=%d enqueued=%t force=%t reason=%s", nodeID, desiredVersion, jobID, enqueued, force, reason)
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
