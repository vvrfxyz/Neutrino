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

	// SyncedUsersVersion is the canonical usersync.HashItems version of
	// SyncedUsers as of the last committed users sync. Empty on state files
	// written before the delta module; such agents must full-sync before any
	// delta can be accepted.
	SyncedUsersVersion string `json:"synced_users_version,omitempty"`

	// PendingUsersSync is the pre-operation journal for an in-flight users
	// sync: persisted before any Xray mutation, cleared in the same update
	// that commits the new baseline. While set, users hot-added by a failed
	// attempt may exist in Xray without being in SyncedUsers.
	PendingUsersSync *PendingUsersSyncState `json:"pending_users_sync,omitempty"`

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
}

type PendingUsersSyncState struct {
	Mode                 string         `json:"mode"`
	BaseVersion          string         `json:"base_version,omitempty"`
	TargetVersion        string         `json:"target_version"`
	TargetUsers          []UserSyncItem `json:"target_users"`
	PossibleRuntimeUsers []UserSyncItem `json:"possible_runtime_users,omitempty"`
	CreatedAt            string         `json:"created_at"`
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

// deepCopy clones everything Update mutations may touch so a failed save can
// roll back without leaving aliased slices/maps behind.
func (s State) deepCopy() State {
	out := s
	if s.SyncedUsers != nil {
		out.SyncedUsers = append([]UserSyncItem(nil), s.SyncedUsers...)
	}
	if s.AckedStats != nil {
		out.AckedStats = make(map[string]StatEntry, len(s.AckedStats))
		for k, v := range s.AckedStats {
			out.AckedStats[k] = v
		}
	}
	if s.PendingUsersSync != nil {
		p := *s.PendingUsersSync
		if p.TargetUsers != nil {
			p.TargetUsers = append([]UserSyncItem(nil), p.TargetUsers...)
		}
		if p.PossibleRuntimeUsers != nil {
			p.PossibleRuntimeUsers = append([]UserSyncItem(nil), p.PossibleRuntimeUsers...)
		}
		out.PendingUsersSync = &p
	}
	return out
}

// Update is memory-atomic: the mutation runs on a deep copy, and the copy is
// kept only after the save succeeds. A failed save leaves both the persisted
// file and the in-memory State unchanged, so a sync path can never observe a
// baseline that was not durably written.
func (st *StateStore) Update(fn func(s *State)) error {
	st.mu.Lock()
	defer st.mu.Unlock()

	next := st.s.deepCopy()
	fn(&next)
	prev := st.s
	st.s = next
	if err := st.saveLocked(); err != nil {
		st.s = prev
		return err
	}
	return nil
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
