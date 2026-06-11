package repo

import (
	"context"
	"time"
)

// pruneBatched executes a batched DELETE statement repeatedly until fewer than
// batchSize rows were deleted. The query must accept (cutoff, batchSize) args
// and delete at most batchSize rows per execution so each statement holds the
// write lock briefly.
func (s *Store) pruneBatched(ctx context.Context, q string, cutoff string, batchSize int) (int64, error) {
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, q, cutoff, batchSize)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < int64(batchSize) {
			break
		}
	}
	return total, nil
}

// PruneTerminalNodeJobs deletes terminal node jobs (succeeded/failed/canceled)
// whose finish time (falling back to created_at) is before olderThan. Pending
// and running jobs are never touched.
func (s *Store) PruneTerminalNodeJobs(ctx context.Context, olderThan time.Time, batchSize int) (int64, error) {
	if olderThan.IsZero() {
		return 0, nil
	}
	if batchSize <= 0 || batchSize > 5000 {
		batchSize = 1000
	}
	q := `
DELETE FROM node_jobs
WHERE id IN (
	SELECT id
	FROM node_jobs
	WHERE status IN ('succeeded','failed','canceled')
	  AND COALESCE(finished_at, created_at) < ?
	ORDER BY id
	LIMIT ?
);
`
	return s.pruneBatched(ctx, q, olderThan.UTC().Format(time.RFC3339), batchSize)
}

// PruneAuditLogs deletes audit log entries created before olderThan.
func (s *Store) PruneAuditLogs(ctx context.Context, olderThan time.Time, batchSize int) (int64, error) {
	if olderThan.IsZero() {
		return 0, nil
	}
	if batchSize <= 0 || batchSize > 5000 {
		batchSize = 1000
	}
	q := `
DELETE FROM audit_logs
WHERE id IN (
	SELECT id
	FROM audit_logs
	WHERE created_at < ?
	ORDER BY id
	LIMIT ?
);
`
	return s.pruneBatched(ctx, q, olderThan.UTC().Format(time.RFC3339), batchSize)
}

// PruneSentAlerts deletes notification alerts that were already dispatched
// (sent_at set) before olderThan. The alerts table has no status column —
// "sent" is the terminal state; pending alerts (sent_at IS NULL) are kept so
// the dispatcher can still deliver them. Dedupe keys are cycle-/minute-scoped,
// so removing old sent rows cannot resurrect a current-cycle duplicate.
func (s *Store) PruneSentAlerts(ctx context.Context, olderThan time.Time, batchSize int) (int64, error) {
	if olderThan.IsZero() {
		return 0, nil
	}
	if batchSize <= 0 || batchSize > 5000 {
		batchSize = 1000
	}
	q := `
DELETE FROM alerts
WHERE id IN (
	SELECT id
	FROM alerts
	WHERE sent_at IS NOT NULL AND sent_at < ?
	ORDER BY id
	LIMIT ?
);
`
	return s.pruneBatched(ctx, q, olderThan.UTC().Format(time.RFC3339), batchSize)
}

// PruneEnforcementLogs deletes enforcement log entries created before olderThan.
func (s *Store) PruneEnforcementLogs(ctx context.Context, olderThan time.Time, batchSize int) (int64, error) {
	if olderThan.IsZero() {
		return 0, nil
	}
	if batchSize <= 0 || batchSize > 5000 {
		batchSize = 1000
	}
	q := `
DELETE FROM enforcement_logs
WHERE id IN (
	SELECT id
	FROM enforcement_logs
	WHERE created_at < ?
	ORDER BY id
	LIMIT ?
);
`
	return s.pruneBatched(ctx, q, olderThan.UTC().Format(time.RFC3339), batchSize)
}

// PruneQuotaWindows deletes quota windows whose window_end is before
// olderThan. The retention must be generous (well past the longest cycle —
// one month) so the active window and anything enforcement/alerting still
// reads are never deleted.
func (s *Store) PruneQuotaWindows(ctx context.Context, olderThan time.Time, batchSize int) (int64, error) {
	if olderThan.IsZero() {
		return 0, nil
	}
	if batchSize <= 0 || batchSize > 5000 {
		batchSize = 1000
	}
	q := `
DELETE FROM quota_windows
WHERE id IN (
	SELECT id
	FROM quota_windows
	WHERE window_end < ?
	ORDER BY id
	LIMIT ?
);
`
	return s.pruneBatched(ctx, q, olderThan.UTC().Format(time.RFC3339), batchSize)
}

// DeleteExpiredAdminSessions removes admin sessions whose expires_at is in
// the past. The table is small (one row per live login), so no batching.
func (s *Store) DeleteExpiredAdminSessions(ctx context.Context, now time.Time) (int64, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at < ?;`, now.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
