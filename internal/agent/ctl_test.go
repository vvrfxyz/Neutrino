package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCtlState(t *testing.T, dir string) string {
	t.Helper()
	statePath := filepath.Join(dir, "state.json")
	st := NewStateStore(statePath)
	if err := st.Load(); err != nil {
		t.Fatalf("load fresh state: %v", err)
	}
	if err := st.Update(func(s *State) {
		s.SyncedUsers = []UserSyncItem{{UserID: 1, Email: "a", UUID: "u1"}, {UserID: 2, Email: "b", UUID: "u2"}}
		s.SyncedUsersVersion = "abc123"
		s.StatsEpoch = 7
		s.XrayReloadPending = true
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	return statePath
}

func TestCollectCtlStatus(t *testing.T) {
	dir := t.TempDir()
	statePath := writeCtlState(t, dir)
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"ok":true}`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	queueDir := filepath.Join(dir, "queue")
	q := NewDiskQueue(queueDir, 1<<20)
	if err := q.Enqueue(UsageBatch{Kind: "stats", Events: []UsageEvent{{SourceEventID: "e1"}}}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	cfg := Config{NodeID: 14, StatePath: statePath, XrayConfigPath: cfgPath, QueueDir: queueDir}
	s := CollectCtlStatus(cfg)

	if !s.StateOK || s.SyncedUsers != 2 || s.SyncedUsersVersion != "abc123" {
		t.Fatalf("state summary wrong: %+v", s)
	}
	if !s.XrayReloadPending || s.StatsEpoch != 7 {
		t.Fatalf("flags lost: %+v", s)
	}
	if !s.XrayConfigExists || !s.XrayConfigValid {
		t.Fatalf("xray config summary wrong: %+v", s)
	}
	if s.Queue.Batches != 1 || s.Queue.Bytes <= 0 {
		t.Fatalf("queue summary wrong: %+v", s.Queue)
	}
	if s.RealityOK {
		t.Fatalf("reality should be absent in temp dir")
	}
	if s.Cert.Present {
		t.Fatalf("cert should be absent")
	}

	// The whole snapshot must be JSON-marshalable for -json output.
	if _, err := json.Marshal(s); err != nil {
		t.Fatalf("marshal status: %v", err)
	}
}

func TestCollectCtlStatusCorruptState(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, []byte("{not json"), 0600); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	s := CollectCtlStatus(Config{StatePath: statePath})
	if s.StateOK || s.StateError == "" {
		t.Fatalf("corrupt state not surfaced: %+v", s)
	}
}

func TestCollectCtlQueueListsFilesOldestFirst(t *testing.T) {
	dir := t.TempDir()
	q := NewDiskQueue(dir, 1<<20)
	for _, id := range []string{"e1", "e2", "e3"} {
		if err := q.Enqueue(UsageBatch{Kind: "stats", Events: []UsageEvent{{SourceEventID: id}}}); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
		time.Sleep(2 * time.Millisecond) // distinct timestamps in filenames
	}
	out := CollectCtlQueue(Config{QueueDir: dir, QueueMaxBytes: 1 << 20}, true)
	if out.Batches != 3 || len(out.Files) != 3 {
		t.Fatalf("queue listing wrong: %+v", out)
	}
	if out.OldestBatch != out.Files[0] {
		t.Fatalf("oldest mismatch: %q vs %q", out.OldestBatch, out.Files[0])
	}
}

func TestRunXrayConfigTestUsesLocalArgvOnly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{}`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	marker := filepath.Join(dir, "ran")

	cfg := Config{
		XrayConfigPath:   cfgPath,
		XrayTestArgsJSON: `["sh","-c","echo ran > ` + marker + `"]`,
	}
	if err := RunXrayConfigTest(context.Background(), cfg); err != nil {
		t.Fatalf("test-xray: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("configured argv did not run: %v", err)
	}

	// Unconfigured argv is an explicit error, not a fallback to anything.
	if err := RunXrayConfigTest(context.Background(), Config{XrayConfigPath: cfgPath}); err == nil ||
		!strings.Contains(err.Error(), "XRAY_TEST_ARGS_JSON") {
		t.Fatalf("expected unconfigured-argv error, got %v", err)
	}
}

func TestListXrayBackupsAndLocalRollback(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"v":2}`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	bak := cfgPath + ".bak.20260101T000000Z"
	if err := os.WriteFile(bak, []byte(`{"v":1}`), 0600); err != nil {
		t.Fatalf("write backup: %v", err)
	}

	names, err := ListXrayBackups(Config{XrayConfigPath: cfgPath})
	if err != nil || len(names) != 1 || names[0] != filepath.Base(bak) {
		t.Fatalf("backups list wrong: %v %v", names, err)
	}

	out, err := RollbackXrayLocal(context.Background(), Config{
		XrayConfigPath:     cfgPath,
		XrayReloadArgsJSON: `["true"]`,
	}, "")
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("rollback result not ok: %+v", out)
	}
	b, _ := os.ReadFile(cfgPath)
	if string(b) != `{"v":1}` {
		t.Fatalf("config not restored: %s", b)
	}
}

func TestRenderBootstrapPreviewValid(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	// Provide reality material so key placeholders resolve.
	if _, err := ensureRealityConfig(filepath.Join(dir, "reality.json")); err != nil {
		t.Fatalf("ensure reality: %v", err)
	}
	rendered, valid, err := RenderBootstrapPreview(Config{
		StatePath:      statePath,
		XrayConfigPath: filepath.Join(dir, "config.json"),
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if !valid {
		t.Fatalf("bootstrap preview not valid json:\n%s", rendered)
	}
	if !strings.Contains(rendered, "vless") {
		t.Fatalf("unexpected preview content")
	}
}
