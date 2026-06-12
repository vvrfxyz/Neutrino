package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"neutrino/internal/usersync"
)

func newDeltaTestAgent(t *testing.T, xray RuntimeClient) *Agent {
	t.Helper()
	state := NewStateStore(filepath.Join(t.TempDir(), "state.json"))
	return &Agent{state: state, xray: xray}
}

func seedBaseline(t *testing.T, a *Agent, users []UserSyncItem) string {
	t.Helper()
	version := hashUsers(users)
	if err := a.state.Update(func(s *State) {
		s.SyncedUsers = users
		s.SyncedUsersVersion = version
	}); err != nil {
		t.Fatalf("seed baseline: %v", err)
	}
	return version
}

func deltaPayloadJSON(t *testing.T, p usersync.JobPayload) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(b)
}

func baselineUsers() []UserSyncItem {
	return []UserSyncItem{
		{UserID: 1, Email: "alice@x", Status: "active", UUID: "u1"},
		{UserID: 2, Email: "bob@x", Status: "active", UUID: "u2"},
		{UserID: 3, Email: "carol@x", Status: "active", UUID: "u3"},
		{UserID: 5, Email: "erin@x", Status: "active", UUID: "u5"},
		{UserID: 6, Email: "frank@x", Status: "active", UUID: "u6"},
		{UserID: 7, Email: "grace@x", Status: "active", UUID: "u7"},
		{UserID: 10, Email: "heidi@x", Status: "active", UUID: "u10"},
		{UserID: 11, Email: "ivan@x", Status: "active", UUID: "u11"},
	}
}

func TestExecUsersSyncDeltaSuccess(t *testing.T) {
	xray := &fakeRuntimeClient{}
	a := newDeltaTestAgent(t, xray)
	base := seedBaseline(t, a, baselineUsers())

	targetItems := make([]usersync.Item, 0, len(baselineUsers())+1)
	for _, it := range syncItems(baselineUsers()) {
		switch it.UserID {
		case 2:
			it.Status = "disabled" // bob disabled -> runtime remove
		case 3:
			continue // carol removed
		}
		targetItems = append(targetItems, it)
	}
	targetItems = append(targetItems, usersync.Item{UserID: 4, Email: "dave@x", Status: "active", UUID: "u4"})
	changes, safe := usersync.DiffItems(syncItems(baselineUsers()), targetItems)
	if !safe {
		t.Fatalf("expected safe diff")
	}
	target := usersync.HashItems(targetItems)
	payload := deltaPayloadJSON(t, usersync.JobPayload{
		Schema: usersync.SchemaV1, Mode: usersync.ModeDelta,
		BaseVersion: base, TargetVersion: target, Changes: changes,
	})

	out, err := a.execUsersSync(context.Background(), payload)
	if err != nil {
		t.Fatalf("delta sync: %v", err)
	}
	if out.AppliedVersion != target {
		t.Fatalf("applied=%s want %s", out.AppliedVersion, target)
	}
	if out.Result["mode"] != "delta" || out.Result["target_version"] != target {
		t.Fatalf("result: %+v", out.Result)
	}
	if len(xray.upserted) != 1 || xray.upserted[0] != "dave@x=u4" {
		t.Fatalf("upserts: %+v", xray.upserted)
	}
	// bob disabled + carol removed
	if len(xray.removed) != 2 {
		t.Fatalf("removed: %+v", xray.removed)
	}
	snap := a.state.Snapshot()
	if snap.SyncedUsersVersion != target {
		t.Fatalf("persisted version=%s want %s", snap.SyncedUsersVersion, target)
	}
	if hashUsers(snap.SyncedUsers) != target {
		t.Fatalf("persisted users do not hash to target")
	}
	if snap.PendingUsersSync != nil {
		t.Fatalf("journal not cleared after success")
	}
}

func TestExecUsersSyncDeltaBaseMismatchRequestsFull(t *testing.T) {
	xray := &fakeRuntimeClient{}
	a := newDeltaTestAgent(t, xray)
	seedBaseline(t, a, baselineUsers())

	payload := deltaPayloadJSON(t, usersync.JobPayload{
		Schema: usersync.SchemaV1, Mode: usersync.ModeDelta,
		BaseVersion: "not-the-base", TargetVersion: "whatever",
		Changes: []usersync.Change{{Action: usersync.ActionUpsert, User: usersync.Item{UserID: 9, Email: "z@x", Status: "active", UUID: "u9"}}},
	})
	_, err := a.execUsersSync(context.Background(), payload)
	assertNeedFullSync(t, err, "base_version_mismatch")
	if len(xray.upserted)+len(xray.removed) != 0 {
		t.Fatalf("xray must not be touched on base mismatch")
	}
	if a.state.Snapshot().PendingUsersSync != nil {
		t.Fatalf("no journal may be written on base mismatch")
	}
}

func TestExecUsersSyncDeltaLocalHashMismatchRequestsFull(t *testing.T) {
	xray := &fakeRuntimeClient{}
	a := newDeltaTestAgent(t, xray)
	base := seedBaseline(t, a, baselineUsers())

	changes := []usersync.Change{{Action: usersync.ActionUpsert, User: usersync.Item{UserID: 9, Email: "z@x", Status: "active", UUID: "u9"}}}
	payload := deltaPayloadJSON(t, usersync.JobPayload{
		Schema: usersync.SchemaV1, Mode: usersync.ModeDelta,
		BaseVersion: base, TargetVersion: "wrong-target-hash", Changes: changes,
	})
	_, err := a.execUsersSync(context.Background(), payload)
	assertNeedFullSync(t, err, "local_hash_mismatch")
	if len(xray.upserted)+len(xray.removed) != 0 {
		t.Fatalf("xray must not be touched on hash mismatch")
	}
}

func TestExecUsersSyncDeltaRemoveBaseMismatchRequestsFull(t *testing.T) {
	xray := &fakeRuntimeClient{}
	a := newDeltaTestAgent(t, xray)
	base := seedBaseline(t, a, baselineUsers())

	// Remove carries the right user_id but the wrong uuid: the agent would
	// compute the right target hash while deleting based on stale identity.
	badRemove := usersync.Change{Action: usersync.ActionRemove, User: usersync.Item{UserID: 3, Email: "carol@x", Status: "active", UUID: "WRONG"}}
	next, err := usersync.ApplyChanges(syncItems(baselineUsers()), []usersync.Change{
		{Action: usersync.ActionRemove, User: usersync.Item{UserID: 3, Email: "carol@x", Status: "active", UUID: "u3"}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	payload := deltaPayloadJSON(t, usersync.JobPayload{
		Schema: usersync.SchemaV1, Mode: usersync.ModeDelta,
		BaseVersion: base, TargetVersion: usersync.HashItems(next), Changes: []usersync.Change{badRemove},
	})
	_, err = a.execUsersSync(context.Background(), payload)
	assertNeedFullSync(t, err, "remove_base_mismatch")
	if len(xray.removed) != 0 {
		t.Fatalf("xray must not be touched on remove mismatch")
	}
}

func TestExecUsersSyncDeltaInvalidLocalBaselineRequestsFull(t *testing.T) {
	xray := &fakeRuntimeClient{}
	a := newDeltaTestAgent(t, xray)
	// Corrupt persisted baseline: duplicate emails. Version matches the hash
	// of the corrupt list, so only canonical validation can catch it.
	corrupt := []UserSyncItem{
		{UserID: 1, Email: "same@x", Status: "active", UUID: "u1"},
		{UserID: 2, Email: "same@x", Status: "active", UUID: "u2"},
	}
	base := seedBaseline(t, a, corrupt)

	payload := deltaPayloadJSON(t, usersync.JobPayload{
		Schema: usersync.SchemaV1, Mode: usersync.ModeDelta,
		BaseVersion: base, TargetVersion: "anything",
		Changes: []usersync.Change{{Action: usersync.ActionUpsert, User: usersync.Item{UserID: 9, Email: "z@x", Status: "active", UUID: "u9"}}},
	})
	_, err := a.execUsersSync(context.Background(), payload)
	assertNeedFullSync(t, err, "invalid_local_baseline")
}

func TestExecUsersSyncDeltaXrayFailureIsRetryableAndKeepsState(t *testing.T) {
	xray := &fakeRuntimeClient{}
	xray.upsertErrFor = map[string]error{"dave@x": errors.New("xray down")}
	a := newDeltaTestAgent(t, xray)
	base := seedBaseline(t, a, baselineUsers())

	targetItems := append(syncItems(baselineUsers()), usersync.Item{UserID: 4, Email: "dave@x", Status: "active", UUID: "u4"})
	changes, safe := usersync.DiffItems(syncItems(baselineUsers()), targetItems)
	if !safe {
		t.Fatalf("expected safe diff")
	}
	target := usersync.HashItems(targetItems)
	payload := deltaPayloadJSON(t, usersync.JobPayload{
		Schema: usersync.SchemaV1, Mode: usersync.ModeDelta,
		BaseVersion: base, TargetVersion: target, Changes: changes,
	})
	_, err := a.execUsersSync(context.Background(), payload)
	var je JobError
	if !errors.As(err, &je) || !je.Retryable {
		t.Fatalf("expected retryable JobError, got %v", err)
	}
	snap := a.state.Snapshot()
	if snap.SyncedUsersVersion != base {
		t.Fatalf("committed baseline must be unchanged after xray failure")
	}
	if snap.PendingUsersSync == nil || snap.PendingUsersSync.TargetVersion != target {
		t.Fatalf("journal must be retained for the retry, got %+v", snap.PendingUsersSync)
	}
}

func TestExecUsersSyncDeltaUnrelatedPendingJournalRequestsFull(t *testing.T) {
	xray := &fakeRuntimeClient{}
	a := newDeltaTestAgent(t, xray)
	base := seedBaseline(t, a, baselineUsers())
	if err := a.state.Update(func(s *State) {
		s.PendingUsersSync = &PendingUsersSyncState{
			Mode: "full", TargetVersion: "other-target",
			TargetUsers: []UserSyncItem{{UserID: 8, Email: "ghost@x", Status: "active", UUID: "u8"}},
			CreatedAt:   "2026-01-01T00:00:00Z",
		}
	}); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	targetItems := append(syncItems(baselineUsers()), usersync.Item{UserID: 4, Email: "dave@x", Status: "active", UUID: "u4"})
	changes, _ := usersync.DiffItems(syncItems(baselineUsers()), targetItems)
	payload := deltaPayloadJSON(t, usersync.JobPayload{
		Schema: usersync.SchemaV1, Mode: usersync.ModeDelta,
		BaseVersion: base, TargetVersion: usersync.HashItems(targetItems), Changes: changes,
	})
	_, err := a.execUsersSync(context.Background(), payload)
	assertNeedFullSync(t, err, "pending_journal_mismatch")
	if len(xray.upserted) != 0 {
		t.Fatalf("xray must not be touched with unrelated journal")
	}
}

func TestExecUsersSyncUnknownSchemaOrModeIsPermanent(t *testing.T) {
	a := newDeltaTestAgent(t, &fakeRuntimeClient{})
	for _, payload := range []string{
		`{"schema":2,"mode":"full","target_version":"v"}`,
		`{"schema":1,"mode":"merge","target_version":"v"}`,
		`{"schema":1,"mode":"full"}`, // missing target_version
		`not json at all`,
	} {
		_, err := a.execUsersSync(context.Background(), payload)
		var je JobError
		if !errors.As(err, &je) {
			t.Fatalf("payload %q: expected JobError, got %v", payload, err)
		}
		if je.Retryable {
			t.Fatalf("payload %q: must be a permanent failure", payload)
		}
	}
}

func TestExecUsersSyncLegacyPayloadStillFullSyncs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"users":[{"user_id":1,"email":"alice@x","uuid":"u1","status":"active"}]}`))
	}))
	defer srv.Close()

	xray := &fakeRuntimeClient{}
	a := newDeltaTestAgent(t, xray)
	a.panel = &PanelClient{baseURL: srv.URL, client: srv.Client()}

	for _, payload := range []string{"", "{}"} {
		out, err := a.execUsersSync(context.Background(), payload)
		if err != nil {
			t.Fatalf("legacy payload %q: %v", payload, err)
		}
		want := hashUsers([]UserSyncItem{{UserID: 1, Email: "alice@x", UUID: "u1", Status: "active"}})
		if out.AppliedVersion != want {
			t.Fatalf("legacy payload %q: applied=%s want locally computed %s", payload, out.AppliedVersion, want)
		}
	}
	snap := a.state.Snapshot()
	if snap.SyncedUsersVersion == "" {
		t.Fatalf("old-panel full sync must persist a locally computed baseline version")
	}
}

func TestExecUsersSyncFullResponseHashMismatchIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema":1,"version":"lying-version","users":[{"user_id":1,"email":"alice@x","uuid":"u1","status":"active"}]}`))
	}))
	defer srv.Close()

	xray := &fakeRuntimeClient{}
	a := newDeltaTestAgent(t, xray)
	a.panel = &PanelClient{baseURL: srv.URL, client: srv.Client()}

	_, err := a.execUsersSync(context.Background(), "")
	var je JobError
	if !errors.As(err, &je) || !je.Retryable {
		t.Fatalf("expected retryable JobError, got %v", err)
	}
	res, ok := je.ResultJSON.(map[string]any)
	if !ok || res["reason"] != "full_response_hash_mismatch" {
		t.Fatalf("result: %+v", je.ResultJSON)
	}
	if len(xray.upserted)+len(xray.removed) != 0 {
		t.Fatalf("inconsistent response must not touch xray")
	}
	if a.state.Snapshot().SyncedUsersVersion != "" {
		t.Fatalf("inconsistent response must not persist state")
	}
}

func TestExecUsersSyncFullVersionedResponsePersistsVersionAndCleansStale(t *testing.T) {
	users := []UserSyncItem{{UserID: 1, Email: "alice@x", UUID: "u1", Status: "active"}}
	version := hashUsers(users)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema":1,"version":"` + version + `","users":[{"user_id":1,"email":"alice@x","uuid":"u1","status":"active"}]}`))
	}))
	defer srv.Close()

	xray := &fakeRuntimeClient{}
	a := newDeltaTestAgent(t, xray)
	a.panel = &PanelClient{baseURL: srv.URL, client: srv.Client()}
	// A previous failed attempt journaled a user that may exist in runtime.
	if err := a.state.Update(func(s *State) {
		s.PendingUsersSync = &PendingUsersSyncState{
			Mode: "full", TargetVersion: "older-attempt",
			TargetUsers: []UserSyncItem{{UserID: 7, Email: "ghost@x", UUID: "u7", Status: "active"}},
			CreatedAt:   "2026-01-01T00:00:00Z",
		}
	}); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	out, err := a.execUsersSync(context.Background(), "")
	if err != nil {
		t.Fatalf("full sync: %v", err)
	}
	if out.AppliedVersion != version {
		t.Fatalf("applied=%s want %s", out.AppliedVersion, version)
	}
	// ghost@x came from the stale journal and is not in the fetched target:
	// it must be removed from the runtime.
	found := false
	for _, r := range xray.removed {
		if r == "ghost@x" {
			found = true
		}
	}
	if !found {
		t.Fatalf("stale journaled user not removed: %+v", xray.removed)
	}
	snap := a.state.Snapshot()
	if snap.SyncedUsersVersion != version || snap.PendingUsersSync != nil {
		t.Fatalf("state after full: version=%s journal=%+v", snap.SyncedUsersVersion, snap.PendingUsersSync)
	}
}

func TestStateStoreUpdateIsMemoryAtomic(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	// The state path's parent is a regular file, so MkdirAll/save must fail.
	st := NewStateStore(filepath.Join(blocker, "state.json"))
	if err := st.Update(func(s *State) { s.SyncedUsersVersion = "v1" }); err == nil {
		t.Fatalf("expected save failure")
	}
	if got := st.Snapshot().SyncedUsersVersion; got != "" {
		t.Fatalf("failed save mutated in-memory state: %q", got)
	}
}

func TestEffectiveSyncedUsersMergesPendingJournal(t *testing.T) {
	s := State{
		SyncedUsers: []UserSyncItem{
			{UserID: 1, Email: "alice@x", Status: "active", UUID: "u1"},
		},
		PendingUsersSync: &PendingUsersSyncState{
			Mode: "full", TargetVersion: "t",
			TargetUsers: []UserSyncItem{
				{UserID: 1, Email: "alice@x", Status: "active", UUID: "u1-new"}, // dup email: committed wins
				{UserID: 2, Email: "bob@x", Status: "active", UUID: "u2"},
			},
			PossibleRuntimeUsers: []UserSyncItem{
				{UserID: 3, Email: "carol@x", Status: "active", UUID: "u3"},
				{UserID: 2, Email: "bob@x", Status: "active", UUID: "u2-old"},
			},
		},
	}
	got := effectiveSyncedUsers(s)
	if len(got) != 3 {
		t.Fatalf("expected 3 effective users, got %+v", got)
	}
	if got[0].UUID != "u1" {
		t.Fatalf("committed baseline must win on duplicate email: %+v", got[0])
	}
	// Without a journal the committed slice is returned as-is.
	s.PendingUsersSync = nil
	if len(effectiveSyncedUsers(s)) != 1 {
		t.Fatalf("expected committed users only")
	}
}

func assertNeedFullSync(t *testing.T, err error, reason string) {
	t.Helper()
	var je JobError
	if !errors.As(err, &je) {
		t.Fatalf("expected JobError, got %v", err)
	}
	if je.Retryable {
		t.Fatalf("need-full-sync failures must be non-retryable: %v", err)
	}
	res, ok := je.ResultJSON.(map[string]any)
	if !ok {
		t.Fatalf("result JSON missing: %+v", je)
	}
	if res["need_full_sync"] != true {
		t.Fatalf("need_full_sync not set: %+v", res)
	}
	if res["reason"] != reason {
		t.Fatalf("reason=%v want %s", res["reason"], reason)
	}
	if !strings.Contains(strings.ToLower(je.Msg), "full") {
		t.Fatalf("message should mention full repair: %q", je.Msg)
	}
}

// A delta that renames an existing user's email passes every hash check (the
// model is keyed by user_id) but would leave the old email live in Xray
// forever; the agent must reject it as a full-repair condition.
func TestExecUsersSyncDeltaRejectsEmailRename(t *testing.T) {
	xray := &fakeRuntimeClient{}
	a := newDeltaTestAgent(t, xray)
	base := seedBaseline(t, a, baselineUsers())

	targetItems := syncItems(baselineUsers())
	for i := range targetItems {
		if targetItems[i].UserID == 1 {
			targetItems[i].Email = "alice-renamed@x"
		}
	}
	payload := deltaPayloadJSON(t, usersync.JobPayload{
		Schema: usersync.SchemaV1, Mode: usersync.ModeDelta,
		BaseVersion: base, TargetVersion: usersync.HashItems(targetItems),
		Changes: []usersync.Change{
			{Action: usersync.ActionUpsert, User: usersync.Item{UserID: 1, Email: "alice-renamed@x", Status: "active", UUID: "u1"}},
		},
	})
	_, err := a.execUsersSync(context.Background(), payload)
	assertNeedFullSync(t, err, "delta_email_move")
	if len(xray.upserted)+len(xray.removed) != 0 {
		t.Fatalf("xray must not be touched on email rename")
	}
	if a.state.Snapshot().PendingUsersSync != nil {
		t.Fatalf("no journal may be written on email rename")
	}
}

// A delta moving one email between two user ids (upsert + remove sharing the
// email) can delete the user the same delta just added, depending on payload
// order. The agent must reject any email appearing in more than one change.
func TestExecUsersSyncDeltaRejectsCrossUserEmailMove(t *testing.T) {
	xray := &fakeRuntimeClient{}
	a := newDeltaTestAgent(t, xray)
	base := seedBaseline(t, a, baselineUsers())

	targetItems := make([]usersync.Item, 0, len(baselineUsers()))
	for _, it := range syncItems(baselineUsers()) {
		switch it.UserID {
		case 5:
			continue // erin's id removed...
		case 2:
			it.Email = "erin@x" // ...her email reused by bob
		}
		targetItems = append(targetItems, it)
	}
	// Adverse ordering: upsert first, remove second.
	payload := deltaPayloadJSON(t, usersync.JobPayload{
		Schema: usersync.SchemaV1, Mode: usersync.ModeDelta,
		BaseVersion: base, TargetVersion: usersync.HashItems(targetItems),
		Changes: []usersync.Change{
			{Action: usersync.ActionUpsert, User: usersync.Item{UserID: 2, Email: "erin@x", Status: "active", UUID: "u2"}},
			{Action: usersync.ActionRemove, User: usersync.Item{UserID: 5, Email: "erin@x", Status: "active", UUID: "u5"}},
		},
	})
	_, err := a.execUsersSync(context.Background(), payload)
	assertNeedFullSync(t, err, "delta_email_move")
	if len(xray.upserted)+len(xray.removed) != 0 {
		t.Fatalf("xray must not be touched on cross-user email move")
	}
}

// A re-delivered delta after a committed-but-unacknowledged success must
// succeed idempotently instead of forcing a full repair.
func TestExecUsersSyncDeltaIdempotentWhenAlreadyAtTarget(t *testing.T) {
	xray := &fakeRuntimeClient{}
	a := newDeltaTestAgent(t, xray)

	targetItems := make([]usersync.Item, 0, len(baselineUsers())+1)
	targetItems = append(targetItems, syncItems(baselineUsers())...)
	targetItems = append(targetItems, usersync.Item{UserID: 4, Email: "dave@x", Status: "active", UUID: "u4"})
	base := hashUsers(baselineUsers())
	target := usersync.HashItems(targetItems)

	// Baseline is already at the delta's *target* (previous attempt committed
	// but the finish report was lost).
	seedBaseline(t, a, fromSyncItems(targetItems))

	payload := deltaPayloadJSON(t, usersync.JobPayload{
		Schema: usersync.SchemaV1, Mode: usersync.ModeDelta,
		BaseVersion: base, TargetVersion: target,
		Changes: []usersync.Change{
			{Action: usersync.ActionUpsert, User: usersync.Item{UserID: 4, Email: "dave@x", Status: "active", UUID: "u4"}},
		},
	})
	out, err := a.execUsersSync(context.Background(), payload)
	if err != nil {
		t.Fatalf("replayed delta must succeed idempotently: %v", err)
	}
	if out.AppliedVersion != target {
		t.Fatalf("applied=%s want %s", out.AppliedVersion, target)
	}
	if len(xray.upserted)+len(xray.removed) != 0 {
		t.Fatalf("idempotent replay must not touch xray")
	}
}

type fakeUptimeRuntimeClient struct {
	fakeRuntimeClient
	uptimes  []uint32
	probeIx  int
	probeErr error
}

func (f *fakeUptimeRuntimeClient) SysUptime(_ context.Context) (uint32, bool, error) {
	if f.probeErr != nil {
		return 0, false, f.probeErr
	}
	if f.probeIx >= len(f.uptimes) {
		return f.uptimes[len(f.uptimes)-1], true, nil
	}
	u := f.uptimes[f.probeIx]
	f.probeIx++
	return u, true, nil
}

// The runtime guard must retry a failed startup restore until it succeeds
// (host reboot: xray's API is not up inside the first attempt's window).
func TestXrayRuntimeGuardRetriesFailedRestore(t *testing.T) {
	restoreErr := errors.New("xray api not ready")
	xray := &fakeUptimeRuntimeClient{uptimes: []uint32{10}}
	xray.upsertErrFor = map[string]error{"alice@x": restoreErr}
	a := newDeltaTestAgent(t, xray)
	seedBaseline(t, a, []UserSyncItem{{UserID: 1, Email: "alice@x", Status: "active", UUID: "u1"}})

	g := &xrayRuntimeGuard{agent: a}
	g.prober, g.canProbe = a.xray.(UptimeProber)
	ctx := context.Background()

	g.attempt(ctx, "startup")
	if g.restored {
		t.Fatalf("restore must report failure while xray errors")
	}
	// Xray comes up; the next tick retries and succeeds.
	xray.upsertErrFor = nil
	g.tick(ctx)
	if !g.restored {
		t.Fatalf("tick must retry the failed restore")
	}
	found := false
	for _, u := range xray.upserted {
		if u == "alice@x=u1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("retry must re-add the committed baseline: %v", xray.upserted)
	}
}

// An uptime regression means xray restarted and lost every hot-added user;
// the guard must re-add the committed baseline.
func TestXrayRuntimeGuardReaddsUsersAfterRestart(t *testing.T) {
	xray := &fakeUptimeRuntimeClient{uptimes: []uint32{100, 200, 5, 20}}
	a := newDeltaTestAgent(t, xray)
	seedBaseline(t, a, []UserSyncItem{{UserID: 1, Email: "alice@x", Status: "active", UUID: "u1"}})

	g := &xrayRuntimeGuard{agent: a}
	g.prober, g.canProbe = a.xray.(UptimeProber)
	ctx := context.Background()

	g.attempt(ctx, "startup")
	if !g.restored {
		t.Fatalf("startup restore failed")
	}
	baselineAdds := len(xray.upserted)

	g.tick(ctx) // uptime 100: baseline reading
	g.tick(ctx) // uptime 200: monotonic, no action
	if len(xray.upserted) != baselineAdds {
		t.Fatalf("monotonic uptime must not trigger re-adds")
	}
	g.tick(ctx) // uptime 5 < 200: restart detected
	if len(xray.upserted) <= baselineAdds {
		t.Fatalf("uptime regression must re-add synced users")
	}
	if !g.restored {
		t.Fatalf("re-add after restart must mark restored")
	}
}

// Probe errors prove nothing: the guard must keep its last reading and not
// thrash re-adds while the API is briefly unreachable.
func TestXrayRuntimeGuardIgnoresProbeErrors(t *testing.T) {
	xray := &fakeUptimeRuntimeClient{uptimes: []uint32{100}}
	a := newDeltaTestAgent(t, xray)
	seedBaseline(t, a, []UserSyncItem{{UserID: 1, Email: "alice@x", Status: "active", UUID: "u1"}})

	g := &xrayRuntimeGuard{agent: a}
	g.prober, g.canProbe = a.xray.(UptimeProber)
	ctx := context.Background()

	g.attempt(ctx, "startup")
	g.tick(ctx) // uptime 100
	adds := len(xray.upserted)
	xray.probeErr = errors.New("api down")
	g.tick(ctx)
	g.tick(ctx)
	if len(xray.upserted) != adds {
		t.Fatalf("probe errors must not trigger re-adds")
	}
	if !g.haveUptime || g.lastUptime != 100 {
		t.Fatalf("guard must keep its last good reading: have=%v last=%d", g.haveUptime, g.lastUptime)
	}
}
