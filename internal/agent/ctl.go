// ctl.go is the exported surface for cmd/neutrinoctl: read-only inspection of
// agent-local state plus the existing fixed apply/rollback/test logic. Nothing
// here accepts commands or file paths from outside the agent's own config —
// the CLI is a window onto local state, not a shell proxy.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CtlStatus is the agent-local status snapshot for `neutrinoctl status`.
type CtlStatus struct {
	NodeID             int64   `json:"node_id"`
	StatePath          string  `json:"state_path"`
	StateOK            bool    `json:"state_ok"`
	StateError         string  `json:"state_error,omitempty"`
	SyncedUsers        int     `json:"synced_users"`
	SyncedUsersVersion string  `json:"synced_users_version,omitempty"`
	PendingUsersSync   string  `json:"pending_users_sync,omitempty"` // mode, or "" when none
	XrayReloadPending  bool    `json:"xray_reload_pending"`
	StatsEpoch         int64   `json:"stats_epoch"`
	AckedStatCounters  int     `json:"acked_stat_counters"`
	XrayConfigPath     string  `json:"xray_config_path"`
	XrayConfigExists   bool    `json:"xray_config_exists"`
	XrayConfigValid    bool    `json:"xray_config_valid"`
	RealityPath        string  `json:"reality_path"`
	RealityOK          bool    `json:"reality_ok"`
	Queue              CtlQueue `json:"queue"`
	Cert               CtlCert  `json:"cert"`
}

// CtlQueue summarizes the usage disk queue for `neutrinoctl queue`.
type CtlQueue struct {
	Dir                string   `json:"dir"`
	Batches            int64    `json:"batches"`
	Bytes              int64    `json:"bytes"`
	QuarantinedBatches int64    `json:"quarantined_batches"`
	OldestBatch        string   `json:"oldest_batch,omitempty"`
	Files              []string `json:"files,omitempty"`
}

// CtlCert summarizes the mTLS client certificate for `neutrinoctl cert`.
type CtlCert struct {
	Path        string `json:"path"`
	Present     bool   `json:"present"`
	Error       string `json:"error,omitempty"`
	CommonName  string `json:"common_name,omitempty"`
	NotAfter    string `json:"not_after,omitempty"`
	DaysLeft    int    `json:"days_left"`
	RenewDue    bool   `json:"renew_due"`
	RenewBefore int    `json:"renew_before_days"`
}

// CtlEnrollInfo is the connectivity/identity view for `neutrinoctl enroll-info`.
type CtlEnrollInfo struct {
	NodeID         int64  `json:"node_id"`
	PanelURL       string `json:"panel_url"`
	PanelMTLSURL   string `json:"panel_mtls_url"`
	CACertPath     string `json:"ca_cert_path"`
	CACertPresent  bool   `json:"ca_cert_present"`
	ClientCertPath string `json:"client_cert_path"`
	ClientKeyPath  string `json:"client_key_path"`
	ClientKeyOK    bool   `json:"client_key_present"`
	Cert           CtlCert `json:"cert"`
	EnrollCodeSet  bool   `json:"enroll_code_set"`
}

// CollectCtlStatus assembles the status snapshot from local files only.
func CollectCtlStatus(cfg Config) CtlStatus {
	out := CtlStatus{
		NodeID:         cfg.NodeID,
		StatePath:      cfg.StatePath,
		XrayConfigPath: cfg.XrayConfigPath,
		RealityPath:    realityPathFromConfig(cfg),
		Queue:          CollectCtlQueue(cfg, false),
		Cert:           CollectCtlCert(cfg),
	}

	st := NewStateStore(cfg.StatePath)
	if err := st.Load(); err != nil {
		out.StateError = err.Error()
	} else {
		s := st.Snapshot()
		out.StateOK = true
		out.SyncedUsers = len(s.SyncedUsers)
		out.SyncedUsersVersion = s.SyncedUsersVersion
		if s.PendingUsersSync != nil {
			out.PendingUsersSync = s.PendingUsersSync.Mode
		}
		out.XrayReloadPending = s.XrayReloadPending
		out.StatsEpoch = s.StatsEpoch
		out.AckedStatCounters = len(s.AckedStats)
	}

	if b, err := os.ReadFile(cfg.XrayConfigPath); err == nil {
		out.XrayConfigExists = true
		out.XrayConfigValid = json.Valid(b)
	}
	if _, ok, err := loadRealityConfig(out.RealityPath); err == nil && ok {
		out.RealityOK = true
	}
	return out
}

// CollectCtlQueue summarizes the disk queue. listFiles also includes the
// individual batch filenames (oldest first).
func CollectCtlQueue(cfg Config, listFiles bool) CtlQueue {
	dir := strings.TrimSpace(cfg.QueueDir)
	out := CtlQueue{Dir: dir}
	if dir == "" {
		return out
	}
	q := NewDiskQueue(dir, cfg.QueueMaxBytes)
	out.Bytes = q.ApproxBytes()
	out.QuarantinedBatches = q.QuarantinedBatches()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	out.Batches = int64(len(names))
	if len(names) > 0 {
		out.OldestBatch = names[0]
	}
	if listFiles {
		out.Files = names
	}
	return out
}

// CollectCtlCert reads the mTLS client cert and applies the same renew-window
// logic the cert renewer uses.
func CollectCtlCert(cfg Config) CtlCert {
	renewBefore := cfg.CertRenewBeforeDays
	if renewBefore <= 0 {
		renewBefore = 7
	}
	out := CtlCert{Path: cfg.PanelMTLSClientCertPath, RenewBefore: renewBefore}
	notAfter, cn, err := loadClientCertNotAfter(cfg.PanelMTLSClientCertPath)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	now := time.Now().UTC()
	out.Present = true
	out.CommonName = cn
	out.NotAfter = notAfter.Format(time.RFC3339)
	out.DaysLeft = int(time.Until(notAfter).Hours() / 24)
	out.RenewDue = shouldRenew(now, notAfter, renewBefore)
	return out
}

// CollectCtlEnrollInfo reports identity/connectivity material presence.
func CollectCtlEnrollInfo(cfg Config) CtlEnrollInfo {
	out := CtlEnrollInfo{
		NodeID:         cfg.NodeID,
		PanelURL:       cfg.PanelURL,
		PanelMTLSURL:   cfg.PanelMTLSURL,
		CACertPath:     cfg.PanelMTLSCACertPath,
		ClientCertPath: cfg.PanelMTLSClientCertPath,
		ClientKeyPath:  cfg.PanelMTLSClientKeyPath,
		Cert:           CollectCtlCert(cfg),
		EnrollCodeSet:  strings.TrimSpace(cfg.EnrollCode) != "",
	}
	if _, err := os.Stat(cfg.PanelMTLSCACertPath); err == nil {
		out.CACertPresent = true
	}
	if _, err := os.Stat(cfg.PanelMTLSClientKeyPath); err == nil {
		out.ClientKeyOK = true
	}
	return out
}

// RunXrayConfigTest runs the agent-local configured test argv against the
// current on-disk config. It never accepts external argv.
func RunXrayConfigTest(ctx context.Context, cfg Config) error {
	args, err := parseArgsJSON(cfg.XrayTestArgsJSON)
	if err != nil {
		return fmt.Errorf("invalid XRAY_TEST_ARGS_JSON: %w", err)
	}
	if len(args) == 0 {
		return fmt.Errorf("XRAY_TEST_ARGS_JSON is not configured")
	}
	cfgPath := strings.TrimSpace(cfg.XrayConfigPath)
	if cfgPath == "" {
		return fmt.Errorf("XRAY_CONFIG_PATH is required")
	}
	if _, err := os.Stat(cfgPath); err != nil {
		return fmt.Errorf("xray config: %w", err)
	}
	return runArgs(ctx, expandArgs(args, cfgPath, cfgPath), cfgPath, cfgPath)
}

// RenderBootstrapPreview renders the embedded template exactly like the
// bootstrap path would (env > reality/local fallbacks > bootstrap defaults)
// and reports JSON validity, without writing anything.
func RenderBootstrapPreview(cfg Config) (string, bool, error) {
	a := &Agent{cfg: cfg, xrayConfigPath: cfg.XrayConfigPath, realityPath: realityPathFromConfig(cfg)}
	if rc, ok, err := loadRealityConfig(a.realityPath); err == nil && ok {
		a.reality = &rc
	}
	rendered, err := a.renderBootstrapTemplate()
	if err != nil {
		return "", false, err
	}
	return rendered, json.Valid([]byte(rendered)), nil
}

// RollbackXrayLocal restores a config backup and reloads using the fixed
// agent-local argv — the same execXrayRollback logic the panel job runs.
// backupName may be empty (latest backup). The result map mirrors the job's.
func RollbackXrayLocal(ctx context.Context, cfg Config, backupName string) (map[string]any, error) {
	reloadArgs, err := parseArgsJSON(cfg.XrayReloadArgsJSON)
	if err != nil {
		return nil, fmt.Errorf("invalid XRAY_RELOAD_ARGS_JSON: %w", err)
	}
	a := &Agent{xrayConfigPath: cfg.XrayConfigPath, xrayReloadArgs: reloadArgs}
	return a.execXrayRollback(ctx, XrayRollbackRequest{BackupName: strings.TrimSpace(backupName)})
}

// ListXrayBackups returns the on-disk config backups, newest last.
func ListXrayBackups(cfg Config) ([]string, error) {
	cfgPath := filepath.Clean(strings.TrimSpace(cfg.XrayConfigPath))
	if cfgPath == "" {
		return nil, fmt.Errorf("XRAY_CONFIG_PATH is required")
	}
	dir := filepath.Dir(cfgPath)
	prefix := filepath.Base(cfgPath) + ".bak."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, 8)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), prefix) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func realityPathFromConfig(cfg Config) string {
	// Mirror agent.New's default: alongside the state file.
	return filepath.Join(filepath.Dir(cfg.StatePath), "reality.json")
}
