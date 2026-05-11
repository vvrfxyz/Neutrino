package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExecJobXrayRollbackReportsAppliedVersion(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	backupName := "config.json.bak.20260425T120000Z"
	backupPath := filepath.Join(dir, backupName)

	if err := os.WriteFile(configPath, []byte(`{"old":false}`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(backupPath, []byte(`{"old":true}`), 0600); err != nil {
		t.Fatalf("write backup: %v", err)
	}

	a := &Agent{
		xrayConfigPath: configPath,
		xrayReloadArgs: []string{"true"},
	}
	res := a.execJob(context.Background(), &NodeJob{
		Kind:        "xray_rollback",
		PayloadJSON: `{"backup_name":"` + backupName + `"}`,
	})

	if res.Status != "succeeded" {
		t.Fatalf("status=%q error=%q", res.Status, res.Error)
	}
	if res.AppliedVersion != "rollback:"+backupName {
		t.Fatalf("applied version=%q, want rollback marker", res.AppliedVersion)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read restored config: %v", err)
	}
	if string(got) != `{"old":true}` {
		t.Fatalf("config was not restored: %s", string(got))
	}
}

func TestExecXrayRollbackReloadFailureReportsRuntimeUnknown(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	backupName := "config.json.bak.20260425T120000Z"
	backupPath := filepath.Join(dir, backupName)

	if err := os.WriteFile(configPath, []byte(`{"old":false}`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(backupPath, []byte(`{"old":true}`), 0600); err != nil {
		t.Fatalf("write backup: %v", err)
	}

	a := &Agent{
		xrayConfigPath: configPath,
		xrayReloadArgs: []string{"false"},
	}
	_, err := a.execXrayRollback(context.Background(), XrayRollbackRequest{BackupName: backupName})
	if err == nil {
		t.Fatalf("expected rollback reload failure")
	}
	je, ok := err.(JobError)
	if !ok {
		t.Fatalf("expected JobError, got %T: %v", err, err)
	}
	result, ok := je.ResultJSON.(map[string]any)
	if !ok {
		t.Fatalf("expected result json map, got %#v", je.ResultJSON)
	}
	if result["runtime_state_unknown"] != true || result["rollback_restore_applied"] != true {
		t.Fatalf("expected runtime unknown restore markers, got %#v", result)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read restored config: %v", err)
	}
	if string(got) != `{"old":true}` {
		t.Fatalf("config was not restored before reload failure: %s", string(got))
	}
}
