package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// State is persisted to disk to make usage push idempotent across restarts.
// Keep it small and stable; bump Version if the schema changes incompatibly.
type State struct {
	Version int `json:"version"`

	// SyncedUsers is the last user list received from panel via /v1/users/sync.
	SyncedUsers []UserSyncItem `json:"synced_users,omitempty"`

	// AckedStats stores last counters that have been successfully applied on the panel side.
	AckedStats map[string]StatEntry `json:"acked_stats,omitempty"`
	// StatsEpoch is included in xray-stats source_event_id to avoid id collisions when xray counters reset.
	StatsEpoch int64 `json:"stats_epoch,omitempty"`

	Access AccessState `json:"access,omitempty"`

	// XrayReloadPending records that the last xray reload attempt failed after
	// a new config was installed on disk. While set, skip-if-unchanged must not
	// skip an apply: the on-disk config may match the desired config without
	// the running xray ever having loaded it.
	XrayReloadPending bool `json:"xray_reload_pending,omitempty"`

	// PendingStatsTarget captures the counters we will commit into AckedStats after a successful push.
	PendingStatsTarget map[string]StatEntry `json:"pending_stats_target,omitempty"`
	// PendingStatsEpochTarget captures the epoch we will commit into StatsEpoch after a successful push.
	PendingStatsEpochTarget *int64 `json:"pending_stats_epoch_target,omitempty"`
	// PendingUsageEvents is retried until accepted by the panel.
	PendingUsageEvents []UsageEvent `json:"pending_usage_events,omitempty"`
}

type AccessState struct {
	Path   string `json:"path,omitempty"`
	Inode  uint64 `json:"inode,omitempty"`
	Offset int64  `json:"offset,omitempty"`
}

type StateStore struct {
	path string
	mu   sync.Mutex
	s    State
}

func NewStateStore(path string) *StateStore {
	path = filepath.Clean(path)
	return &StateStore{
		path: path,
		s: State{
			Version:    4,
			AckedStats: make(map[string]StatEntry),
		},
	}
}

func (st *StateStore) Load() error {
	st.mu.Lock()
	defer st.mu.Unlock()

	b, err := os.ReadFile(st.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s.Version < 4 {
		s.Version = 4
	}
	if s.AckedStats == nil {
		s.AckedStats = make(map[string]StatEntry)
	}
	st.s = s
	return nil
}

func (st *StateStore) Snapshot() State {
	st.mu.Lock()
	defer st.mu.Unlock()

	// Return a shallow copy; maps/slices are referenced, but callers must treat it read-only.
	return st.s
}

func (st *StateStore) Update(fn func(s *State)) error {
	st.mu.Lock()
	defer st.mu.Unlock()

	fn(&st.s)
	return st.saveLocked()
}

func (st *StateStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(st.path), 0700); err != nil {
		// If path is like "state.json" in cwd, Dir() is "." which is fine.
		return err
	}
	b, err := json.MarshalIndent(st.s, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	// fsync before rename: a crash after rename must never leave a truncated
	// state file, or AckedStats/Access offsets would silently rewind and the
	// agent would re-report usage the panel cannot fully dedupe.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, st.path); err != nil {
		return err
	}
	// Best-effort directory fsync so the rename itself is durable.
	if dir, err := os.Open(filepath.Dir(st.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
