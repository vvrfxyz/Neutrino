package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type XrayApplyRequest struct {
	Template       string            `json:"template"`
	Vars           map[string]string `json:"vars"`
	RollbackOnFail bool              `json:"rollback_on_fail,omitempty"`
}

type XrayRollbackRequest struct {
	BackupName string `json:"backup_name,omitempty"` // basename only; if empty, rollback latest backup
}

// xrayReloadPending reports whether the previous reload attempt failed after a
// config was installed on disk. While true, skip-if-unchanged is disabled.
func (a *Agent) xrayReloadPending() bool {
	if a.state != nil {
		return a.state.Snapshot().XrayReloadPending
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.reloadPendingMem
}

func (a *Agent) setXrayReloadPending(pending bool) {
	if a.state != nil {
		_ = a.state.Update(func(s *State) { s.XrayReloadPending = pending })
		return
	}
	a.mu.Lock()
	a.reloadPendingMem = pending
	a.mu.Unlock()
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

	// Smart restart: when the canonical form of the new config matches the
	// existing on-disk config, skip the entire apply pipeline (backup + write
	// + reload). This prevents needless connection churn when the panel emits
	// a re-sync that produces an identical config. Never skip while a previous
	// reload is still pending: the on-disk config may already match without
	// the running xray ever having loaded it.
	if old, err := os.ReadFile(a.xrayConfigPath); err == nil && !a.xrayReloadPending() {
		if configEqualCanonical(old, renderedBytes) {
			return map[string]any{
				"ok":               true,
				"skipped":          true,
				"reason":           "config unchanged",
				"config_path":      a.xrayConfigPath,
				"runtime_reloaded": false,
			}, nil
		}
	}

	backupPath := ""
	if old, err := os.ReadFile(a.xrayConfigPath); err == nil {
		backupPath = fmt.Sprintf("%s.bak.%s", a.xrayConfigPath, time.Now().UTC().Format("20060102T150405Z"))
		if werr := os.WriteFile(backupPath, old, 0600); werr != nil {
			// A failed/truncated backup must never be used as a rollback source.
			_ = os.Remove(backupPath)
			if req.RollbackOnFail {
				// rollback_on_fail relies on this backup; fail before touching the
				// live config so a retry can run with an intact rollback path.
				return nil, JobError{Retryable: true, Msg: "write config backup failed: " + werr.Error()}
			}
			log.Printf("warn: write config backup failed (%s): %v; continuing without backup", backupPath, werr)
			backupPath = ""
		}
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

	// Write-ahead the reload intent: once the rename lands, the on-disk config
	// no longer matches the running xray. If the agent dies anywhere between
	// the rename and a successful reload, the persisted flag keeps the retried
	// job from being skipped as "config unchanged".
	a.setXrayReloadPending(true)

	if err := os.Rename(tmpPath, a.xrayConfigPath); err != nil {
		// Leave the pending flag set: it may also be carrying state from an
		// earlier failed reload, and a spurious pending only costs one extra
		// reload on the next apply.
		return nil, JobError{Retryable: true, Msg: "install config failed: " + err.Error()}
	}

	if err := runArgs(ctx, expandArgs(a.xrayReloadArgs, a.xrayConfigPath, a.xrayConfigPath), a.xrayConfigPath, a.xrayConfigPath); err != nil {
		rolledBack := false
		rollbackRestored := false
		rollbackErr := ""
		result := map[string]any{
			"ok":                       false,
			"runtime_reload_attempted": true,
			"runtime_state_unknown":    true,
		}
		if req.RollbackOnFail && backupPath != "" {
			if rbErr := restoreBackupFile(a.xrayConfigPath, backupPath); rbErr != nil {
				rollbackErr = rbErr.Error()
				result["rollback_error"] = rollbackErr
			} else if rbReloadErr := runArgs(ctx, expandArgs(a.xrayReloadArgs, a.xrayConfigPath, a.xrayConfigPath), a.xrayConfigPath, a.xrayConfigPath); rbReloadErr != nil {
				rollbackRestored = true
				rollbackErr = rbReloadErr.Error()
				result["rollback_restore_applied"] = true
				result["rollback_reload_error"] = rollbackErr
			} else {
				rollbackRestored = true
				rolledBack = true
				delete(result, "runtime_state_unknown")
				result["runtime_reloaded"] = true
				result["rollback_applied"] = true
			}
		}
		// Unless rollback fully restored and reloaded the previous config, the
		// running xray no longer matches the on-disk file. Record that so the
		// retry attempt cannot be skipped by the unchanged-config check.
		a.setXrayReloadPending(!rolledBack)
		msg := "xray reload failed: " + err.Error()
		if rolledBack {
			msg = msg + " (rolled back to previous config)"
		} else if rollbackErr != "" {
			msg = msg + " (rollback failed: " + rollbackErr + ")"
		}
		if rollbackRestored {
			result["rollback_restore_applied"] = true
		}
		return nil, JobError{Retryable: true, Msg: msg, ResultJSON: result}
	}
	a.setXrayReloadPending(false)

	pruneConfigBackups(a.xrayConfigPath, keepConfigBackups)

	out := map[string]any{"ok": true, "config_path": a.xrayConfigPath, "runtime_reloaded": true}
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
	// Write-ahead, mirroring execXrayApply: the restore replaces the on-disk
	// config before any reload happens.
	a.setXrayReloadPending(true)
	if err := restoreBackupFile(a.xrayConfigPath, backupPath); err != nil {
		return nil, JobError{Retryable: true, Msg: "restore failed: " + err.Error()}
	}
	if err := runArgs(ctx, expandArgs(a.xrayReloadArgs, a.xrayConfigPath, a.xrayConfigPath), a.xrayConfigPath, a.xrayConfigPath); err != nil {
		a.setXrayReloadPending(true)
		return nil, JobError{
			Retryable: true,
			Msg:       "reload failed: " + err.Error(),
			ResultJSON: map[string]any{
				"ok":                       false,
				"backup_name":              filepath.Base(backupPath),
				"rollback_restore_applied": true,
				"runtime_reload_attempted": true,
				"runtime_state_unknown":    true,
			},
		}
	}
	a.setXrayReloadPending(false)
	return map[string]any{
		"ok":               true,
		"config_path":      a.xrayConfigPath,
		"backup_name":      filepath.Base(backupPath),
		"runtime_reloaded": true,
		"rollback_applied": true,
	}, nil
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

// keepConfigBackups is how many config.json.bak.* files pruneConfigBackups
// retains after a successful apply.
const keepConfigBackups = 5

// pruneConfigBackups removes old <config>.bak.<timestamp> files, keeping the
// most recent `keep`. Best-effort: failures only log, never fail the apply.
func pruneConfigBackups(configPath string, keep int) {
	configPath = filepath.Clean(strings.TrimSpace(configPath))
	if configPath == "" || keep < 0 {
		return
	}
	dir := filepath.Dir(configPath)
	prefix := filepath.Base(configPath) + ".bak."
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("warn: prune config backups: read dir %s: %v", dir, err)
		return
	}
	cands := make([]string, 0, 8)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), prefix) {
			cands = append(cands, e.Name())
		}
	}
	if len(cands) <= keep {
		return
	}
	// Suffix is yyyymmddThhmmssZ, so lexical order is chronological.
	sort.Strings(cands)
	for _, name := range cands[:len(cands)-keep] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			log.Printf("warn: prune config backup %s: %v", name, err)
		}
	}
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
