package agent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"neutrino/internal/xrayapi"
)

type fakeRuntimeClient struct {
	fakeUserSyncApplier
	online map[string][]xrayapi.OnlineIP
	errFor map[string]error
}

func (f *fakeRuntimeClient) PullUserTraffic(_ context.Context, _ string) (int64, int64, error) {
	return 0, 0, nil
}

func (f *fakeRuntimeClient) PullOnlineIPs(_ context.Context, email string) ([]xrayapi.OnlineIP, error) {
	if f.errFor != nil {
		if err, ok := f.errFor[email]; ok {
			return nil, err
		}
	}
	return f.online[email], nil
}

func TestBuildOnlineSnapshotUsesActiveSyncedUsers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	state := NewStateStore(filepath.Join(dir, "state.json"))
	if err := state.Update(func(s *State) {
		s.SyncedUsers = []UserSyncItem{
			{UserID: 1, Email: "alice@example.com", Status: "active"},
			{UserID: 2, Email: "disabled@example.com", Status: "disabled"},
			{UserID: 3, Email: "bob@example.com", Status: "active"},
			{UserID: 4, Email: "   ", Status: "active"},
		}
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	observedAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	xray := &fakeRuntimeClient{online: map[string][]xrayapi.OnlineIP{
		"alice@example.com": {
			{IP: "192.0.2.10", LastSeenAt: observedAt.Add(-time.Second)},
		},
		"bob@example.com": {
			{IP: "2001:db8::1", LastSeenAt: observedAt},
		},
	}}
	a := &Agent{state: state, xray: xray}

	got, err := a.buildOnlineSnapshot(context.Background(), observedAt)
	if err != nil {
		t.Fatalf("buildOnlineSnapshot: %v", err)
	}
	if got == nil {
		t.Fatalf("snapshot is nil")
	}
	if got.ObservedAt != observedAt.Format(time.RFC3339) {
		t.Fatalf("observed_at=%q", got.ObservedAt)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items=%+v, want 2", got.Items)
	}
	seen := map[string]int64{}
	for _, item := range got.Items {
		seen[item.ClientIP] = item.UserID
		if item.Email == "" {
			t.Fatalf("expected email on snapshot item: %+v", item)
		}
		if item.LastSeenAt == "" {
			t.Fatalf("expected last_seen_at on snapshot item: %+v", item)
		}
	}
	if seen["192.0.2.10"] != 1 || seen["2001:db8::1"] != 3 {
		t.Fatalf("unexpected item mapping: %+v items=%+v", seen, got.Items)
	}
}

func TestBuildOnlineSnapshotFailsWholeSnapshotOnCollectionError(t *testing.T) {
	t.Parallel()

	state := NewStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err := state.Update(func(s *State) {
		s.SyncedUsers = []UserSyncItem{
			{UserID: 1, Email: "alice@example.com", Status: "active"},
			{UserID: 2, Email: "bob@example.com", Status: "active"},
		}
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	a := &Agent{
		state: state,
		xray: &fakeRuntimeClient{
			online: map[string][]xrayapi.OnlineIP{"alice@example.com": {{IP: "192.0.2.10", LastSeenAt: time.Now().UTC()}}},
			errFor: map[string]error{"bob@example.com": errors.New("xray unavailable")},
		},
	}

	got, err := a.buildOnlineSnapshot(context.Background(), time.Now().UTC())
	if err == nil {
		t.Fatalf("expected collection error")
	}
	if got != nil {
		t.Fatalf("expected snapshot to be omitted on error, got %+v", got)
	}
}
