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
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}
