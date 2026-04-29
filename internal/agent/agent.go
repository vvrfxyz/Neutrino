package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"neutrino/internal/hostnet"
	"neutrino/internal/xrayapi"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
)

type Agent struct {
	cfg            Config
	xray           *xrayapi.Client
	panel          *PanelClient
	panelMu        sync.RWMutex
	state          *StateStore
	queue          *DiskQueue
	sampler        *AccessSampler
	xrayConfigPath string
	xrayTestArgs   []string
	xrayReloadArgs []string
	xrayUsersMu    sync.Mutex

	realityPath string
	reality     *RealityConfig

	startedAt time.Time
	mu        sync.RWMutex

	lastNetInTotal  int64
	lastNetOutTotal int64
	lastNetAt       time.Time
	hasLastNet      bool
}

type StatEntry struct {
	Uplink   int64 `json:"uplink"`
	Downlink int64 `json:"downlink"`
}

type UserSyncItem struct {
	UserID int64  `json:"user_id"`
	Email  string `json:"email"`
	UUID   string `json:"uuid"`
	Status string `json:"status"`
}

type XrayApplyRequest struct {
	Template       string            `json:"template"`
	Vars           map[string]string `json:"vars"`
	RollbackOnFail bool              `json:"rollback_on_fail,omitempty"`
}

type XrayRollbackRequest struct {
	BackupName string `json:"backup_name,omitempty"` // basename only; if empty, rollback latest backup
}

type userSyncApplier interface {
	UpsertUser(ctx context.Context, email, uuid string) error
	RemoveUser(ctx context.Context, email string) error
}

type jobResult struct {
	Status         string
	Retryable      bool
	HTTPStatus     *int
	ResultJSON     any
	Error          string
	AppliedVersion string
}

func New(cfg Config) (*Agent, error) {
	if cfg.NodeID <= 0 {
		return nil, fmt.Errorf("NODE_ID is required")
	}
	if strings.TrimSpace(cfg.PanelMTLSURL) == "" {
		return nil, fmt.Errorf("PANEL_MTLS_URL is required")
	}
	if strings.TrimSpace(cfg.PanelMTLSCACertPath) == "" || strings.TrimSpace(cfg.PanelMTLSClientCertPath) == "" || strings.TrimSpace(cfg.PanelMTLSClientKeyPath) == "" {
		return nil, fmt.Errorf("panel mTLS client paths are required")
	}

	st := NewStateStore(cfg.StatePath)
	_ = st.Load()

	// Auto-enroll if cert/key/ca are missing.
	if err := ensureEnrolled(cfg); err != nil {
		return nil, err
	}

	panel, err := NewPanelClient(cfg.PanelMTLSURL, cfg.NodeID, cfg.PanelMTLSCACertPath, cfg.PanelMTLSClientCertPath, cfg.PanelMTLSClientKeyPath)
	if err != nil {
		return nil, err
	}

	xray := xrayapi.NewRaw(cfg.XrayAPIAddr, cfg.XrayInboundTag, cfg.XrayFlow)
	q := NewDiskQueue(cfg.QueueDir, cfg.QueueMaxBytes)

	testArgs, err := parseArgsJSON(cfg.XrayTestArgsJSON)
	if err != nil {
		return nil, fmt.Errorf("invalid XRAY_TEST_ARGS_JSON: %w", err)
	}
	reloadArgs, err := parseArgsJSON(cfg.XrayReloadArgsJSON)
	if err != nil {
		return nil, fmt.Errorf("invalid XRAY_RELOAD_ARGS_JSON: %w", err)
	}
	if strings.TrimSpace(cfg.XrayConfigPath) == "" {
		// Match upstream Xray container default confdir (/usr/local/etc/xray).
		cfg.XrayConfigPath = "/usr/local/etc/xray/config.json"
	}
	if !filepath.IsAbs(cfg.XrayConfigPath) {
		return nil, fmt.Errorf("XRAY_CONFIG_PATH must be an absolute path")
	}

	a := &Agent{
		cfg:            cfg,
		xray:           xray,
		state:          st,
		queue:          q,
		sampler:        NewAccessSampler(cfg.AccessZeroByteMaxPerMinPerUser, cfg.AccessZeroByteMaxPerMinPerUserDest),
		xrayConfigPath: filepath.Clean(cfg.XrayConfigPath),
		xrayTestArgs:   testArgs,
		xrayReloadArgs: reloadArgs,
		realityPath:    filepath.Join(filepath.Dir(cfg.StatePath), "reality.json"),
		startedAt:      time.Now().UTC(),
	}
	a.setPanelClient(panel)

	// Ensure REALITY key material exists locally (private key stays on node; panel only receives public params).
	if rc, err := ensureRealityConfig(a.realityPath); err == nil {
		a.reality = &rc
		// Best-effort: populate report payload used by Heartbeat.
		if p := a.buildNodeReportPayload(); p != nil {
			panel.SetReportPayload(*p)
		}
	} else {
		log.Printf("warn: ensure reality config failed: %v", err)
	}

	// Best-effort bootstrap: if xray config is missing, render a minimal config from the embedded template
	// and restart xray. This avoids a deadlock where xray can't start without config but agent depends on it.
	a.bootstrapXrayConfigIfMissing(context.Background())

	return a, nil
}

func (a *Agent) panelClient() *PanelClient {
	a.panelMu.RLock()
	defer a.panelMu.RUnlock()
	return a.panel
}

func (a *Agent) setPanelClient(p *PanelClient) {
	// Keep report payload in sync across panel client reloads (cert renew rotates TLS material).
	if p != nil {
		if rp := a.buildNodeReportPayload(); rp != nil {
			p.SetReportPayload(*rp)
		}
	}
	a.panelMu.Lock()
	a.panel = p
	a.panelMu.Unlock()
}

func ensureEnrolled(cfg Config) error {
	// If cert+key+ca already exist, do nothing.
	if fileExists(cfg.PanelMTLSClientCertPath) && fileExists(cfg.PanelMTLSClientKeyPath) && fileExists(cfg.PanelMTLSCACertPath) {
		return nil
	}
	if strings.TrimSpace(cfg.PanelURL) == "" {
		return fmt.Errorf("PANEL_URL is required for initial enroll")
	}
	if strings.TrimSpace(cfg.EnrollCode) == "" {
		return fmt.Errorf("ENROLL_CODE is required for initial enroll")
	}

	key, csrPEM, keyPEM, err := generateKeyAndCSR(cfg.NodeID)
	if err != nil {
		return err
	}
	_ = key // only used for sanity

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := Enroll(ctx, cfg.PanelURL, cfg.NodeID, cfg.EnrollCode, csrPEM)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(cfg.PanelMTLSClientCertPath), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.PanelMTLSClientKeyPath), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.PanelMTLSCACertPath), 0700); err != nil {
		return err
	}

	if err := writeFileAtomic(cfg.PanelMTLSClientKeyPath, []byte(keyPEM), 0600); err != nil {
		return err
	}
	if err := writeFileAtomic(cfg.PanelMTLSClientCertPath, []byte(resp.CertPEM), 0644); err != nil {
		return err
	}
	if err := writeFileAtomic(cfg.PanelMTLSCACertPath, []byte(resp.CABundlePEM), 0644); err != nil {
		return err
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func generateKeyAndCSR(nodeID int64) (*ecdsa.PrivateKey, string, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", "", err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkixNameForNode(nodeID),
	}, key)
	if err != nil {
		return nil, "", "", err
	}
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, "", "", err
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return key, csrPEM, keyPEM, nil
}

func pkixNameForNode(nodeID int64) pkix.Name {
	return pkix.Name{CommonName: fmt.Sprintf("node-%d", nodeID)}
}

func (a *Agent) Run(ctx context.Context) error {
	if strings.TrimSpace(a.cfg.AgentHTTPAddr) != "" {
		go a.startHealthServer(ctx)
	}
	log.Printf("[agent] access-log timezone = %s (AGENT_ACCESS_LOG_TZ=%q)", a.cfg.AccessLogLocation().String(), a.cfg.AccessLogTZ)
	log.Printf("agent: monthly-net location=%s", a.cfg.MonthLocation().String())
	go a.startCertRenewer(ctx)
	go a.restoreSyncedUsersOnce(ctx)
	go a.startJobRunner(ctx)
	go a.startUsageLoop(ctx)
	go a.startHeartbeat(ctx)
	<-ctx.Done()
	return nil
}

func (a *Agent) startHealthServer(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: a.cfg.AgentHTTPAddr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("agent health listening on %s", a.cfg.AgentHTTPAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("health server error: %v", err)
	}
}

func (a *Agent) restoreSyncedUsersOnce(ctx context.Context) {
	restoreCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	synced, err := a.restoreSyncedUsers(restoreCtx)
	if err != nil {
		log.Printf("restore synced xray users failed reason=startup synced=%d err=%v", synced, err)
		return
	}
	if synced > 0 {
		log.Printf("restored synced xray users reason=startup synced=%d", synced)
	}
}

func (a *Agent) restoreSyncedUsers(ctx context.Context) (synced int, err error) {
	if a == nil || a.state == nil || a.xray == nil {
		return 0, nil
	}
	a.xrayUsersMu.Lock()
	defer a.xrayUsersMu.Unlock()

	users := a.state.Snapshot().SyncedUsers
	if len(users) == 0 {
		return 0, nil
	}
	return restoreActiveUsers(ctx, a.xray, users)
}

func (a *Agent) startJobRunner(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		pc := a.panelClient()
		if pc == nil {
			time.Sleep(2 * time.Second)
			continue
		}
		jobCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
		job, ok, err := pc.ClaimJob(jobCtx, 25)
		cancel()
		if err != nil {
			log.Printf("claim job failed: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		if !ok || job == nil {
			continue
		}

		res := a.execJob(ctx, job)
		// Re-fetch panel client: cert renewal can hot-swap the client between claim and finish.
		pc = a.panelClient()
		if pc == nil {
			time.Sleep(2 * time.Second)
			continue
		}
		finishCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = pc.FinishJob(finishCtx, job.ID, FinishJobRequest{
			Status:         res.Status,
			Retryable:      res.Retryable,
			HTTPStatus:     res.HTTPStatus,
			ResultJSON:     res.ResultJSON,
			Error:          res.Error,
			AppliedVersion: res.AppliedVersion,
			Attempt:        job.Attempts,
		})
		cancel()
		if err != nil {
			log.Printf("finish job id=%d kind=%s err=%v", job.ID, job.Kind, err)
		}
	}
}

func (a *Agent) execJob(ctx context.Context, job *NodeJob) jobResult {
	if job == nil {
		return jobResult{Status: "failed", Retryable: true, Error: "nil job"}
	}
	timeout := 0
	if job.TimeoutSec != nil && *job.TimeoutSec > 0 {
		timeout = *job.TimeoutSec
	}
	runCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	}
	defer cancel()

	switch strings.TrimSpace(job.Kind) {
	case "users_sync":
		applied, synced, removed, err := a.execUsersSync(runCtx)
		if err != nil {
			return jobErr(err)
		}
		return jobResult{
			Status:    "succeeded",
			Retryable: false,
			ResultJSON: map[string]any{
				"synced":  synced,
				"removed": removed,
				"failed":  0,
			},
			AppliedVersion: applied,
		}
	case "xray_apply":
		var req XrayApplyRequest
		if err := json.Unmarshal([]byte(job.PayloadJSON), &req); err != nil {
			return jobResult{Status: "failed", Retryable: false, Error: "invalid payload_json"}
		}
		out, err := a.execXrayApply(runCtx, req)
		if err != nil {
			return jobErr(err)
		}
		return jobResult{Status: "succeeded", ResultJSON: out, AppliedVersion: strings.TrimSpace(job.DesiredVersion)}
	case "xray_rollback":
		var req XrayRollbackRequest
		if err := json.Unmarshal([]byte(job.PayloadJSON), &req); err != nil {
			return jobResult{Status: "failed", Retryable: false, Error: "invalid payload_json"}
		}
		out, err := a.execXrayRollback(runCtx, req)
		if err != nil {
			return jobErr(err)
		}
		appliedVersion := "rollback"
		if name, ok := out["backup_name"].(string); ok && strings.TrimSpace(name) != "" {
			appliedVersion = "rollback:" + strings.TrimSpace(name)
		}
		return jobResult{Status: "succeeded", ResultJSON: out, AppliedVersion: appliedVersion}
	default:
		return jobResult{Status: "failed", Retryable: false, Error: "unknown job kind"}
	}
}

type JobError struct {
	Retryable  bool
	Msg        string
	ResultJSON any
	HTTPStatus *int
}

func (e JobError) Error() string { return e.Msg }

func jobErr(err error) jobResult {
	if err == nil {
		return jobResult{Status: "succeeded"}
	}
	if je, ok := err.(JobError); ok {
		return jobResult{Status: "failed", Retryable: je.Retryable, HTTPStatus: je.HTTPStatus, ResultJSON: je.ResultJSON, Error: je.Msg}
	}
	return jobResult{Status: "failed", Retryable: true, Error: err.Error()}
}

func (a *Agent) execUsersSync(ctx context.Context) (appliedVersion string, synced int, removed int, err error) {
	pc := a.panelClient()
	if pc == nil {
		return "", 0, 0, JobError{Retryable: true, Msg: "panel client not ready", ResultJSON: map[string]any{"synced": 0, "removed": 0, "failed": 0}}
	}
	prevUsers := a.state.Snapshot().SyncedUsers
	users, err := pc.FetchUsers(ctx)
	if err != nil {
		return "", 0, 0, JobError{Retryable: true, Msg: err.Error(), ResultJSON: map[string]any{"synced": 0, "removed": 0, "failed": 0}}
	}
	a.xrayUsersMu.Lock()
	defer a.xrayUsersMu.Unlock()
	synced, removed, err = applyUsersSync(ctx, a.xray, prevUsers, users)
	if err != nil {
		return "", synced, removed, err
	}
	_ = a.state.Update(func(s *State) { s.SyncedUsers = users })

	appliedVersion = hashUsers(users)
	return appliedVersion, synced, removed, nil
}

func applyUsersSync(ctx context.Context, xray userSyncApplier, prevUsers []UserSyncItem, currentUsers []UserSyncItem) (synced int, removed int, err error) {
	failures := make([]string, 0, 4)
	recordFailure := func(action string, email string, err error) {
		log.Printf("%s user %s failed: %v", action, email, err)
		failures = append(failures, fmt.Sprintf("%s %s: %v", action, email, err))
	}

	for _, stale := range staleSyncedUsers(prevUsers, currentUsers) {
		email := strings.TrimSpace(stale.Email)
		if email == "" {
			continue
		}
		if err := xray.RemoveUser(ctx, email); err != nil {
			recordFailure("remove stale", email, err)
			continue
		}
		removed++
	}

	for _, u := range currentUsers {
		email := strings.TrimSpace(u.Email)
		if email == "" {
			continue
		}
		if u.Status == "active" && strings.TrimSpace(u.UUID) != "" {
			if err := xray.UpsertUser(ctx, email, u.UUID); err != nil {
				recordFailure("upsert", email, err)
				continue
			}
			synced++
			continue
		}
		if err := xray.RemoveUser(ctx, email); err != nil {
			recordFailure("remove", email, err)
			continue
		}
		removed++
	}

	if len(failures) > 0 {
		details := failures
		if len(details) > 3 {
			details = details[:3]
		}
		msg := fmt.Sprintf("users sync incomplete: %d operation(s) failed", len(failures))
		msg = msg + " (" + strings.Join(details, "; ") + ")"
		if len(failures) > len(details) {
			msg = msg + fmt.Sprintf("; +%d more", len(failures)-len(details))
		}
		return synced, removed, JobError{
			Retryable: true,
			Msg:       msg,
			ResultJSON: map[string]any{
				"synced":   synced,
				"removed":  removed,
				"failed":   len(failures),
				"failures": failures,
			},
		}
	}
	return synced, removed, nil
}

func restoreActiveUsers(ctx context.Context, xray userSyncApplier, users []UserSyncItem) (synced int, err error) {
	failures := make([]string, 0, 4)
	for _, u := range users {
		email := strings.TrimSpace(u.Email)
		uuid := strings.TrimSpace(u.UUID)
		if email == "" || uuid == "" || u.Status != "active" {
			continue
		}
		if err := xray.UpsertUser(ctx, email, uuid); err != nil {
			log.Printf("restore user %s failed: %v", email, err)
			failures = append(failures, fmt.Sprintf("restore %s: %v", email, err))
			continue
		}
		synced++
	}
	if len(failures) > 0 {
		details := failures
		if len(details) > 3 {
			details = details[:3]
		}
		msg := fmt.Sprintf("restore users incomplete: %d operation(s) failed", len(failures))
		msg += " (" + strings.Join(details, "; ") + ")"
		if len(failures) > len(details) {
			msg += fmt.Sprintf("; +%d more", len(failures)-len(details))
		}
		return synced, JobError{Retryable: true, Msg: msg, ResultJSON: map[string]any{"synced": synced, "failed": len(failures), "failures": failures}}
	}
	return synced, nil
}

func staleSyncedUsers(prevUsers []UserSyncItem, currentUsers []UserSyncItem) []UserSyncItem {
	currentByEmail := make(map[string]struct{}, len(currentUsers))
	for _, u := range currentUsers {
		email := strings.TrimSpace(u.Email)
		if email == "" {
			continue
		}
		currentByEmail[email] = struct{}{}
	}
	stale := make([]UserSyncItem, 0, len(prevUsers))
	seen := make(map[string]struct{}, len(prevUsers))
	for _, u := range prevUsers {
		email := strings.TrimSpace(u.Email)
		if email == "" {
			continue
		}
		if _, ok := currentByEmail[email]; ok {
			continue
		}
		if _, ok := seen[email]; ok {
			continue
		}
		seen[email] = struct{}{}
		stale = append(stale, u)
	}
	return stale
}

func hashUsers(users []UserSyncItem) string {
	type item struct {
		UserID int64  `json:"user_id"`
		Email  string `json:"email"`
		Status string `json:"status"`
		UUID   string `json:"uuid,omitempty"`
	}
	items := make([]item, 0, len(users))
	for _, u := range users {
		items = append(items, item{UserID: u.UserID, Email: u.Email, Status: u.Status, UUID: u.UUID})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UserID < items[j].UserID })
	b, _ := json.Marshal(items)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (a *Agent) execXrayApply(ctx context.Context, req XrayApplyRequest) (map[string]any, error) {
	if strings.TrimSpace(a.xrayConfigPath) == "" {
		return nil, JobError{Retryable: false, Msg: "XRAY_CONFIG_PATH is required"}
	}
	if len(a.xrayReloadArgs) == 0 {
		return nil, JobError{Retryable: false, Msg: "XRAY_RELOAD_ARGS_JSON is required for xray_apply"}
	}

	rendered := os.Expand(req.Template, func(k string) string {
		// 1) Job vars (panel) always win.
		if req.Vars != nil {
			if v, ok := req.Vars[k]; ok {
				return v
			}
		}
		// 2) Environment vars (node local) if not placeholders.
		if v := os.Getenv(k); !isPlaceholder(v) {
			return v
		}
		// 3) Agent-local fallbacks (e.g. REALITY private key + short_id, fixed port defaults).
		if v := a.lookupLocalTemplateVar(k); v != "" {
			return v
		}
		// 4) Last resort: return env (even if placeholder) so failures are explicit.
		return os.Getenv(k)
	})
	renderedBytes := []byte(rendered)
	if !json.Valid(renderedBytes) {
		return nil, JobError{Retryable: false, Msg: "rendered config is not valid json"}
	}

	backupPath := ""
	if old, err := os.ReadFile(a.xrayConfigPath); err == nil {
		backupPath = fmt.Sprintf("%s.bak.%s", a.xrayConfigPath, time.Now().UTC().Format("20060102T150405Z"))
		_ = os.WriteFile(backupPath, old, 0600)
	}

	dir := filepath.Dir(a.xrayConfigPath)
	base := filepath.Base(a.xrayConfigPath)
	tmpPath := filepath.Join(dir, fmt.Sprintf(".%s.tmp.%d", base, time.Now().UTC().UnixNano()))
	if err := os.WriteFile(tmpPath, renderedBytes, 0600); err != nil {
		return nil, JobError{Retryable: true, Msg: "write temp config failed: " + err.Error()}
	}
	defer func() { _ = os.Remove(tmpPath) }()

	if len(a.xrayTestArgs) > 0 {
		if err := runArgs(ctx, expandArgs(a.xrayTestArgs, tmpPath, a.xrayConfigPath), tmpPath, a.xrayConfigPath); err != nil {
			return nil, JobError{Retryable: false, Msg: "xray test failed: " + err.Error()}
		}
	}

	if err := os.Rename(tmpPath, a.xrayConfigPath); err != nil {
		return nil, JobError{Retryable: true, Msg: "install config failed: " + err.Error()}
	}

	if err := runArgs(ctx, expandArgs(a.xrayReloadArgs, a.xrayConfigPath, a.xrayConfigPath), a.xrayConfigPath, a.xrayConfigPath); err != nil {
		rolledBack := false
		rollbackErr := ""
		if req.RollbackOnFail && backupPath != "" {
			if rbErr := restoreBackupFile(a.xrayConfigPath, backupPath); rbErr != nil {
				rollbackErr = rbErr.Error()
			} else if rbReloadErr := runArgs(ctx, expandArgs(a.xrayReloadArgs, a.xrayConfigPath, a.xrayConfigPath), a.xrayConfigPath, a.xrayConfigPath); rbReloadErr != nil {
				rollbackErr = rbReloadErr.Error()
			} else {
				rolledBack = true
			}
		}
		msg := "xray reload failed: " + err.Error()
		if rolledBack {
			msg = msg + " (rolled back to previous config)"
		} else if rollbackErr != "" {
			msg = msg + " (rollback failed: " + rollbackErr + ")"
		}
		return nil, JobError{Retryable: true, Msg: msg}
	}

	out := map[string]any{"ok": true, "config_path": a.xrayConfigPath}
	if strings.TrimSpace(backupPath) != "" {
		out["backup_name"] = filepath.Base(backupPath)
	}
	return out, nil
}

func (a *Agent) execXrayRollback(ctx context.Context, req XrayRollbackRequest) (map[string]any, error) {
	if strings.TrimSpace(a.xrayConfigPath) == "" {
		return nil, JobError{Retryable: false, Msg: "XRAY_CONFIG_PATH is required"}
	}
	if len(a.xrayReloadArgs) == 0 {
		return nil, JobError{Retryable: false, Msg: "XRAY_RELOAD_ARGS_JSON is required for xray_rollback"}
	}
	backupPath, err := resolveBackupPath(a.xrayConfigPath, strings.TrimSpace(req.BackupName))
	if err != nil {
		return nil, JobError{Retryable: false, Msg: "invalid backup_name: " + err.Error()}
	}
	if backupPath == "" {
		return nil, JobError{Retryable: false, Msg: "no backup found"}
	}
	if err := restoreBackupFile(a.xrayConfigPath, backupPath); err != nil {
		return nil, JobError{Retryable: true, Msg: "restore failed: " + err.Error()}
	}
	if err := runArgs(ctx, expandArgs(a.xrayReloadArgs, a.xrayConfigPath, a.xrayConfigPath), a.xrayConfigPath, a.xrayConfigPath); err != nil {
		return nil, JobError{Retryable: true, Msg: "reload failed: " + err.Error()}
	}
	return map[string]any{"ok": true, "config_path": a.xrayConfigPath, "backup_name": filepath.Base(backupPath)}, nil
}

func parseArgsJSON(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	trimmed := make([]string, 0, len(out))
	for _, a := range out {
		v := strings.TrimSpace(a)
		if v != "" {
			trimmed = append(trimmed, v)
		}
	}
	return trimmed, nil
}

func expandArgs(args []string, neutrinoConfigPath string, xrayConfigPath string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		out = append(out, os.Expand(a, func(k string) string {
			switch k {
			case "NEUTRINO_CONFIG_PATH":
				return neutrinoConfigPath
			case "XRAY_CONFIG_PATH":
				return xrayConfigPath
			default:
				return os.Getenv(k)
			}
		}))
	}
	return out
}

func runArgs(ctx context.Context, args []string, neutrinoConfigPath string, xrayConfigPath string) error {
	if len(args) == 0 {
		return nil
	}
	c := exec.CommandContext(ctx, args[0], args[1:]...)
	c.Env = append(os.Environ(),
		"NEUTRINO_CONFIG_PATH="+neutrinoConfigPath,
		"XRAY_CONFIG_PATH="+xrayConfigPath,
	)
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = &out
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(out.String())
		if msg == "" {
			return err
		}
		return fmt.Errorf("%v: %s", err, msg)
	}
	return nil
}

func resolveBackupPath(configPath string, backupName string) (string, error) {
	configPath = filepath.Clean(strings.TrimSpace(configPath))
	if configPath == "" {
		return "", fmt.Errorf("missing config_path")
	}
	dir := filepath.Dir(configPath)
	base := filepath.Base(configPath)
	prefix := base + ".bak."

	if backupName != "" {
		// Basename only; reject any path separators.
		if strings.Contains(backupName, "/") || strings.Contains(backupName, "\\") || filepath.Base(backupName) != backupName {
			return "", fmt.Errorf("backup_name must be a basename")
		}
		if !strings.HasPrefix(backupName, prefix) {
			return "", fmt.Errorf("backup_name must start with %s", prefix)
		}
		return filepath.Join(dir, backupName), nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	cands := make([]string, 0, 8)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, prefix) {
			cands = append(cands, name)
		}
	}
	sort.Strings(cands)
	if len(cands) == 0 {
		return "", nil
	}
	// Latest lexical works because suffix is yyyymmddThhmmssZ.
	return filepath.Join(dir, cands[len(cands)-1]), nil
}

func restoreBackupFile(configPath, backupPath string) error {
	configPath = filepath.Clean(strings.TrimSpace(configPath))
	backupPath = filepath.Clean(strings.TrimSpace(backupPath))
	if configPath == "" || backupPath == "" {
		return fmt.Errorf("missing config/backup path")
	}
	if filepath.Dir(configPath) != filepath.Dir(backupPath) {
		return fmt.Errorf("backup must be in the same directory as config")
	}
	cfgBase := filepath.Base(configPath)
	bakBase := filepath.Base(backupPath)
	if !strings.HasPrefix(bakBase, cfgBase+".bak.") {
		return fmt.Errorf("backup filename must start with %s.bak.", cfgBase)
	}
	b, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, b, 0600)
}

func (a *Agent) startHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	sendOnce := func() {
		pc := a.panelClient()
		if pc == nil {
			return
		}
		reportedAt := time.Now().UTC()
		rp := a.buildNodeReportPayload()
		if rp == nil {
			rp = &NodeReportPayload{}
		}
		rp.ReportedAt = reportedAt.Format(time.RFC3339)
		rp.Metrics = a.heartbeatMetrics(reportedAt)
		pc.SetReportPayload(*rp)
		if err := pc.Heartbeat(ctx); err != nil {
			log.Printf("heartbeat failed: %v", err)
		}
	}
	sendOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendOnce()
		}
	}
}

func (a *Agent) heartbeatMetrics(reportedAt time.Time) *NodeReportMetrics {
	if a == nil {
		return nil
	}
	if reportedAt.IsZero() {
		reportedAt = time.Now().UTC()
	}
	m := &NodeReportMetrics{
		Goroutines: runtime.NumGoroutine(),
	}

	// Best-effort host/container metrics for node monitoring.
	if cpuList, err := cpu.PercentWithContext(context.Background(), 0, false); err == nil && len(cpuList) > 0 {
		v := cpuList[0]
		if v < 0 {
			v = 0
		}
		if v > 1000 {
			v = 1000
		}
		m.CPUPercent = v
	}
	if vm, err := mem.VirtualMemoryWithContext(context.Background()); err == nil && vm != nil {
		// Match panel host monitor: report used bytes.
		m.MemoryBytes = hostnet.ClampInt64FromUint64(vm.Used)
	}
	if inTotal, outTotal, netSource, err := a.readNetTotals(context.Background()); err == nil {
		if a.hasLastNet && !a.lastNetAt.IsZero() {
			dt := reportedAt.Sub(a.lastNetAt).Seconds()
			if dt > 0 {
				inDelta := inTotal - a.lastNetInTotal
				outDelta := outTotal - a.lastNetOutTotal
				if inDelta >= 0 {
					m.InboundBPS = float64(inDelta) / dt
				}
				if outDelta >= 0 {
					m.OutboundBPS = float64(outDelta) / dt
				}
			}
		}
		a.lastNetInTotal, a.lastNetOutTotal, a.lastNetAt, a.hasLastNet = inTotal, outTotal, reportedAt, true
		monthLoc := a.cfg.MonthLocation()
		m.MonthKey = localMonthKey(reportedAt, monthLoc)
		m.MonthTimezone = localTimezoneName(reportedAt, monthLoc)
		m.NetRXTotal = inTotal
		m.NetTXTotal = outTotal
		m.NetSource = netSource
	}
	if du, err := disk.UsageWithContext(context.Background(), a.diskUsagePath()); err == nil && du != nil {
		m.DiskTotalBytes = hostnet.ClampInt64FromUint64(du.Total)
		m.DiskUsedBytes = hostnet.ClampInt64FromUint64(du.Used)
		m.DiskFreeBytes = hostnet.ClampInt64FromUint64(du.Free)
		if du.UsedPercent >= 0 {
			m.DiskUsedPercent = du.UsedPercent
		}
	}

	if !a.startedAt.IsZero() {
		up := time.Since(a.startedAt).Seconds()
		if up > 0 {
			m.UptimeSec = int64(up)
		}
	}
	if a.queue != nil {
		m.QueueBytes = a.queue.ApproxBytes()
		m.QueueBatches = a.queue.ApproxBatches()
	}
	return m
}

func (a *Agent) diskUsagePath() string {
	if a == nil {
		return "/"
	}
	p := strings.TrimSpace(a.cfg.StatePath)
	if p != "" && filepath.IsAbs(p) {
		dir := filepath.Clean(filepath.Dir(p))
		if dir != "" {
			return dir
		}
	}
	return "/"
}

// Usage pipeline: durable queue + flush-first.
func (a *Agent) startUsageLoop(ctx context.Context) {
	statsInterval := time.Duration(a.cfg.StatsPollSec) * time.Second
	if statsInterval < time.Second {
		statsInterval = 5 * time.Second
	}
	accessInterval := time.Duration(a.cfg.AccessPollSec) * time.Second
	if accessInterval < time.Second {
		accessInterval = 2 * time.Second
	}
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	lastStatsAt := time.Time{}
	lastAccessAt := time.Time{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 1) Flush queued batches first (try multiple items per tick to catch up faster).
			flushed := 0
			for flushed < 8 {
				path, batch, ok, err := a.queue.PeekOldest()
				if err != nil {
					log.Printf("queue peek failed: %v", err)
					break
				}
				if !ok {
					break
				}
				pc := a.panelClient()
				if pc == nil {
					break
				}
				if err := pc.PushUsage(ctx, batch.Events); err != nil {
					log.Printf("push queued usage failed: %v", err)
					break
				}
				_ = a.state.Update(func(s *State) {
					switch batch.Kind {
					case "stats":
						if batch.Stats != nil {
							if s.AckedStats == nil {
								s.AckedStats = make(map[string]StatEntry)
							}
							for k, v := range batch.Stats.NextAck {
								s.AckedStats[k] = v
							}
							s.StatsEpoch = batch.Stats.Epoch
						}
					case "access":
						if batch.Access != nil {
							s.Access.Path = batch.Access.Path
							s.Access.Inode = batch.Access.Inode
							s.Access.Offset = batch.Access.Offset
						}
					}
				})
				if err := a.queue.Dequeue(path); err != nil {
					log.Printf("queue dequeue failed: %v", err)
					break
				}
				flushed++
			}
			if flushed > 0 {
				continue
			}
			if !shouldGenerateUsageBatches(flushed, a.queue) {
				continue
			}

			// 2) Generate due batches only when queue is empty.
			// We may enqueue both access and stats in the same tick; this prevents
			// access batches from starving stats on busy nodes while still keeping
			// the stronger invariant that we never sample fresh usage while any
			// older batch remains pending in the queue.
			now := time.Now()
			var enqueued int
			lastAccessAt, lastStatsAt, enqueued = enqueueDueUsageBatches(
				now,
				lastAccessAt,
				lastStatsAt,
				accessInterval,
				statsInterval,
				func() (UsageBatch, bool) { return a.buildAccessBatch(ctx) },
				func() (UsageBatch, bool) { return a.buildStatsBatch(ctx) },
				a.queue.Enqueue,
			)
			if enqueued > 0 {
				continue
			}
		}
	}
}

func shouldGenerateUsageBatches(flushed int, q interface{ ApproxBatches() int64 }) bool {
	if flushed > 0 {
		return false
	}
	return !queueHasPending(q)
}

func queueHasPending(q interface{ ApproxBatches() int64 }) bool {
	if q == nil {
		return false
	}
	return q.ApproxBatches() > 0
}

type usageBatchBuilder func() (UsageBatch, bool)
type usageBatchEnqueuer func(UsageBatch) error

func enqueueDueUsageBatches(
	now time.Time,
	lastAccessAt time.Time,
	lastStatsAt time.Time,
	accessInterval time.Duration,
	statsInterval time.Duration,
	buildAccess usageBatchBuilder,
	buildStats usageBatchBuilder,
	enqueue usageBatchEnqueuer,
) (time.Time, time.Time, int) {
	enqueued := 0

	if usagePollDue(lastAccessAt, now, accessInterval) {
		if batch, ok := buildAccess(); ok {
			if err := enqueue(batch); err != nil {
				log.Printf("enqueue access batch failed: %v", err)
			} else {
				lastAccessAt = now
				enqueued++
			}
		} else {
			lastAccessAt = now
		}
	}

	if usagePollDue(lastStatsAt, now, statsInterval) {
		if batch, ok := buildStats(); ok {
			if err := enqueue(batch); err != nil {
				log.Printf("enqueue stats batch failed: %v", err)
			} else {
				lastStatsAt = now
				enqueued++
			}
		} else {
			lastStatsAt = now
		}
	}

	return lastAccessAt, lastStatsAt, enqueued
}

func usagePollDue(last time.Time, now time.Time, interval time.Duration) bool {
	return last.IsZero() || now.Sub(last) >= interval
}

type statsCounterSample struct {
	UserID  int64
	Email   string
	Acked   StatEntry
	Current StatEntry
}

func planStatsBatch(nodeID int64, samples []statsCounterSample, baseEpoch int64, maxEvents int, now time.Time) UsageBatch {
	epochTarget := baseEpoch
	for _, sample := range samples {
		if sample.Current.Uplink < sample.Acked.Uplink || sample.Current.Downlink < sample.Acked.Downlink {
			epochTarget++
			break
		}
	}

	events := make([]UsageEvent, 0, len(samples)*2)
	nextAck := make(map[string]StatEntry, len(samples))

	for _, sample := range samples {
		if sample.UserID <= 0 || strings.TrimSpace(sample.Email) == "" {
			continue
		}

		ackedToCommit := sample.Acked
		upDelta, upRebase := deltaValue(sample.Acked.Uplink, sample.Current.Uplink)
		downDelta, downRebase := deltaValue(sample.Acked.Downlink, sample.Current.Downlink)

		if upDelta == 0 {
			ackedToCommit.Uplink = upRebase
		}
		if downDelta == 0 {
			ackedToCommit.Downlink = downRebase
		}

		if upDelta > 0 && statsBatchHasCapacity(len(events), maxEvents) {
			nodeIDCopy := nodeID
			events = append(events, UsageEvent{
				UserID:        sample.UserID,
				NodeID:        &nodeIDCopy,
				Direction:     "inbound",
				Bytes:         upDelta,
				Source:        "xray-stats",
				SourceEventID: fmt.Sprintf("xray-stats:%d:%s:e%d:uplink:%d", nodeID, sample.Email, epochTarget, sample.Current.Uplink),
				At:            now.Format(time.RFC3339),
			})
			ackedToCommit.Uplink = sample.Current.Uplink
		}
		if downDelta > 0 && statsBatchHasCapacity(len(events), maxEvents) {
			nodeIDCopy := nodeID
			events = append(events, UsageEvent{
				UserID:        sample.UserID,
				NodeID:        &nodeIDCopy,
				Direction:     "outbound",
				Bytes:         downDelta,
				Source:        "xray-stats",
				SourceEventID: fmt.Sprintf("xray-stats:%d:%s:e%d:downlink:%d", nodeID, sample.Email, epochTarget, sample.Current.Downlink),
				At:            now.Format(time.RFC3339),
			})
			ackedToCommit.Downlink = sample.Current.Downlink
		}

		if ackedToCommit != sample.Acked {
			nextAck[sample.Email] = ackedToCommit
		}
	}

	return UsageBatch{
		Kind:      "stats",
		CreatedAt: now.Format(time.RFC3339),
		Events:    events,
		Stats:     &StatsMeta{NextAck: nextAck, Epoch: epochTarget},
	}
}

func statsBatchHasCapacity(currentEvents int, maxEvents int) bool {
	return maxEvents <= 0 || currentEvents < maxEvents
}

func (a *Agent) buildStatsBatch(ctx context.Context) (UsageBatch, bool) {
	snap := a.state.Snapshot()
	users := snap.SyncedUsers
	if len(users) == 0 {
		return UsageBatch{}, false
	}

	samples := make([]statsCounterSample, 0, len(users))

	for _, u := range users {
		if strings.TrimSpace(u.Email) == "" || u.Status != "active" {
			continue
		}
		if u.UserID <= 0 {
			continue
		}
		uplinkCurr, downlinkCurr, err := a.xray.PullUserTraffic(ctx, u.Email)
		if err != nil {
			continue
		}
		samples = append(samples, statsCounterSample{
			UserID:  u.UserID,
			Email:   u.Email,
			Acked:   snap.AckedStats[u.Email],
			Current: StatEntry{Uplink: uplinkCurr, Downlink: downlinkCurr},
		})
	}

	batch := planStatsBatch(a.cfg.NodeID, samples, snap.StatsEpoch, a.cfg.PushBatchMaxEvents, time.Now().UTC())
	if len(batch.Events) == 0 {
		if batch.Stats != nil && (len(batch.Stats.NextAck) > 0 || batch.Stats.Epoch != snap.StatsEpoch) {
			// Persist epoch/counter-only acknowledgements (for example, xray counter
			// resets with zero delta) even when there is nothing to send to panel.
			_ = a.state.Update(func(s *State) {
				if s.AckedStats == nil {
					s.AckedStats = make(map[string]StatEntry)
				}
				for k, v := range batch.Stats.NextAck {
					s.AckedStats[k] = v
				}
				s.StatsEpoch = batch.Stats.Epoch
			})
		}
		return UsageBatch{}, false
	}

	return batch, true
}

func deltaValue(last, current int64) (delta, rebaseTo int64) {
	if current < 0 {
		return 0, 0
	}
	if current >= last {
		return current - last, current
	}
	return current, current
}

func (a *Agent) buildAccessBatch(ctx context.Context) (UsageBatch, bool) {
	path := strings.TrimSpace(a.cfg.XrayAccessLogPath)
	if path == "" {
		return UsageBatch{}, false
	}

	snap := a.state.Snapshot()
	users := snap.SyncedUsers
	if len(users) == 0 {
		return UsageBatch{}, false
	}
	userIDByEmail := make(map[string]int64, len(users))
	for _, u := range users {
		if u.UserID > 0 && strings.TrimSpace(u.Email) != "" {
			userIDByEmail[u.Email] = u.UserID
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		return UsageBatch{}, false
	}
	inode := fileInode(info)

	access := snap.Access
	if strings.TrimSpace(access.Path) == "" {
		access.Path = path
	}
	if access.Path != path {
		access.Path = path
		access.Inode = 0
		access.Offset = 0
	}
	if access.Inode != 0 && inode != 0 && inode != access.Inode {
		access.Offset = 0
	}
	if info.Size() < access.Offset {
		access.Offset = 0
	}

	f, err := os.Open(path)
	if err != nil {
		return UsageBatch{}, false
	}
	defer f.Close()
	if _, err := f.Seek(access.Offset, 0); err != nil {
		return UsageBatch{}, false
	}

	maxLines := 300
	if a.cfg.PushBatchMaxEvents > 0 && a.cfg.PushBatchMaxEvents < maxLines {
		maxLines = a.cfg.PushBatchMaxEvents
	}

	reader := bufio.NewReader(f)
	cur := access.Offset
	events := make([]UsageEvent, 0, 32)
	onlineEvents := make(map[string]UsageEvent)
	newOffset := cur

	for len(events) < maxLines {
		line, readErr := reader.ReadString('\n')
		if len(line) == 0 {
			if readErr != nil {
				break
			}
			continue
		}
		start := cur
		cur += int64(len(line))
		newOffset = cur

		evt, ok := parseAccessLine(path, inode, start, strings.TrimSpace(line), userIDByEmail, a.cfg.NodeID, a.cfg.AccessLogLocation())
		if ok {
			// Zero-byte sampling/limit.
			if a.sampler.Allow(evt.UserID, evt.Destination, time.Now()) {
				events = append(events, evt)
			} else if onlineEvt, onlineKey, ok := compactAccessOnlineEvent(evt, a.cfg.NodeID); ok {
				onlineEvents[onlineKey] = onlineEvt
			}
		}
		if readErr != nil {
			break
		}
	}
	if len(onlineEvents) > 0 {
		keys := make([]string, 0, len(onlineEvents))
		for k := range onlineEvents {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			events = append(events, onlineEvents[k])
		}
	}

	if len(events) == 0 {
		// Advance the cursor even when this read window only contained
		// unmatched, inactive-user, or sampled-out lines.
		if newOffset != access.Offset || access.Inode == 0 || access.Path != path {
			_ = a.state.Update(func(s *State) {
				s.Access.Path = path
				s.Access.Inode = inode
				s.Access.Offset = newOffset
			})
		}
		return UsageBatch{}, false
	}

	return UsageBatch{
		Kind:      "access",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Events:    events,
		Access:    &AccessMeta{Path: path, Inode: inode, Offset: newOffset},
	}, true
}

func fileInode(info os.FileInfo) uint64 {
	if info == nil {
		return 0
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st != nil {
		return uint64(st.Ino)
	}
	return 0
}

var (
	reAccessEmail = regexp.MustCompile(`\bemail:\s*([^\s]+)`)
	reAccessDest  = regexp.MustCompile(`\b(tcp|udp):(\[[^\]]+\]|[^\s:]+):([0-9]{1,5})`)
	reAccessRoute = regexp.MustCompile(`\[([^\s\]]+)\s*>>\s*([^\s\]]+)\]`)
	reAccessSNI   = regexp.MustCompile(`\bsni:\s*([^\s]+)`)
	reAccessFrom  = regexp.MustCompile(`\bfrom\s+(?:\[([^\]]+)\]|([^\s:]+)):[0-9]+`)
)

func extractEventTime(line string, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	if len(line) >= 19 {
		if t, err := time.ParseInLocation("2006/01/02 15:04:05", line[:19], loc); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC()
}

func parseAccessLine(path string, inode uint64, startOffset int64, line string, userIDByEmail map[string]int64, nodeID int64, loc *time.Location) (UsageEvent, bool) {
	if strings.TrimSpace(line) == "" {
		return UsageEvent{}, false
	}
	emailMatch := reAccessEmail.FindStringSubmatch(line)
	if len(emailMatch) < 2 {
		return UsageEvent{}, false
	}
	email := strings.TrimSpace(emailMatch[1])
	userID := userIDByEmail[email]
	if userID <= 0 {
		return UsageEvent{}, false
	}
	destMatch := reAccessDest.FindStringSubmatch(line)
	if len(destMatch) < 4 {
		return UsageEvent{}, false
	}

	proto := destMatch[1]
	host := strings.Trim(destMatch[2], "[]")
	port, _ := strconv.ParseInt(destMatch[3], 10, 64)

	targetHost := ""
	targetIP := ""
	if ip := net.ParseIP(host); ip == nil {
		targetHost = host
	} else {
		targetIP = host
	}

	inboundTag := ""
	if routeMatch := reAccessRoute.FindStringSubmatch(line); len(routeMatch) >= 2 {
		inboundTag = strings.TrimSpace(routeMatch[1])
	}

	clientIP := ""
	if fromMatch := reAccessFrom.FindStringSubmatch(line); len(fromMatch) >= 2 {
		switch {
		case strings.TrimSpace(fromMatch[1]) != "":
			clientIP = strings.TrimSpace(fromMatch[1])
		case len(fromMatch) >= 3:
			clientIP = strings.TrimSpace(fromMatch[2])
		}
	}

	sni := ""
	if sniMatch := reAccessSNI.FindStringSubmatch(line); len(sniMatch) >= 2 {
		sni = strings.TrimSpace(sniMatch[1])
	}
	if sni == "" && proto == "tcp" && targetHost != "" {
		sni = targetHost
	}

	eventAt := extractEventTime(line, loc)

	sum := sha1.Sum([]byte(fmt.Sprintf("%s|%d|%d|%d|%s", path, inode, startOffset, len(line), line)))
	eventID := hex.EncodeToString(sum[:])
	nid := nodeID

	return UsageEvent{
		UserID:    userID,
		NodeID:    &nid,
		Direction: "outbound",
		Bytes:     0,
		Source:    "xray-access",
		// Include node_id to avoid cross-node id collisions in panel idempotency keying.
		SourceEventID: fmt.Sprintf("xray-access:%d:%s", nodeID, eventID),
		TargetHost:    targetHost,
		TargetIP:      targetIP,
		TargetPort:    port,
		SNI:           sni,
		Destination:   proto + ":" + host + ":" + destMatch[3],
		ClientIP:      clientIP,
		InboundTag:    inboundTag,
		At:            eventAt.UTC().Format(time.RFC3339),
	}, true
}

func compactAccessOnlineEvent(evt UsageEvent, nodeID int64) (UsageEvent, string, bool) {
	clientIP := strings.TrimSpace(evt.ClientIP)
	if evt.UserID <= 0 || clientIP == "" {
		return UsageEvent{}, "", false
	}
	eventAt, err := time.Parse(time.RFC3339, evt.At)
	if err != nil {
		eventAt = time.Now().UTC()
	}
	minute := eventAt.UTC().Truncate(time.Minute)
	key := fmt.Sprintf("%d|%s|%s", evt.UserID, clientIP, minute.Format("200601021504"))
	sum := sha1.Sum([]byte(fmt.Sprintf("%d|%d|%s|%s", nodeID, evt.UserID, clientIP, minute.Format(time.RFC3339))))
	nid := nodeID
	return UsageEvent{
		UserID:        evt.UserID,
		NodeID:        &nid,
		Direction:     "outbound",
		Bytes:         0,
		Source:        "xray-access",
		SourceEventID: fmt.Sprintf("xray-access-online:%d:%s", nodeID, hex.EncodeToString(sum[:])),
		TargetHost:    evt.TargetHost,
		TargetIP:      evt.TargetIP,
		TargetPort:    evt.TargetPort,
		SNI:           evt.SNI,
		Destination:   evt.Destination,
		ClientIP:      clientIP,
		InboundTag:    evt.InboundTag,
		At:            minute.Format(time.RFC3339),
	}, key, true
}
