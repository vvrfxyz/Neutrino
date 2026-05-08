package repo

import (
	"context"
	"testing"
	"time"
)

func TestListNodeJobSummariesBatchesPendingAndRunningState(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	nodeA, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "summary-node-a",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "a.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node a: %v", err)
	}
	nodeB, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "summary-node-b",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "b.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node b: %v", err)
	}

	if _, _, err := s.EnqueueNodeJob(ctx, nodeA.ID, "users_sync", "users-a", `{}`, 30, "pending-a-1"); err != nil {
		t.Fatalf("enqueue node a users: %v", err)
	}
	if _, _, err := s.EnqueueNodeJob(ctx, nodeA.ID, "xray_apply", "xray-a", `{"template":"{}"}`, 30, "pending-a-2"); err != nil {
		t.Fatalf("enqueue node a xray: %v", err)
	}
	jobB, _, err := s.EnqueueNodeJob(ctx, nodeB.ID, "users_sync", "users-b", `{}`, 30, "running-b")
	if err != nil {
		t.Fatalf("enqueue node b: %v", err)
	}
	if _, ok, err := s.ClaimNextNodeJobForNode(ctx, nodeB.ID); err != nil || !ok {
		t.Fatalf("claim node b ok=%v err=%v", ok, err)
	}

	startedAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := s.RawDB().ExecContext(ctx, `
INSERT INTO node_jobs(node_id, kind, desired_version, payload_json, status, attempts, retryable, timeout_sec, correlation_id, created_at, started_at)
VALUES (?, 'xray_rollback', 'rollback-a', '{}', 'running', 1, 1, 30, 'running-a', ?, ?);
`, nodeA.ID, startedAt, startedAt); err != nil {
		t.Fatalf("insert running node a: %v", err)
	}

	summaries, err := s.ListNodeJobSummaries(ctx)
	if err != nil {
		t.Fatalf("list summaries: %v", err)
	}

	a := summaries[nodeA.ID]
	if a.Pending != 2 {
		t.Fatalf("node a pending=%d, want 2", a.Pending)
	}
	if a.RunningKind != "xray_rollback" || a.RunningDesired != "rollback-a" || a.RunningCorrelation != "running-a" {
		t.Fatalf("unexpected node a running summary: %+v", a)
	}
	if a.RunningStartedAt != startedAt {
		t.Fatalf("node a running started=%q, want %q", a.RunningStartedAt, startedAt)
	}

	b := summaries[nodeB.ID]
	if b.Pending != 0 {
		t.Fatalf("node b pending=%d, want 0", b.Pending)
	}
	if b.RunningKind != "users_sync" || b.RunningDesired != "users-b" || b.RunningCorrelation != "running-b" {
		t.Fatalf("unexpected node b running summary for job %d: %+v", jobB, b)
	}
}
