package repo

import (
	"context"
	"testing"
	"time"
)

func TestPruneTerminalNodeJobsKeepsRecentAndNonTerminal(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "prune-jobs-node",
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
	old := now.AddDate(0, 0, -10).Format(time.RFC3339)
	recent := now.AddDate(0, 0, -1).Format(time.RFC3339)
	insert := func(status, createdAt string, finishedAt any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `
INSERT INTO node_jobs(node_id, kind, status, created_at, finished_at)
VALUES (?, 'users_sync', ?, ?, ?);
`, node.ID, status, createdAt, finishedAt); err != nil {
			t.Fatalf("insert %s job: %v", status, err)
		}
	}
	insert("succeeded", old, old)    // prunable
	insert("failed", old, old)       // prunable
	insert("canceled", old, nil)     // prunable via created_at fallback
	insert("succeeded", old, recent) // finished recently: kept
	insert("pending", old, nil)      // non-terminal: kept even though old
	insert("running", old, nil)      // non-terminal: kept even though old

	deleted, err := s.PruneTerminalNodeJobs(ctx, now.AddDate(0, 0, -7), 2)
	if err != nil {
		t.Fatalf("prune node jobs: %v", err)
	}
	if deleted != 3 {
		t.Fatalf("expected 3 deleted, got %d", deleted)
	}
	var remaining int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_jobs WHERE node_id = ?;`, node.ID).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 3 {
		t.Fatalf("expected 3 remaining jobs, got %d", remaining)
	}
	var nonTerminal int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM node_jobs WHERE node_id = ? AND status IN ('pending','running');
`, node.ID).Scan(&nonTerminal); err != nil {
		t.Fatalf("count non-terminal: %v", err)
	}
	if nonTerminal != 2 {
		t.Fatalf("pending/running jobs must survive pruning, got %d", nonTerminal)
	}
}

func TestPruneQuotaWindowsDeletesOnlyLongEndedWindows(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newActiveUser(t, s)

	now := time.Now().UTC()
	insert := func(start, end time.Time) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `
INSERT INTO quota_windows(user_id, window_start, window_end, cycle_type, cycle_key)
VALUES (?, ?, ?, 'month', ?);
`, u.ID, start.Format(time.RFC3339), end.Format(time.RFC3339), start.Format("2006-01")); err != nil {
			t.Fatalf("insert quota window: %v", err)
		}
	}
	insert(now.AddDate(0, 0, -130), now.AddDate(0, 0, -100)) // prunable
	insert(now.AddDate(0, 0, -125), now.AddDate(0, 0, -95))  // prunable
	insert(now.AddDate(0, 0, -40), now.AddDate(0, 0, -10))   // recent past: kept
	insert(now.AddDate(0, 0, -10), now.AddDate(0, 0, 20))    // active window: kept
	// CreateUser also seeds the current cycle window (ends in the future).

	deleted, err := s.PruneQuotaWindows(ctx, now.AddDate(0, 0, -90), 1)
	if err != nil {
		t.Fatalf("prune quota windows: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("expected 2 deleted, got %d", deleted)
	}
	var remaining int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM quota_windows WHERE user_id = ?;`, u.ID).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 3 {
		t.Fatalf("expected 3 remaining windows, got %d", remaining)
	}
	var active int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM quota_windows WHERE user_id = ? AND window_end > ?;
`, u.ID, now.Format(time.RFC3339)).Scan(&active); err != nil {
		t.Fatalf("count active: %v", err)
	}
	if active != 2 {
		t.Fatalf("active windows must survive pruning, got %d", active)
	}
}

func TestDeleteExpiredAdminSessions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	now := time.Now().UTC()
	insert := func(id string, expiresAt time.Time) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `
INSERT INTO admin_sessions(session_id, username, expires_at, created_at, last_seen_at)
VALUES (?, 'admin', ?, ?, ?);
`, id, expiresAt.Format(time.RFC3339), now.AddDate(0, 0, -2).Format(time.RFC3339), now.Format(time.RFC3339)); err != nil {
			t.Fatalf("insert session %s: %v", id, err)
		}
	}
	insert("expired-session", now.Add(-time.Hour))
	insert("live-session", now.Add(time.Hour))

	deleted, err := s.DeleteExpiredAdminSessions(ctx, now)
	if err != nil {
		t.Fatalf("delete expired sessions: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 deleted session, got %d", deleted)
	}
	var live int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_sessions WHERE session_id = 'live-session';`).Scan(&live); err != nil {
		t.Fatalf("count live sessions: %v", err)
	}
	if live != 1 {
		t.Fatalf("live session must survive, got %d", live)
	}
}
