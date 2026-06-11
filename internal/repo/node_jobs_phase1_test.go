package repo

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestEnqueueNodeJobParallelEnqueuesNeverDoubleInsert(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "parallel-enqueue-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	const workers = 20
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, err := s.EnqueueNodeJob(ctx, node.ID, "users_sync", "v1", `{"users":[]}`, 30, "")
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("enqueue error: %v", err)
		}
	}

	var pending int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM node_jobs WHERE node_id = ? AND kind = 'users_sync' AND status = 'pending';
`, node.ID).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 1 {
		t.Fatalf("expected exactly one pending job after %d concurrent enqueues, got %d", workers, pending)
	}
}

func TestFinishNodeJobForNodeRejectsNonTerminalStatus(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "finish-whitelist-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, _, err := s.EnqueueNodeJob(ctx, node.ID, "users_sync", "v1", `{"users":[]}`, 30, ""); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	job, ok, err := s.ClaimNextNodeJobForNode(ctx, node.ID)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	for _, status := range []string{"pending", "running", "canceled", "garbage"} {
		_, err := s.FinishNodeJobForNode(ctx, node.ID, job.ID, FinishNodeJobInput{
			Status:  status,
			Attempt: job.Attempts,
		})
		if !errors.Is(err, ErrNodeJobInvalidStatus) {
			t.Fatalf("status %q: expected ErrNodeJobInvalidStatus, got %v", status, err)
		}
	}

	// The job must still be finishable with a terminal status afterwards.
	final, err := s.FinishNodeJobForNode(ctx, node.ID, job.ID, FinishNodeJobInput{
		Status:  "succeeded",
		Attempt: job.Attempts,
	})
	if err != nil {
		t.Fatalf("finish succeeded: %v", err)
	}
	if final != "succeeded" {
		t.Fatalf("final status = %q, want succeeded", final)
	}
}

func TestPruneUsageEventKeysDeletesOnlyOldKeys(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	now := time.Now().UTC()
	insert := func(id string, createdAt time.Time) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `
INSERT INTO usage_event_keys(source, source_event_id, user_id, node_id, event_at, created_at)
VALUES ('test', ?, 1, NULL, ?, ?);
`, id, createdAt.Format(time.RFC3339), createdAt.Format(time.RFC3339)); err != nil {
			t.Fatalf("insert key %s: %v", id, err)
		}
	}
	insert("old-1", now.AddDate(0, 0, -30))
	insert("old-2", now.AddDate(0, 0, -15))
	insert("fresh-1", now.AddDate(0, 0, -1))
	insert("fresh-2", now)

	deleted, err := s.PruneUsageEventKeys(ctx, now.AddDate(0, 0, -14), 1)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}

	var remaining int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_event_keys WHERE source = 'test';`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 2 {
		t.Fatalf("remaining = %d, want 2", remaining)
	}
}
