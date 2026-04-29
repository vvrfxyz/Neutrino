package repo

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestEnsureAdminCredentialDoesNotOverwriteExistingAdmin(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.UpsertAdminCredential(ctx, "first-admin", "first-pass"); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if err := s.EnsureAdminCredential(ctx, "second-admin", "second-pass"); err != nil {
		t.Fatalf("ensure admin: %v", err)
	}

	if err := s.AuthenticateAdmin(ctx, "first-admin", "first-pass"); err != nil {
		t.Fatalf("original admin should still work: %v", err)
	}
	if err := s.AuthenticateAdmin(ctx, "second-admin", "second-pass"); err == nil {
		t.Fatalf("ensure should not replace existing admin")
	}
}

func TestFinishNodeJobForNodeClearsTerminalMetadataOnRequeue(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "job-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	jobID, _, err := s.EnqueueNodeJob(ctx, node.ID, "users_sync", "v1", `{"users":[]}`, 30, "corr-1")
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	claimed, ok, err := s.ClaimNextNodeJobForNode(ctx, node.ID)
	if err != nil || !ok {
		t.Fatalf("claim job ok=%v err=%v", ok, err)
	}

	httpStatus := 502
	final, err := s.FinishNodeJobForNode(ctx, node.ID, jobID, FinishNodeJobInput{
		Status:      "failed",
		Retryable:   true,
		HTTPStatus:  &httpStatus,
		ResultJSON:  `{"detail":"bad gateway"}`,
		ErrorMsg:    "transient failure",
		MaxAttempts: 5,
		Attempt:     claimed.Attempts,
	})
	if err != nil {
		t.Fatalf("finish job: %v", err)
	}
	if final != "pending" {
		t.Fatalf("unexpected final status: %s", final)
	}

	job, ok, err := s.GetNodeJob(ctx, jobID)
	if err != nil || !ok {
		t.Fatalf("get job ok=%v err=%v", ok, err)
	}
	if job.Status != "pending" {
		t.Fatalf("unexpected job status: %s", job.Status)
	}
	if job.HTTPStatus != nil {
		t.Fatalf("http status should be cleared: %+v", *job.HTTPStatus)
	}
	if job.ResultJSON != "" {
		t.Fatalf("result json should be cleared: %q", job.ResultJSON)
	}
	if job.LastError != "" {
		t.Fatalf("last error should be cleared: %q", job.LastError)
	}
	if job.FinishedAt != nil {
		t.Fatalf("finished_at should be cleared: %+v", job.FinishedAt)
	}

	claimed, ok, err = s.ClaimNextNodeJobForNode(ctx, node.ID)
	if err != nil || !ok {
		t.Fatalf("reclaim job ok=%v err=%v", ok, err)
	}
	if claimed.Status != "running" {
		t.Fatalf("unexpected claimed status: %s", claimed.Status)
	}
	if claimed.HTTPStatus != nil {
		t.Fatalf("claimed http status should stay cleared")
	}
	if claimed.ResultJSON != "" {
		t.Fatalf("claimed result json should stay cleared: %q", claimed.ResultJSON)
	}
	if claimed.LastError != "" {
		t.Fatalf("claimed last error should stay cleared: %q", claimed.LastError)
	}
	if claimed.FinishedAt != nil {
		t.Fatalf("claimed finished_at should stay cleared: %+v", claimed.FinishedAt)
	}
}

func TestEnqueueNodeJobResetsRetryStateWhenReplacingPendingPayload(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "replace-pending-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	jobID, _, err := s.EnqueueNodeJob(ctx, node.ID, "users_sync", "v1", `{"version":1}`, 30, "old")
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	oldTime := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := s.RawDB().ExecContext(ctx, `
UPDATE node_jobs
SET attempts = 4,
	retryable = 0,
	http_status = 503,
	result_json = '{"old":true}',
	last_error = 'old failure',
	started_at = ?,
	finished_at = ?
WHERE id = ?;
`, oldTime, oldTime, jobID); err != nil {
		t.Fatalf("seed old retry state: %v", err)
	}

	gotID, enqueued, err := s.EnqueueNodeJob(ctx, node.ID, "users_sync", "v2", `{"version":2}`, 45, "new")
	if err != nil {
		t.Fatalf("replace pending job: %v", err)
	}
	if gotID != jobID {
		t.Fatalf("expected pending job %d to be reused, got %d", jobID, gotID)
	}
	if enqueued {
		t.Fatalf("expected replacing pending job to report enqueued=false")
	}

	job, ok, err := s.GetNodeJob(ctx, jobID)
	if err != nil || !ok {
		t.Fatalf("get job ok=%v err=%v", ok, err)
	}
	if job.DesiredVersion != "v2" || job.PayloadJSON != `{"version":2}` {
		t.Fatalf("job payload not replaced: version=%q payload=%q", job.DesiredVersion, job.PayloadJSON)
	}
	if job.Attempts != 0 {
		t.Fatalf("attempts=%d, want 0", job.Attempts)
	}
	if !job.Retryable {
		t.Fatalf("retryable=false, want true")
	}
	if job.HTTPStatus != nil || job.ResultJSON != "" || job.LastError != "" || job.StartedAt != nil || job.FinishedAt != nil {
		t.Fatalf("old execution metadata was not cleared: %+v", job)
	}
}

func TestFinishNodeJobForNodeRejectsStaleAttempt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "stale-finish-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	jobID, _, err := s.EnqueueNodeJob(ctx, node.ID, "xray_apply", "v1", `{}`, 30, "stale")
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	first, ok, err := s.ClaimNextNodeJobForNode(ctx, node.ID)
	if err != nil || !ok {
		t.Fatalf("claim first attempt ok=%v err=%v", ok, err)
	}
	if first.Attempts != 1 {
		t.Fatalf("first claim attempt=%d, want 1", first.Attempts)
	}
	oldStartedAt := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	if _, err := s.RawDB().ExecContext(ctx, `UPDATE node_jobs SET started_at = ? WHERE id = ?;`, oldStartedAt, jobID); err != nil {
		t.Fatalf("age running job: %v", err)
	}

	affected, err := s.SweepTimedOutRunningJobs(ctx, map[string]time.Duration{"xray_apply": time.Second}, 5)
	if err != nil {
		t.Fatalf("sweep timed out job: %v", err)
	}
	if affected != 1 {
		t.Fatalf("affected=%d, want 1", affected)
	}

	_, err = s.FinishNodeJobForNode(ctx, node.ID, jobID, FinishNodeJobInput{
		Status:      "succeeded",
		MaxAttempts: 5,
		Attempt:     first.Attempts,
	})
	if !errors.Is(err, ErrNodeJobNotRunning) {
		t.Fatalf("expected stale pending finish to fail with ErrNodeJobNotRunning, got %v", err)
	}
	job, ok, err := s.GetNodeJob(ctx, jobID)
	if err != nil || !ok {
		t.Fatalf("get job ok=%v err=%v", ok, err)
	}
	if job.Status != "pending" {
		t.Fatalf("stale finish changed job status to %s", job.Status)
	}

	second, ok, err := s.ClaimNextNodeJobForNode(ctx, node.ID)
	if err != nil || !ok {
		t.Fatalf("claim second attempt ok=%v err=%v", ok, err)
	}
	if second.Attempts != 2 {
		t.Fatalf("second claim attempt=%d, want 2", second.Attempts)
	}

	_, err = s.FinishNodeJobForNode(ctx, node.ID, jobID, FinishNodeJobInput{
		Status:      "succeeded",
		MaxAttempts: 5,
		Attempt:     first.Attempts,
	})
	if !errors.Is(err, ErrNodeJobNotRunning) {
		t.Fatalf("expected stale running finish to fail with ErrNodeJobNotRunning, got %v", err)
	}
	job, ok, err = s.GetNodeJob(ctx, jobID)
	if err != nil || !ok {
		t.Fatalf("get job after stale finish ok=%v err=%v", ok, err)
	}
	if job.Status != "running" || job.Attempts != second.Attempts {
		t.Fatalf("stale attempt should leave second attempt running, got %+v", job)
	}

	final, err := s.FinishNodeJobForNode(ctx, node.ID, jobID, FinishNodeJobInput{
		Status:      "succeeded",
		MaxAttempts: 5,
		Attempt:     second.Attempts,
	})
	if err != nil {
		t.Fatalf("finish current attempt: %v", err)
	}
	if final != "succeeded" {
		t.Fatalf("final=%s, want succeeded", final)
	}
}

func TestSweepTimedOutRunningJobsClearsTerminalMetadataOnRequeue(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "timeout-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	now := time.Now().UTC()
	startedAt := now.Add(-2 * time.Minute).Format(time.RFC3339)
	finishedAt := now.Add(-time.Minute).Format(time.RFC3339)
	resultJSON := `{"stale":true}`
	lastError := "stale"
	if _, err := s.RawDB().ExecContext(ctx, `
INSERT INTO node_jobs(node_id, kind, desired_version, payload_json, status, attempts, retryable, http_status, timeout_sec, correlation_id, last_error, result_json, created_at, started_at, finished_at)
VALUES (?, 'users_sync', 'v1', '{}', 'running', 1, 1, 504, 30, 'corr-timeout', ?, ?, ?, ?, ?);
`, node.ID, lastError, resultJSON, now.Add(-3*time.Minute).Format(time.RFC3339), startedAt, finishedAt); err != nil {
		t.Fatalf("insert running job: %v", err)
	}

	affected, err := s.SweepTimedOutRunningJobs(ctx, map[string]time.Duration{"users_sync": time.Second}, 5)
	if err != nil {
		t.Fatalf("sweep timed out jobs: %v", err)
	}
	if affected != 1 {
		t.Fatalf("unexpected affected count: %d", affected)
	}

	var status string
	var httpStatus sql.NullInt64
	var result sql.NullString
	var errMsg sql.NullString
	var finished sql.NullString
	if err := s.RawDB().QueryRowContext(ctx, `
SELECT status, http_status, result_json, last_error, finished_at
FROM node_jobs
WHERE node_id = ? AND kind = 'users_sync'
LIMIT 1;
`, node.ID).Scan(&status, &httpStatus, &result, &errMsg, &finished); err != nil {
		t.Fatalf("query swept job: %v", err)
	}
	if status != "pending" {
		t.Fatalf("unexpected status after sweep: %s", status)
	}
	if httpStatus.Valid {
		t.Fatalf("http status should be cleared after requeue")
	}
	if result.Valid && result.String != "" {
		t.Fatalf("result json should be cleared: %q", result.String)
	}
	if errMsg.Valid && errMsg.String != "" {
		t.Fatalf("last error should be cleared: %q", errMsg.String)
	}
	if finished.Valid && finished.String != "" {
		t.Fatalf("finished_at should be cleared: %q", finished.String)
	}
}

func TestMarkNodeXrayRolledBackClearsDesiredState(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "rollback-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
		Managed:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, _, err := s.DeployManagedXray(ctx, node.ID, `{"template":"{}"}`, "desired-v1", 120, "xray_apply"); err != nil {
		t.Fatalf("deploy managed xray: %v", err)
	}
	if _, ok, err := s.GetNodeDesiredState(ctx, node.ID); err != nil || !ok {
		t.Fatalf("expected desired state before rollback ok=%v err=%v", ok, err)
	}

	if err := s.MarkNodeXrayRolledBack(ctx, node.ID, "rollback:config.json.bak.20260425T120000Z"); err != nil {
		t.Fatalf("mark rollback: %v", err)
	}

	after, err := s.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("get node after rollback: %v", err)
	}
	if after.DesiredXrayVersion != "" {
		t.Fatalf("desired xray version should be cleared, got %q", after.DesiredXrayVersion)
	}
	if after.AppliedXrayVersion != "rollback:config.json.bak.20260425T120000Z" {
		t.Fatalf("unexpected applied xray version: %q", after.AppliedXrayVersion)
	}
	if _, ok, err := s.GetNodeDesiredState(ctx, node.ID); err != nil || ok {
		t.Fatalf("desired state should be removed after rollback ok=%v err=%v", ok, err)
	}
}

func TestSweepTimedOutRunningJobsAtomic(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "atomic-sweep-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	now := time.Now().UTC()
	startedAt := now.Add(-2 * time.Minute).Format(time.RFC3339)
	if _, err := s.RawDB().ExecContext(ctx, `
INSERT INTO node_jobs(node_id, kind, desired_version, payload_json, status, attempts, retryable, timeout_sec, correlation_id, created_at, started_at)
VALUES (?, 'users_sync', 'v1', '{}', 'running', 1, 1, 30, 'corr-atomic', ?, ?);
`, node.ID, now.Add(-3*time.Minute).Format(time.RFC3339), startedAt); err != nil {
		t.Fatalf("insert running job: %v", err)
	}

	affected, err := s.SweepTimedOutRunningJobs(ctx, map[string]time.Duration{"users_sync": time.Second}, 5)
	if err != nil {
		t.Fatalf("sweep timed out jobs: %v", err)
	}
	if affected != 1 {
		t.Fatalf("unexpected affected count: %d", affected)
	}

	var status string
	if err := s.RawDB().QueryRowContext(ctx, `SELECT status FROM node_jobs WHERE node_id = ? AND kind = 'users_sync' LIMIT 1;`, node.ID).Scan(&status); err != nil {
		t.Fatalf("query job status: %v", err)
	}
	if status != "pending" {
		t.Fatalf("unexpected job status after sweep: %s", status)
	}

	var lastErrorKind sql.NullString
	var lastError sql.NullString
	if err := s.RawDB().QueryRowContext(ctx, `SELECT last_job_error_kind, last_error FROM nodes WHERE id = ?;`, node.ID).Scan(&lastErrorKind, &lastError); err != nil {
		t.Fatalf("query node error state: %v", err)
	}
	if !lastErrorKind.Valid || lastErrorKind.String != "retryable" {
		t.Fatalf("expected nodes.last_job_error_kind=retryable, got %+v", lastErrorKind)
	}
	if !lastError.Valid || lastError.String != "timeout" {
		t.Fatalf("expected nodes.last_error=timeout, got %+v", lastError)
	}
}

type claimResult struct {
	ok  bool
	err error
}

func TestClaimNextNodeJobForNode_ParallelClaimsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "parallel-claim-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	if _, _, err := s.EnqueueNodeJob(ctx, node.ID, "users_sync", "v1", `{"users":[]}`, 30, "parallel"); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}

	const workers = 20
	results := make(chan claimResult, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, ok, cerr := s.ClaimNextNodeJobForNode(ctx, node.ID)
			results <- claimResult{ok: ok, err: cerr}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	okCount := 0
	for r := range results {
		if r.err != nil {
			t.Fatalf("unexpected claim error: %v", r.err)
		}
		if r.ok {
			okCount++
		}
	}
	if okCount != 1 {
		t.Fatalf("expected exactly one successful claim, got %d", okCount)
	}
}

func TestClaimNextNodeJobForNodeKindsFiltersPendingJobs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "filtered-claim-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	xrayJobID, _, err := s.EnqueueNodeJob(ctx, node.ID, "xray_apply", "xray-v1", `{}`, 30, "xray")
	if err != nil {
		t.Fatalf("enqueue xray job: %v", err)
	}
	usersJobID, _, err := s.EnqueueNodeJob(ctx, node.ID, "users_sync", "users-v1", `{}`, 30, "users")
	if err != nil {
		t.Fatalf("enqueue users job: %v", err)
	}

	claimed, ok, err := s.ClaimNextNodeJobForNodeKinds(ctx, node.ID, []string{"users_sync"})
	if err != nil || !ok {
		t.Fatalf("claim filtered job ok=%v err=%v", ok, err)
	}
	if claimed.ID != usersJobID || claimed.Kind != "users_sync" {
		t.Fatalf("expected users_sync claim, got %+v", claimed)
	}

	xrayJob, ok, err := s.GetNodeJob(ctx, xrayJobID)
	if err != nil || !ok {
		t.Fatalf("get xray job ok=%v err=%v", ok, err)
	}
	if xrayJob.Status != "pending" {
		t.Fatalf("expected xray job to remain pending, got %+v", xrayJob)
	}
}
