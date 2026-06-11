package repo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"neutrino/internal/usersync"
)

func newSyncNode(t *testing.T, s *Store, name string) Node {
	t.Helper()
	n, err := s.CreateNode(context.Background(), CreateNodeInput{
		Name:     name,
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	return n
}

func claimUsersSync(t *testing.T, s *Store, nodeID int64) NodeJob {
	t.Helper()
	job, ok, err := s.ClaimNextNodeJobForNode(context.Background(), nodeID)
	if err != nil || !ok {
		t.Fatalf("claim users_sync ok=%v err=%v", ok, err)
	}
	if job.Kind != "users_sync" {
		t.Fatalf("claimed %s, want users_sync", job.Kind)
	}
	return job
}

func finishUsersSyncOK(t *testing.T, s *Store, nodeID int64, job NodeJob, applied string) FinishUsersSyncResult {
	t.Helper()
	result, _ := json.Marshal(map[string]any{
		"mode":           "full",
		"target_version": applied,
		"synced":         1,
		"removed":        0,
		"failed":         0,
	})
	res, err := s.FinishUsersSyncJobForNode(context.Background(), nodeID, job.ID, FinishNodeJobInput{
		Status:      "succeeded",
		MaxAttempts: 5,
		Attempt:     job.Attempts,
		ResultJSON:  string(result),
	}, applied)
	if err != nil {
		t.Fatalf("finish users_sync: %v", err)
	}
	return res
}

func pendingUsersSyncPayload(t *testing.T, s *Store, nodeID int64) (usersync.JobPayload, bool) {
	t.Helper()
	job, has, err := getPendingUsersSyncJobExec(context.Background(), s.db, nodeID)
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}
	if !has {
		return usersync.JobPayload{}, false
	}
	p, class := classifyUsersSyncPayload(job.PayloadJSON)
	if class == usersSyncPayloadInvalid {
		t.Fatalf("pending payload invalid: %s", job.PayloadJSON)
	}
	return p, true
}

// Full bootstrap: first prepare sends schema-1 full; an accepted schema-1
// result sets the delta-ready marker; the next no-drift prepare is a no-op.
func TestPrepareUsersSyncFullBootstrapAndDeltaReadyMarker(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node := newSyncNode(t, s, "sync-bootstrap")
	newActiveUser(t, s)

	res, err := s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !res.Enqueued || res.Mode != "full" || res.TargetVersion == "" {
		t.Fatalf("first prepare: %+v", res)
	}
	payload, has := pendingUsersSyncPayload(t, s, node.ID)
	if !has || payload.Schema != 1 || payload.Mode != "full" || payload.TargetVersion != res.TargetVersion {
		t.Fatalf("pending payload: %+v has=%v", payload, has)
	}
	if _, ok, err := s.GetNodeUserSyncSnapshot(ctx, node.ID, res.TargetVersion); err != nil || !ok {
		t.Fatalf("target snapshot missing ok=%v err=%v", ok, err)
	}

	job := claimUsersSync(t, s, node.ID)
	fin := finishUsersSyncOK(t, s, node.ID, job, res.TargetVersion)
	if !fin.Accepted || !fin.DeltaReadyMarked {
		t.Fatalf("finish: %+v", fin)
	}
	n, err := s.GetNode(ctx, node.ID)
	if err != nil || n.AppliedUsersVersion != res.TargetVersion {
		t.Fatalf("applied=%s want %s err=%v", n.AppliedUsersVersion, res.TargetVersion, err)
	}
	rollout, err := s.GetNodeUsersSyncRollout(ctx, node.ID)
	if err != nil || rollout.BaselineSchema != 1 || rollout.DeltaReadyAt == nil {
		t.Fatalf("rollout: %+v err=%v", rollout, err)
	}

	// No drift: prepare must not enqueue anything.
	res2, err := s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	if err != nil {
		t.Fatalf("second prepare: %v", err)
	}
	if res2.Enqueued || res2.Mode != "" {
		t.Fatalf("no-drift prepare enqueued a job: %+v", res2)
	}
	if _, has := pendingUsersSyncPayload(t, s, node.ID); has {
		t.Fatalf("no pending job expected after no-drift prepare")
	}
}

// After a proven baseline, a small user change generates a delta job whose
// applied success keeps the marker; before the marker, the same change
// generates full.
func TestPrepareUsersSyncDeltaModeSelection(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node := newSyncNode(t, s, "sync-delta")
	for i := 0; i < 5; i++ {
		newActiveUser(t, s)
	}

	// Bootstrap to delta-ready.
	res, err := s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	job := claimUsersSync(t, s, node.ID)
	finishUsersSyncOK(t, s, node.ID, job, res.TargetVersion)

	// One added user → delta.
	newActiveUser(t, s)
	res2, err := s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	if err != nil {
		t.Fatalf("prepare delta: %v", err)
	}
	if !res2.Enqueued || res2.Mode != "delta" {
		t.Fatalf("expected delta, got %+v", res2)
	}
	payload, _ := pendingUsersSyncPayload(t, s, node.ID)
	if payload.Mode != "delta" || payload.BaseVersion != res.TargetVersion || payload.TargetVersion != res2.TargetVersion {
		t.Fatalf("delta payload: %+v", payload)
	}
	if len(payload.Changes) != 1 || payload.Changes[0].Action != "upsert" {
		t.Fatalf("delta changes: %+v", payload.Changes)
	}

	// Delta success: accepted via delta-mode result, marker kept.
	dJob := claimUsersSync(t, s, node.ID)
	result, _ := json.Marshal(map[string]any{
		"mode": "delta", "base_version": payload.BaseVersion, "target_version": res2.TargetVersion,
		"synced": 1, "removed": 0, "failed": 0,
	})
	fin, err := s.FinishUsersSyncJobForNode(ctx, node.ID, dJob.ID, FinishNodeJobInput{
		Status: "succeeded", MaxAttempts: 5, Attempt: dJob.Attempts, ResultJSON: string(result),
	}, res2.TargetVersion)
	if err != nil || !fin.Accepted {
		t.Fatalf("delta finish: %+v err=%v", fin, err)
	}
	rollout, _ := s.GetNodeUsersSyncRollout(ctx, node.ID)
	if rollout.BaselineSchema != 1 {
		t.Fatalf("delta success must keep the marker, got %+v", rollout)
	}
}

func TestPrepareUsersSyncRequiresBaselineSchemaForDelta(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node := newSyncNode(t, s, "sync-no-marker")
	for i := 0; i < 5; i++ {
		newActiveUser(t, s)
	}

	// Bootstrap, but finish with a LEGACY result (no mode/target fields):
	// the version is accepted yet the marker must stay unset.
	res, err := s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	job := claimUsersSync(t, s, node.ID)
	fin, err := s.FinishUsersSyncJobForNode(ctx, node.ID, job.ID, FinishNodeJobInput{
		Status: "succeeded", MaxAttempts: 5, Attempt: job.Attempts,
		ResultJSON: `{"synced":5,"removed":0,"failed":0}`,
	}, res.TargetVersion)
	if err != nil || !fin.Accepted || fin.DeltaReadyMarked {
		t.Fatalf("legacy finish: %+v err=%v", fin, err)
	}
	if fin.RepairEnqueued {
		t.Fatalf("legacy success on a schema-0 node must not loop another forced full")
	}
	rollout, _ := s.GetNodeUsersSyncRollout(ctx, node.ID)
	if rollout.BaselineSchema != 0 {
		t.Fatalf("marker must stay 0 for legacy agents, got %+v", rollout)
	}

	// Matching desired/applied hashes alone must not enable delta.
	newActiveUser(t, s)
	res2, err := s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	if err != nil {
		t.Fatalf("prepare 2: %v", err)
	}
	if res2.Mode != "full" {
		t.Fatalf("schema-0 node must get full, got %+v", res2)
	}
}

func TestFinishLegacyResultOnDeltaReadyNodeClearsMarker(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node := newSyncNode(t, s, "sync-drift")
	newActiveUser(t, s)

	res, _ := s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	job := claimUsersSync(t, s, node.ID)
	finishUsersSyncOK(t, s, node.ID, job, res.TargetVersion) // delta-ready now

	// Force another full and answer with a legacy result: capability drift.
	if _, err := s.PrepareUsersSync(ctx, node.ID, true, "repair"); err != nil {
		t.Fatalf("forced prepare: %v", err)
	}
	job2 := claimUsersSync(t, s, node.ID)
	fin, err := s.FinishUsersSyncJobForNode(ctx, node.ID, job2.ID, FinishNodeJobInput{
		Status: "succeeded", MaxAttempts: 5, Attempt: job2.Attempts,
		ResultJSON: `{"synced":1,"removed":0,"failed":0}`,
	}, res.TargetVersion)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if !fin.Accepted || !fin.DeltaReadyCleared || !fin.RepairEnqueued {
		t.Fatalf("capability drift not handled: %+v", fin)
	}
	rollout, _ := s.GetNodeUsersSyncRollout(ctx, node.ID)
	if rollout.BaselineSchema != 0 || rollout.DeltaReadyAt != nil {
		t.Fatalf("marker not cleared: %+v", rollout)
	}
	payload, has := pendingUsersSyncPayload(t, s, node.ID)
	if !has || payload.Mode != "full" || payload.Reason != "baseline_backfill" {
		t.Fatalf("expected baseline_backfill repair, got %+v has=%v", payload, has)
	}
}

func TestFinishVersionMismatchEnqueuesRepairWithoutAccepting(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node := newSyncNode(t, s, "sync-mismatch")
	newActiveUser(t, s)

	res, _ := s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	job := claimUsersSync(t, s, node.ID)
	fin, err := s.FinishUsersSyncJobForNode(ctx, node.ID, job.ID, FinishNodeJobInput{
		Status: "succeeded", MaxAttempts: 5, Attempt: job.Attempts,
		ResultJSON: `{"mode":"full","target_version":"bogus"}`,
	}, "bogus-version")
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if fin.Accepted || !fin.RepairEnqueued {
		t.Fatalf("mismatched applied must not be accepted: %+v", fin)
	}
	n, _ := s.GetNode(ctx, node.ID)
	if n.AppliedUsersVersion != "" {
		t.Fatalf("applied_users_version must stay unset, got %s", n.AppliedUsersVersion)
	}
	payload, has := pendingUsersSyncPayload(t, s, node.ID)
	if !has || payload.Reason != "finish_version_mismatch" || payload.Mode != "full" {
		t.Fatalf("repair payload: %+v has=%v", payload, has)
	}
	_ = res
}

func TestFinishEmptyAppliedVersionEnqueuesRepair(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node := newSyncNode(t, s, "sync-empty-applied")
	newActiveUser(t, s)

	_, _ = s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	job := claimUsersSync(t, s, node.ID)
	fin, err := s.FinishUsersSyncJobForNode(ctx, node.ID, job.ID, FinishNodeJobInput{
		Status: "succeeded", MaxAttempts: 5, Attempt: job.Attempts,
	}, "  ")
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if fin.Accepted || !fin.RepairEnqueued {
		t.Fatalf("empty applied version must enqueue repair: %+v", fin)
	}
}

func TestFinishNeedFullSyncEnqueuesForcedFull(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node := newSyncNode(t, s, "sync-needfull")
	newActiveUser(t, s)

	_, _ = s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	job := claimUsersSync(t, s, node.ID)
	fin, err := s.FinishUsersSyncJobForNode(ctx, node.ID, job.ID, FinishNodeJobInput{
		Status: "failed", Retryable: false, MaxAttempts: 5, Attempt: job.Attempts,
		ResultJSON: `{"mode":"delta","need_full_sync":true,"reason":"base_version_mismatch"}`,
		ErrorMsg:   "base mismatch",
	}, "")
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if fin.FinalStatus != "failed" || !fin.RepairEnqueued {
		t.Fatalf("finish: %+v", fin)
	}
	payload, has := pendingUsersSyncPayload(t, s, node.ID)
	if !has || payload.Mode != "full" || payload.Reason != "base_version_mismatch" {
		t.Fatalf("repair payload: %+v has=%v", payload, has)
	}
}

func TestFinishRetryableFailureDoesNotEnqueueRepair(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node := newSyncNode(t, s, "sync-retry")
	newActiveUser(t, s)

	_, _ = s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	job := claimUsersSync(t, s, node.ID)
	fin, err := s.FinishUsersSyncJobForNode(ctx, node.ID, job.ID, FinishNodeJobInput{
		Status: "failed", Retryable: true, MaxAttempts: 5, Attempt: job.Attempts,
		ErrorMsg: "xray briefly down",
	}, "")
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if fin.FinalStatus != "pending" || fin.RepairEnqueued {
		t.Fatalf("retryable failure must requeue without repair: %+v", fin)
	}
}

// Forced full must never be satisfied by a running delta: it leaves a pending
// full repair that is the next claimable job.
func TestForcedFullNotSwallowedByRunningDelta(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node := newSyncNode(t, s, "sync-forced")
	for i := 0; i < 5; i++ {
		newActiveUser(t, s)
	}

	res, _ := s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	job := claimUsersSync(t, s, node.ID)
	finishUsersSyncOK(t, s, node.ID, job, res.TargetVersion) // delta-ready

	newActiveUser(t, s)
	res2, _ := s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	if res2.Mode != "delta" {
		t.Fatalf("expected delta, got %+v", res2)
	}
	running := claimUsersSync(t, s, node.ID) // delta is now running

	// Forced full while the delta runs.
	forced, err := s.PrepareUsersSync(ctx, node.ID, true, "xray_apply_followup")
	if err != nil {
		t.Fatalf("forced prepare: %v", err)
	}
	payload, has := pendingUsersSyncPayload(t, s, node.ID)
	if !has || payload.Mode != "full" || payload.Reason != "xray_apply_followup" {
		t.Fatalf("forced full must wait pending behind the running delta: %+v has=%v", payload, has)
	}

	// Even if the running delta succeeds at the same version, the protected
	// pending full must survive redundant-job cleanup.
	result, _ := json.Marshal(map[string]any{
		"mode": "delta", "base_version": res.TargetVersion, "target_version": res2.TargetVersion,
	})
	fin, err := s.FinishUsersSyncJobForNode(ctx, node.ID, running.ID, FinishNodeJobInput{
		Status: "succeeded", MaxAttempts: 5, Attempt: running.Attempts, ResultJSON: string(result),
	}, res2.TargetVersion)
	if err != nil || !fin.Accepted {
		t.Fatalf("finish running delta: %+v err=%v", fin, err)
	}
	payload, has = pendingUsersSyncPayload(t, s, node.ID)
	if !has || payload.Reason != "xray_apply_followup" {
		t.Fatalf("protected repair was canceled: %+v has=%v", payload, has)
	}
	next := claimUsersSync(t, s, node.ID)
	np, _ := classifyUsersSyncPayload(next.PayloadJSON)
	if np.Mode != "full" || np.Reason != "xray_apply_followup" {
		t.Fatalf("next claim must be the forced full repair, got %+v", np)
	}
	_ = forced
}

// A later delta never displaces a pending full; a later full displaces a
// pending delta.
func TestUsersSyncEnqueuePriorityFullOverDelta(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node := newSyncNode(t, s, "sync-priority")
	for i := 0; i < 5; i++ {
		newActiveUser(t, s)
	}

	res, _ := s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	job := claimUsersSync(t, s, node.ID)
	finishUsersSyncOK(t, s, node.ID, job, res.TargetVersion) // delta-ready

	// Pending delta...
	newActiveUser(t, s)
	res2, _ := s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	if res2.Mode != "delta" {
		t.Fatalf("expected delta, got %+v", res2)
	}
	// ...displaced by a forced full...
	if _, err := s.PrepareUsersSync(ctx, node.ID, true, "repair"); err != nil {
		t.Fatalf("forced prepare: %v", err)
	}
	payload, _ := pendingUsersSyncPayload(t, s, node.ID)
	if payload.Mode != "full" {
		t.Fatalf("forced full must displace pending delta: %+v", payload)
	}
	// ...and a follow-up ordinary prepare (which would pick delta) must NOT
	// displace the pending full.
	newActiveUser(t, s)
	res3, err := s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	if err != nil {
		t.Fatalf("prepare 3: %v", err)
	}
	payload, _ = pendingUsersSyncPayload(t, s, node.ID)
	if payload.Mode != "full" {
		t.Fatalf("delta displaced a pending full: %+v", payload)
	}
	_ = res3
}

func TestUsersSyncStaleSuccessEnqueuesFollowUp(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node := newSyncNode(t, s, "sync-stale-success")
	newActiveUser(t, s)

	res, _ := s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	job := claimUsersSync(t, s, node.ID)

	// Users change while the job runs; desired moves to v2.
	newActiveUser(t, s)
	res2, _ := s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	if res2.TargetVersion == res.TargetVersion {
		t.Fatalf("expected desired version to move")
	}

	// The agent reports the captured (v1) version: accepted, but a follow-up
	// must converge to v2.
	fin := finishUsersSyncOK(t, s, node.ID, job, res.TargetVersion)
	if !fin.Accepted || !fin.FollowUpEnqueued {
		t.Fatalf("stale success: %+v", fin)
	}
	n, _ := s.GetNode(ctx, node.ID)
	if n.AppliedUsersVersion != res.TargetVersion {
		t.Fatalf("applied=%s want %s", n.AppliedUsersVersion, res.TargetVersion)
	}
	payload, has := pendingUsersSyncPayload(t, s, node.ID)
	if !has || payload.TargetVersion != res2.TargetVersion {
		t.Fatalf("follow-up payload: %+v has=%v", payload, has)
	}
}

func TestFailedBaselineBackfillClearsBackfillMarker(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node := newSyncNode(t, s, "sync-backfill-fail")
	newActiveUser(t, s)

	if _, err := s.PrepareUsersSync(ctx, node.ID, true, "baseline_backfill"); err != nil {
		t.Fatalf("backfill prepare: %v", err)
	}
	if err := s.SetNodeUsersSyncBackfillAt(ctx, node.ID, time.Now()); err != nil {
		t.Fatalf("set backfill at: %v", err)
	}
	job := claimUsersSync(t, s, node.ID)
	fin, err := s.FinishUsersSyncJobForNode(ctx, node.ID, job.ID, FinishNodeJobInput{
		Status: "failed", Retryable: false, MaxAttempts: 5, Attempt: job.Attempts,
		ErrorMsg: "agent exploded",
	}, "")
	if err != nil || fin.FinalStatus != "failed" {
		t.Fatalf("finish: %+v err=%v", fin, err)
	}
	rollout, _ := s.GetNodeUsersSyncRollout(ctx, node.ID)
	if rollout.BackfillAt != nil {
		t.Fatalf("failed backfill must clear the marker so startup retries: %+v", rollout)
	}
}

func TestListNodesNeedingUsersSyncBackfill(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	n1 := newSyncNode(t, s, "bf-1")
	n2 := newSyncNode(t, s, "bf-2")
	if err := s.SetNodeEnabled(ctx, n2.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	ids, err := s.ListNodesNeedingUsersSyncBackfill(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ids) != 1 || ids[0] != n1.ID {
		t.Fatalf("ids=%v want [%d]", ids, n1.ID)
	}
	if err := s.SetNodeUsersSyncBackfillAt(ctx, n1.ID, time.Now()); err != nil {
		t.Fatalf("mark: %v", err)
	}
	ids, _ = s.ListNodesNeedingUsersSyncBackfill(ctx)
	if len(ids) != 0 {
		t.Fatalf("marked node still listed: %v", ids)
	}
}

func TestPruneNodeUserSyncSnapshotsRetention(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node := newSyncNode(t, s, "sync-prune")

	// 15 snapshots, oldest first.
	for i := 0; i < 15; i++ {
		version := strings.Repeat("0", 3) + string(rune('a'+i))
		created := time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC).Format(time.RFC3339)
		if err := upsertNodeUserSyncSnapshotExec(ctx, s.db, node.ID, version, []usersync.Item{}, created); err != nil {
			t.Fatalf("seed snapshot %d: %v", i, err)
		}
	}
	// Mark the two oldest as desired/applied: they must survive pruning.
	if _, err := s.db.ExecContext(ctx, `UPDATE nodes SET desired_users_version='000a', applied_users_version='000b' WHERE id=?;`, node.ID); err != nil {
		t.Fatalf("mark versions: %v", err)
	}
	if err := s.PruneNodeUserSyncSnapshots(ctx, node.ID); err != nil {
		t.Fatalf("prune: %v", err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM node_user_sync_snapshots WHERE node_id=?;`, node.ID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	// newest 10 + the two referenced old ones.
	if count != 12 {
		t.Fatalf("count=%d want 12", count)
	}
	for _, version := range []string{"000a", "000b"} {
		if _, ok, _ := s.GetNodeUserSyncSnapshot(ctx, node.ID, version); !ok {
			t.Fatalf("referenced snapshot %s was pruned", version)
		}
	}
}

func TestPrepareUsersSyncDisabledNodeConvergesToEmptyOnce(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node := newSyncNode(t, s, "sync-disabled")
	newActiveUser(t, s)

	if err := s.SetNodeEnabled(ctx, node.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	res, err := s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !res.Enqueued || res.Mode != "full" {
		t.Fatalf("disabled node cleanup: %+v", res)
	}
	if res.TargetVersion != usersync.HashItems(nil) {
		t.Fatalf("disabled node target must be the empty-list hash")
	}
	job := claimUsersSync(t, s, node.ID)
	finishUsersSyncOK(t, s, node.ID, job, res.TargetVersion)

	// Once the empty target is applied, no repeated cleanup job.
	res2, err := s.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	if err != nil {
		t.Fatalf("prepare 2: %v", err)
	}
	if res2.Enqueued {
		t.Fatalf("disabled node re-enqueued cleanup: %+v", res2)
	}
}
