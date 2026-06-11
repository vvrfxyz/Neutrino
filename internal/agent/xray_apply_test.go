package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// reloadCounter returns reload args that append a marker line to a counter
// file each time xray is asked to reload, plus a function to read the counter.
func reloadCounter(t *testing.T, dir string) ([]string, func() int) {
	t.Helper()
	counterPath := filepath.Join(dir, "reload.count")
	args := []string{"sh", "-c", "echo x >> " + counterPath}
	read := func() int {
		b, err := os.ReadFile(counterPath)
		if err != nil {
			if os.IsNotExist(err) {
				return 0
			}
			t.Fatalf("read counter: %v", err)
		}
		return strings.Count(string(b), "x")
	}
	return args, read
}

func TestExecXrayApplySkipsWhenCanonicalConfigUnchanged(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	args, count := reloadCounter(t, dir)

	a := &Agent{xrayConfigPath: configPath, xrayReloadArgs: args}
	template := `{"hello":"world","port":443}`

	res, err := a.execXrayApply(context.Background(), XrayApplyRequest{Template: template})
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if skipped, _ := res["skipped"].(bool); skipped {
		t.Fatalf("first apply should not skip; res=%+v", res)
	}
	if c := count(); c != 1 {
		t.Fatalf("counter after first apply = %d, want 1", c)
	}

	res, err = a.execXrayApply(context.Background(), XrayApplyRequest{Template: template})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if skipped, _ := res["skipped"].(bool); !skipped {
		t.Fatalf("second apply should skip; res=%+v", res)
	}
	if c := count(); c != 1 {
		t.Fatalf("counter must not advance on skip; got %d", c)
	}
}

func TestExecXrayApplySkipsWhitespaceOnlyDiff(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	args, count := reloadCounter(t, dir)

	// Seed an existing config with non-canonical whitespace + key order.
	pretty := "{\n    \"b\": 2,\n    \"a\": 1\n}\n"
	if err := os.WriteFile(configPath, []byte(pretty), 0600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	a := &Agent{xrayConfigPath: configPath, xrayReloadArgs: args}
	res, err := a.execXrayApply(context.Background(), XrayApplyRequest{Template: `{"a":1,"b":2}`})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if skipped, _ := res["skipped"].(bool); !skipped {
		t.Fatalf("apply should skip on whitespace-only diff; res=%+v", res)
	}
	if c := count(); c != 0 {
		t.Fatalf("counter must stay at 0; got %d", c)
	}

	got, _ := os.ReadFile(configPath)
	if string(got) != pretty {
		t.Fatalf("config file should be untouched on skip; got %q", string(got))
	}
}

func TestExecXrayApplyAppliesWhenContentDiffers(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	args, count := reloadCounter(t, dir)

	if err := os.WriteFile(configPath, []byte(`{"port":443}`), 0600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	a := &Agent{xrayConfigPath: configPath, xrayReloadArgs: args}
	res, err := a.execXrayApply(context.Background(), XrayApplyRequest{Template: `{"port":8443}`})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if skipped, _ := res["skipped"].(bool); skipped {
		t.Fatalf("apply should not skip when content differs")
	}
	if c := count(); c != 1 {
		t.Fatalf("counter should be 1 after apply; got %d", c)
	}
}

func TestExecXrayApplyAppliesWhenExistingConfigCorrupt(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	args, count := reloadCounter(t, dir)

	// Corrupt JSON on disk: smart-skip's parse fails → must apply.
	if err := os.WriteFile(configPath, []byte(`{not json`), 0600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	a := &Agent{xrayConfigPath: configPath, xrayReloadArgs: args}
	res, err := a.execXrayApply(context.Background(), XrayApplyRequest{Template: `{"port":443}`})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if skipped, _ := res["skipped"].(bool); skipped {
		t.Fatalf("apply should not skip when existing config fails to parse")
	}
	if c := count(); c != 1 {
		t.Fatalf("counter should be 1; got %d", c)
	}
}

// failNTimesReloadScript writes a reload script that fails the first n
// invocations and succeeds afterwards, returning the script path.
func failNTimesReloadScript(t *testing.T, dir string, n int) string {
	t.Helper()
	countPath := filepath.Join(dir, "reload.count")
	scriptPath := filepath.Join(dir, "reload.sh")
	script := `#!/bin/sh
countfile=` + strconv.Quote(countPath) + `
n=0
if [ -f "$countfile" ]; then n=$(cat "$countfile"); fi
n=$((n + 1))
echo "$n" > "$countfile"
if [ "$n" -le ` + strconv.Itoa(n) + ` ]; then exit 1; fi
exit 0
`
	if err := os.WriteFile(scriptPath, []byte(script), 0700); err != nil {
		t.Fatalf("write reload script: %v", err)
	}
	return scriptPath
}

// A failed reload installs the new config on disk but leaves xray running the
// old one. The retry of the same job renders identical bytes — without the
// reload-pending flag, skip-if-unchanged would mark the retry "succeeded"
// without ever reloading xray.
func TestExecXrayApplyRetriesReloadAfterFailureDespiteUnchangedConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	scriptPath := failNTimesReloadScript(t, dir, 1)

	a := &Agent{xrayConfigPath: configPath, xrayReloadArgs: []string{"sh", scriptPath}}
	template := `{"port":443}`

	if _, err := a.execXrayApply(context.Background(), XrayApplyRequest{Template: template}); err == nil {
		t.Fatalf("first apply should fail on reload")
	}
	if !a.xrayReloadPending() {
		t.Fatalf("reload-pending flag must be set after a failed reload")
	}

	// Retry with the identical template: must NOT skip, must reload.
	res, err := a.execXrayApply(context.Background(), XrayApplyRequest{Template: template})
	if err != nil {
		t.Fatalf("retry apply: %v", err)
	}
	if skipped, _ := res["skipped"].(bool); skipped {
		t.Fatalf("retry must not be skipped while reload is pending; res=%+v", res)
	}
	if reloaded, _ := res["runtime_reloaded"].(bool); !reloaded {
		t.Fatalf("retry must reload xray; res=%+v", res)
	}
	if a.xrayReloadPending() {
		t.Fatalf("reload-pending flag must clear after a successful reload")
	}

	// Third apply with the same template skips again as usual.
	res, err = a.execXrayApply(context.Background(), XrayApplyRequest{Template: template})
	if err != nil {
		t.Fatalf("third apply: %v", err)
	}
	if skipped, _ := res["skipped"].(bool); !skipped {
		t.Fatalf("third apply should skip once reload recovered; res=%+v", res)
	}
}

func TestExecXrayApplyReloadPendingPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	scriptPath := failNTimesReloadScript(t, dir, 1)
	statePath := filepath.Join(dir, "state.json")

	st := NewStateStore(statePath)
	a := &Agent{xrayConfigPath: configPath, xrayReloadArgs: []string{"sh", scriptPath}, state: st}
	if _, err := a.execXrayApply(context.Background(), XrayApplyRequest{Template: `{"port":443}`}); err == nil {
		t.Fatalf("first apply should fail on reload")
	}

	// Simulate agent restart: fresh StateStore from the same file.
	st2 := NewStateStore(statePath)
	if err := st2.Load(); err != nil {
		t.Fatalf("load state: %v", err)
	}
	a2 := &Agent{xrayConfigPath: configPath, xrayReloadArgs: []string{"sh", scriptPath}, state: st2}
	res, err := a2.execXrayApply(context.Background(), XrayApplyRequest{Template: `{"port":443}`})
	if err != nil {
		t.Fatalf("apply after restart: %v", err)
	}
	if skipped, _ := res["skipped"].(bool); skipped {
		t.Fatalf("apply after restart must not skip while reload is pending; res=%+v", res)
	}
}

// The reload-pending flag must be persisted BEFORE the config is installed:
// if the agent dies between the rename and a successful reload, the restarted
// agent would otherwise see disk==desired with pending=false and skip the
// retry without ever reloading xray.
func TestExecXrayApplyPersistsReloadPendingBeforeReload(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	statePath := filepath.Join(dir, "state.json")
	observedPath := filepath.Join(dir, "observed")

	// The reload script runs exactly in the crash window (after rename,
	// before reload success) and records whether the flag is already durable.
	script := `#!/bin/sh
if grep -q '"xray_reload_pending": true' ` + strconv.Quote(statePath) + `; then
  echo yes > ` + strconv.Quote(observedPath) + `
else
  echo no > ` + strconv.Quote(observedPath) + `
fi
exit 0
`
	scriptPath := filepath.Join(dir, "reload.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0700); err != nil {
		t.Fatalf("write reload script: %v", err)
	}

	a := &Agent{xrayConfigPath: configPath, xrayReloadArgs: []string{"sh", scriptPath}, state: NewStateStore(statePath)}
	if _, err := a.execXrayApply(context.Background(), XrayApplyRequest{Template: `{"port":443}`}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	observed, err := os.ReadFile(observedPath)
	if err != nil {
		t.Fatalf("read observed: %v", err)
	}
	if strings.TrimSpace(string(observed)) != "yes" {
		t.Fatalf("reload-pending flag was not persisted before the reload ran")
	}
	if a.xrayReloadPending() {
		t.Fatalf("flag must clear after successful reload")
	}
}

func TestExecXrayApplyReportsRollbackReloadForRepair(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	countPath := filepath.Join(dir, "reload.count")
	scriptPath := filepath.Join(dir, "reload.sh")
	script := `#!/bin/sh
countfile=` + strconv.Quote(countPath) + `
n=0
if [ -f "$countfile" ]; then n=$(cat "$countfile"); fi
n=$((n + 1))
echo "$n" > "$countfile"
if [ "$n" -eq 1 ]; then exit 1; fi
exit 0
`
	if err := os.WriteFile(scriptPath, []byte(script), 0700); err != nil {
		t.Fatalf("write reload script: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(`{"old":true}`), 0600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	a := &Agent{xrayConfigPath: configPath, xrayReloadArgs: []string{"sh", scriptPath}}
	_, err := a.execXrayApply(context.Background(), XrayApplyRequest{
		Template:       `{"old":false}`,
		RollbackOnFail: true,
	})
	if err == nil {
		t.Fatalf("expected apply failure")
	}
	je, ok := err.(JobError)
	if !ok {
		t.Fatalf("expected JobError, got %T: %v", err, err)
	}
	result, ok := je.ResultJSON.(map[string]any)
	if !ok {
		t.Fatalf("expected result json map, got %#v", je.ResultJSON)
	}
	if result["rollback_applied"] != true || result["runtime_reloaded"] != true {
		t.Fatalf("expected rollback_applied/runtime_reloaded markers, got %#v", result)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(got) != `{"old":true}` {
		t.Fatalf("expected config restored after rollback, got %s", string(got))
	}
}

// blockBackupWrites plants non-empty directories at the backup paths the next
// apply would use, making the backup os.WriteFile fail deterministically.
func blockBackupWrites(t *testing.T, configPath string) {
	t.Helper()
	now := time.Now().UTC()
	for _, ts := range []time.Time{now, now.Add(time.Second)} {
		p := fmt.Sprintf("%s.bak.%s", configPath, ts.Format("20060102T150405Z"))
		if err := os.MkdirAll(filepath.Join(p, "block"), 0700); err != nil {
			t.Fatalf("plant backup blocker: %v", err)
		}
	}
}

func TestExecXrayApplyBackupFailureWithRollbackFailsBeforeMutation(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	args, count := reloadCounter(t, dir)

	if err := os.WriteFile(configPath, []byte(`{"old":true}`), 0600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	blockBackupWrites(t, configPath)

	a := &Agent{xrayConfigPath: configPath, xrayReloadArgs: args}
	_, err := a.execXrayApply(context.Background(), XrayApplyRequest{
		Template:       `{"old":false}`,
		RollbackOnFail: true,
	})
	if err == nil {
		t.Fatalf("expected apply failure when backup write fails with rollback_on_fail")
	}
	je, ok := err.(JobError)
	if !ok {
		t.Fatalf("expected JobError, got %T: %v", err, err)
	}
	if !je.Retryable {
		t.Fatalf("backup write failure must be retryable, got %+v", je)
	}
	if !strings.Contains(je.Msg, "backup") {
		t.Fatalf("error should mention backup, got %q", je.Msg)
	}
	got, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatalf("read config: %v", readErr)
	}
	if string(got) != `{"old":true}` {
		t.Fatalf("live config must be untouched when backup fails, got %s", string(got))
	}
	if c := count(); c != 0 {
		t.Fatalf("reload must not run when backup fails; counter=%d", c)
	}
}

func TestExecXrayApplyBackupFailureWithoutRollbackContinues(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	args, count := reloadCounter(t, dir)

	if err := os.WriteFile(configPath, []byte(`{"old":true}`), 0600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	blockBackupWrites(t, configPath)

	a := &Agent{xrayConfigPath: configPath, xrayReloadArgs: args}
	res, err := a.execXrayApply(context.Background(), XrayApplyRequest{Template: `{"old":false}`})
	if err != nil {
		t.Fatalf("apply should continue past backup failure without rollback_on_fail: %v", err)
	}
	if reloaded, _ := res["runtime_reloaded"].(bool); !reloaded {
		t.Fatalf("expected runtime_reloaded; res=%+v", res)
	}
	if _, has := res["backup_name"]; has {
		t.Fatalf("must not report a backup_name when the backup failed; res=%+v", res)
	}
	got, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatalf("read config: %v", readErr)
	}
	if string(got) != `{"old":false}` {
		t.Fatalf("new config should be installed, got %s", string(got))
	}
	if c := count(); c != 1 {
		t.Fatalf("reload should run once; counter=%d", c)
	}
}

func TestExecXrayApplyPrunesOldBackups(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	args, _ := reloadCounter(t, dir)

	if err := os.WriteFile(configPath, []byte(`{"old":true}`), 0600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	for i := 0; i < 7; i++ {
		name := fmt.Sprintf("%s.bak.2020010%dT000000Z", configPath, i+1)
		if err := os.WriteFile(name, []byte("old"), 0600); err != nil {
			t.Fatalf("seed backup: %v", err)
		}
	}

	a := &Agent{xrayConfigPath: configPath, xrayReloadArgs: args}
	res, err := a.execXrayApply(context.Background(), XrayApplyRequest{Template: `{"old":false}`})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	newBackup, _ := res["backup_name"].(string)
	if newBackup == "" {
		t.Fatalf("expected backup_name in result; res=%+v", res)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	baks := make([]string, 0, 8)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "config.json.bak.") {
			baks = append(baks, e.Name())
		}
	}
	sort.Strings(baks)
	if len(baks) != keepConfigBackups {
		t.Fatalf("expected %d backups after prune, got %d: %v", keepConfigBackups, len(baks), baks)
	}
	if baks[len(baks)-1] != newBackup {
		t.Fatalf("newest backup %s must survive prune, got %v", newBackup, baks)
	}
	for _, name := range baks {
		if name == "config.json.bak.20200101T000000Z" || name == "config.json.bak.20200102T000000Z" || name == "config.json.bak.20200103T000000Z" {
			t.Fatalf("oldest backups must be pruned, still present: %v", baks)
		}
	}
}
