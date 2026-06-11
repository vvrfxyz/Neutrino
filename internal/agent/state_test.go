package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateStoreLoadReturnsErrorOnCorruptFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{corrupt"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	st := NewStateStore(path)
	if err := st.Load(); err == nil {
		t.Fatalf("expected load error for corrupt state file")
	}
}

func TestStateStoreLoadMissingFileIsNotAnError(t *testing.T) {
	t.Parallel()

	st := NewStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err := st.Load(); err != nil {
		t.Fatalf("missing state file must not error: %v", err)
	}
}

func TestStateStoreSaveRoundTripsXrayReloadPending(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	st := NewStateStore(path)
	if err := st.Update(func(s *State) { s.XrayReloadPending = true }); err != nil {
		t.Fatalf("update: %v", err)
	}

	st2 := NewStateStore(path)
	if err := st2.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !st2.Snapshot().XrayReloadPending {
		t.Fatalf("XrayReloadPending not persisted")
	}
}

func TestNewAgentFailsOnCorruptState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, []byte("{corrupt"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := New(Config{
		NodeID:                  1,
		PanelMTLSURL:            "https://127.0.0.1:1",
		PanelMTLSCACertPath:     filepath.Join(dir, "ca.crt"),
		PanelMTLSClientCertPath: filepath.Join(dir, "node.crt"),
		PanelMTLSClientKeyPath:  filepath.Join(dir, "node.key"),
		StatePath:               statePath,
	})
	if err == nil {
		t.Fatalf("expected New to fail on corrupt state file")
	}
}
