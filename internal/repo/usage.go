package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	usageMaxFutureSkew     = 5 * time.Minute
	usageMaxActiveBackdate = 26 * time.Hour
)

func (s *Store) reactivateOverLimitUserForNewCycleTx(ctx context.Context, tx *sql.Tx, userID int64, now time.Time) (bool, error) {
	now = now.UTC()
	nowStr := now.Format(time.RFC3339)

	res, err := tx.ExecContext(ctx, `
UPDATE users
SET status = 'active', removed_at = NULL
WHERE id = ? AND status = 'over_limit' AND expires_at > ?;
`, userID, nowStr)
	if err != nil {
		return false, err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed == 0 {
		return false, nil
	}

	if err := s.activateLatestProxyLinkTx(ctx, tx, userID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
	INSERT INTO enforcement_logs(user_id, action, reason, detail, created_at)
	VALUES (?, 'enable_user', 'quota_cycle_reset', 'auto re-enabled at new quota window', ?);
	`, userID, nowStr); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) RecordUsage(ctx context.Context, in UsageInput) (User, error) {
	var err error
	in, err = normalizeUsageInput(in)
	if err != nil {
		return User{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()

	if exists, err := s.usageEventExistsTx(ctx, tx, in.Source, in.SourceEventID); err != nil {
		return User{}, err
	} else if exists {
		_ = tx.Rollback()
		return s.GetUser(ctx, in.UserID)
	}

	mode, status, limitBytes, quotaCycle, quotaTZ, expiresAt, removedAt, err := s.loadUserUsageMetaTx(ctx, tx, in.UserID)
	if err != nil {
		return User{}, err
	}
	if in.NodeID != nil && *in.NodeID > 0 {
		if err := s.ensureUserAllowedOnNodeTx(ctx, tx, in.UserID, *in.NodeID); err != nil {
			return User{}, err
		}
	}
	now := time.Now().UTC()
	if err := validateUsageWriteTimestamp(in.At, now, status == "active"); err != nil {
		return User{}, err
	}
	if !usageAllowedForUserState(status, in.At, expiresAt, removedAt) {
		return User{}, ErrUserInactive
	}
	if err := s.applyUsageEventTx(ctx, tx, in, mode, limitBytes, quotaCycle, quotaTZ); err != nil {
		if errors.Is(err, ErrDuplicateEvent) {
			_ = tx.Rollback()
			return s.GetUser(ctx, in.UserID)
		}
		return User{}, err
	}

	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return s.GetUser(ctx, in.UserID)
}

func (s *Store) SweepExpiredUsers(ctx context.Context) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
SELECT id
FROM users
WHERE status = 'active' AND expires_at < ?;
`, now)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	ids := make([]int64, 0, 8)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, userID := range ids {
		if _, err := tx.ExecContext(ctx, `
UPDATE users
SET status = 'expired', removed_at = ?
WHERE id = ? AND status = 'active';
`, now, userID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE proxy_links SET active = 0 WHERE user_id = ?`, userID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO enforcement_logs(user_id, action, reason, detail, created_at)
VALUES (?, 'disable_user', 'expired', 'auto sweep expired user', ?);
`, userID, now); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func normalizeUsageInput(in UsageInput) (UsageInput, error) {
	if in.At.IsZero() {
		in.At = time.Now().UTC()
	}
	in.At = in.At.UTC()
	in.Source = strings.TrimSpace(in.Source)
	if in.Source == "" {
		in.Source = "manual"
	}
	in.SourceEventID = strings.TrimSpace(in.SourceEventID)
	if in.SourceEventID == "" {
		return UsageInput{}, fmt.Errorf("source_event_id is required")
	}
	if in.Direction != "inbound" && in.Direction != "outbound" {
		return UsageInput{}, fmt.Errorf("invalid direction: %s", in.Direction)
	}
	if in.Bytes < 0 {
		return UsageInput{}, fmt.Errorf("invalid bytes: %d", in.Bytes)
	}
	return in, nil
}

func (s *Store) loadUserUsageMetaTx(ctx context.Context, tx *sql.Tx, userID int64) (mode, status string, limitBytes int64, quotaCycle, quotaTZ string, expiresAt time.Time, removedAt *time.Time, err error) {
	var expiresAtRaw string
	var removedAtRaw sql.NullString
	if err = tx.QueryRowContext(ctx, `
SELECT expires_at, removed_at, counting_mode, status, monthly_limit_bytes, quota_cycle, quota_tz
FROM users
WHERE id = ?;
`, userID).Scan(&expiresAtRaw, &removedAtRaw, &mode, &status, &limitBytes, &quotaCycle, &quotaTZ); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrUserNotFound
		}
		return "", "", 0, "", "", time.Time{}, nil, err
	}
	expiresAt, err = time.Parse(time.RFC3339, expiresAtRaw)
	if err != nil {
		return "", "", 0, "", "", time.Time{}, nil, err
	}
	if removedAtRaw.Valid && strings.TrimSpace(removedAtRaw.String) != "" {
		parsed, parseErr := time.Parse(time.RFC3339, removedAtRaw.String)
		if parseErr != nil {
			return "", "", 0, "", "", time.Time{}, nil, parseErr
		}
		removedAt = &parsed
	}
	if quotaCycle != "day" && quotaCycle != "week" && quotaCycle != "month" {
		quotaCycle = "month"
	}
	if strings.TrimSpace(quotaTZ) == "" {
		quotaTZ = "Asia/Shanghai"
	}
	return mode, status, limitBytes, quotaCycle, quotaTZ, expiresAt, removedAt, nil
}

func validateUsageWriteTimestamp(eventAt, now time.Time, activeUser bool) error {
	eventAt = eventAt.UTC()
	now = now.UTC()
	if eventAt.After(now.Add(usageMaxFutureSkew)) {
		return fmt.Errorf("%w: event_at is too far in the future", ErrUsageTimestampSkew)
	}
	if activeUser && eventAt.Before(now.Add(-usageMaxActiveBackdate)) {
		return fmt.Errorf("%w: %w: event_at is too far in the past", ErrUsageTimestampSkew, ErrUsageTimestampTooOld)
	}
	return nil
}

func usageAllowedForUserState(status string, eventAt, expiresAt time.Time, removedAt *time.Time) bool {
	eventAt = eventAt.UTC()
	expiresAt = expiresAt.UTC()
	if status == "active" {
		return !eventAt.After(expiresAt)
	}
	if status != "expired" && removedAt == nil {
		return false
	}
	cutoff := expiresAt
	if removedAt != nil && removedAt.UTC().Before(cutoff) {
		cutoff = removedAt.UTC()
	}
	return !eventAt.After(cutoff)
}

func (s *Store) ensureUserAllowedOnNodeTx(ctx context.Context, tx *sql.Tx, userID, nodeID int64) error {
	if nodeID <= 0 {
		return nil
	}
	var allowed int
	if err := tx.QueryRowContext(ctx, `
SELECT CASE
	WHEN EXISTS (SELECT 1 FROM user_node_access WHERE user_id = ?)
	THEN CASE
		WHEN EXISTS (SELECT 1 FROM user_node_access WHERE user_id = ? AND node_id = ?) THEN 1
		ELSE 0
	END
	ELSE 1
END;
`, userID, userID, nodeID).Scan(&allowed); err != nil {
		return err
	}
	if allowed == 0 {
		return ErrUserNotAllowedOnNode
	}
	return nil
}

func (s *Store) usageEventExistsTx(ctx context.Context, tx *sql.Tx, source, sourceEventID string) (bool, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
SELECT 1
FROM usage_event_keys
WHERE source = ? AND source_event_id = ?
LIMIT 1;
`, source, sourceEventID).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return id > 0, nil
}

func (s *Store) reserveUsageEventKeyTx(ctx context.Context, tx *sql.Tx, in UsageInput) error {
	nodeID := sql.NullInt64{}
	if in.NodeID != nil && *in.NodeID > 0 {
		nodeID = sql.NullInt64{Int64: *in.NodeID, Valid: true}
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO usage_event_keys(source, source_event_id, user_id, node_id, event_at, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(source, source_event_id) DO NOTHING;
`, in.Source, in.SourceEventID, in.UserID, nodeID, in.At.Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return ErrDuplicateEvent
	}
	return nil
}

// PruneUsageEventKeys deletes dedupe keys recorded before olderThan (keyed on
// created_at = ingest time, not event time). Retention must comfortably exceed
// the longest plausible agent replay window — the 26h active-user backdate cap
// plus however long a disk-queued batch can wait before flushing — or replayed
// events would be double-counted.
func (s *Store) PruneUsageEventKeys(ctx context.Context, olderThan time.Time, batchSize int) (int64, error) {
	if olderThan.IsZero() {
		return 0, nil
	}
	if batchSize <= 0 || batchSize > 5000 {
		batchSize = 1000
	}
	cutoff := olderThan.UTC().Format(time.RFC3339)
	q := `
DELETE FROM usage_event_keys
WHERE rowid IN (
	SELECT rowid
	FROM usage_event_keys
	WHERE created_at < ?
	LIMIT ?
);
`
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, q, cutoff, batchSize)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < int64(batchSize) {
			return total, nil
		}
	}
}

func (s *Store) ensureQuotaWindowTx(ctx context.Context, tx *sql.Tx, userID int64, cycleType, quotaTZ string, eventAt time.Time) (time.Time, time.Time, bool, error) {
	var existingStart, existingEnd string
	err := tx.QueryRowContext(ctx, `
	SELECT window_start, window_end
	FROM quota_windows
	WHERE user_id = ? AND window_start <= ? AND window_end > ?
	ORDER BY window_start DESC
	LIMIT 1;
	`, userID, eventAt.UTC().Format(time.RFC3339), eventAt.UTC().Format(time.RFC3339)).Scan(&existingStart, &existingEnd)
	if err == nil {
		parsed, parseErr := time.Parse(time.RFC3339, existingStart)
		parsedEnd, parseEndErr := time.Parse(time.RFC3339, existingEnd)
		if parseErr == nil && parseEndErr == nil {
			return parsed, parsedEnd, false, nil
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, time.Time{}, false, err
	}

	windowStart, windowEnd, cycleKey := quotaWindowBounds(cycleType, quotaTZ, eventAt)
	res, err := tx.ExecContext(ctx, `
INSERT INTO quota_windows(user_id, window_start, window_end, cycle_type, cycle_key, reset_reason, inbound_bytes, outbound_bytes, effective_bytes, credit_bytes)
VALUES (?, ?, ?, ?, ?, 'auto_cycle', 0, 0, 0, 0)
	ON CONFLICT(user_id, window_start) DO NOTHING;
	`, userID, windowStart.Format(time.RFC3339), windowEnd.Format(time.RFC3339), cycleType, cycleKey)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
	UPDATE quota_windows
	SET closed_at = COALESCE(closed_at, ?)
	WHERE user_id = ? AND window_end <= ?;
	`, eventAt.UTC().Format(time.RFC3339), userID, windowStart.Format(time.RFC3339)); err != nil {
		return time.Time{}, time.Time{}, false, err
	}

	_ = res
	return windowStart, windowEnd, true, nil
}

func (s *Store) applyUsageEventTx(ctx context.Context, tx *sql.Tx, in UsageInput, mode string, limitBytes int64, cycleType, quotaTZ string) error {
	windowStart, windowEnd, _, err := s.ensureQuotaWindowTx(ctx, tx, in.UserID, cycleType, quotaTZ, in.At)
	if err != nil {
		return err
	}
	if err := s.reserveUsageEventKeyTx(ctx, tx, in); err != nil {
		return err
	}

	// source_event_id is required by schema; normalizeUsageInput enforces this.
	sourceEventID := in.SourceEventID
	nodeID := sql.NullInt64{}
	if in.NodeID != nil && *in.NodeID > 0 {
		nodeID = sql.NullInt64{Int64: *in.NodeID, Valid: true}
	}
	targetHost := nullableString(in.TargetHost)
	targetIP := nullableString(in.TargetIP)
	targetPort := sql.NullInt64{}
	if in.TargetPort > 0 {
		targetPort = sql.NullInt64{Int64: in.TargetPort, Valid: true}
	}
	sni := nullableString(in.SNI)
	destination := nullableString(in.Destination)
	clientIP := nullableString(in.ClientIP)
	inboundTag := nullableString(in.InboundTag)

	res, err := tx.ExecContext(ctx, `
INSERT INTO traffic_events(
	user_id, node_id, direction, bytes, event_at, source, source_event_id,
	target_host, target_ip, target_port, sni, destination, client_ip, inbound_tag
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(source, source_event_id) DO NOTHING;
`, in.UserID, nodeID, in.Direction, in.Bytes, in.At.Format(time.RFC3339), in.Source, sourceEventID,
		targetHost, targetIP, targetPort, sni, destination, clientIP, inboundTag)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		// Deduplicated: do not double-count stats/quota/online.
		return ErrDuplicateEvent
	}

	if in.Source == "xray-stats" {
		// Keep charts stable even if we prune raw xray-stats events.
		if err := s.recordTrafficRollupHourlyTx(ctx, tx, in); err != nil {
			return err
		}
	}

	if err := s.updateTrafficStats(ctx, tx, in); err != nil {
		return err
	}
	if err := s.updateQuotaWindow(ctx, tx, in.UserID, windowStart, in.Direction, in.Bytes, mode); err != nil {
		return err
	}
	now := time.Now().UTC()
	if !now.Before(windowStart) && now.Before(windowEnd) {
		if err := s.enforceLimit(ctx, tx, in.UserID, windowStart, limitBytes); err != nil {
			return err
		}
		if err := s.queueQuotaAlertsTx(ctx, tx, in.UserID, windowStart, limitBytes); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) recordTrafficRollupHourlyTx(ctx context.Context, tx *sql.Tx, in UsageInput) error {
	if in.UserID <= 0 {
		return nil
	}
	if in.Direction != "inbound" && in.Direction != "outbound" {
		return nil
	}
	if in.NodeID == nil || *in.NodeID <= 0 {
		// xray-stats is expected to include node_id; skip to avoid polluting rollups.
		return nil
	}
	nodeID := *in.NodeID

	bucket := in.At.UTC().Truncate(time.Hour)
	bucketStart := bucket.Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)

	var inInc, outInc int64
	if in.Direction == "inbound" {
		inInc = in.Bytes
	} else {
		outInc = in.Bytes
	}

	_, err := tx.ExecContext(ctx, `
INSERT INTO traffic_rollups_hourly(user_id, node_id, bucket_start, inbound_bytes, outbound_bytes, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(user_id, node_id, bucket_start) DO UPDATE SET
	inbound_bytes = traffic_rollups_hourly.inbound_bytes + excluded.inbound_bytes,
	outbound_bytes = traffic_rollups_hourly.outbound_bytes + excluded.outbound_bytes,
	updated_at = excluded.updated_at;
`, in.UserID, nodeID, bucketStart, inInc, outInc, now)
	return err
}

func (s *Store) updateTrafficStats(ctx context.Context, tx *sql.Tx, in UsageInput) error {
	if _, err := tx.ExecContext(ctx, `
INSERT INTO traffic_stats(user_id, inbound_bytes, outbound_bytes)
VALUES (?, 0, 0)
ON CONFLICT(user_id) DO NOTHING;
`, in.UserID); err != nil {
		return err
	}

	var lastSeenRaw sql.NullString
	if err := tx.QueryRowContext(ctx, `
SELECT last_seen_at
FROM traffic_stats
WHERE user_id = ?;
`, in.UserID).Scan(&lastSeenRaw); err != nil {
		return err
	}

	updateLastSeen := true
	if lastSeenRaw.Valid && strings.TrimSpace(lastSeenRaw.String) != "" {
		if lastSeen, err := time.Parse(time.RFC3339, lastSeenRaw.String); err == nil {
			updateLastSeen = !in.At.Before(lastSeen)
		}
	}
	updateLastSeenInt := 0
	if updateLastSeen {
		updateLastSeenInt = 1
	}
	targetHost := nullableString(in.TargetHost)
	targetIP := nullableString(in.TargetIP)
	targetPort := nullableInt64(in.TargetPort)
	targetPresentInt := 0
	if targetHost.Valid || targetIP.Valid || targetPort.Valid {
		targetPresentInt = 1
	}
	eventAt := in.At.Format(time.RFC3339)

	var inboundDelta, outboundDelta int64
	switch in.Direction {
	case "inbound":
		inboundDelta = in.Bytes
	case "outbound":
		outboundDelta = in.Bytes
	default:
		return fmt.Errorf("invalid direction: %s", in.Direction)
	}

	_, err := tx.ExecContext(ctx, `
UPDATE traffic_stats
SET inbound_bytes = inbound_bytes + ?,
	outbound_bytes = outbound_bytes + ?,
	last_seen_at = CASE WHEN ? THEN ? ELSE last_seen_at END,
	last_target_host = CASE WHEN ? AND ? THEN ? ELSE last_target_host END,
	last_target_ip = CASE WHEN ? AND ? THEN ? ELSE last_target_ip END,
	last_target_port = CASE WHEN ? AND ? THEN ? ELSE last_target_port END
WHERE user_id = ?;
`, inboundDelta, outboundDelta,
		updateLastSeenInt, eventAt,
		updateLastSeenInt, targetPresentInt, targetHost,
		updateLastSeenInt, targetPresentInt, targetIP,
		updateLastSeenInt, targetPresentInt, targetPort,
		in.UserID)
	return err
}

func (s *Store) updateQuotaWindow(ctx context.Context, tx *sql.Tx, userID int64, winStart time.Time, direction string, bytes int64, mode string) error {
	switch direction {
	case "inbound":
		if _, err := tx.ExecContext(ctx, `
UPDATE quota_windows
SET inbound_bytes = inbound_bytes + ?
WHERE user_id = ? AND window_start = ?;
`, bytes, userID, winStart.Format(time.RFC3339)); err != nil {
			return err
		}
	case "outbound":
		if _, err := tx.ExecContext(ctx, `
UPDATE quota_windows
SET outbound_bytes = outbound_bytes + ?
WHERE user_id = ? AND window_start = ?;
`, bytes, userID, winStart.Format(time.RFC3339)); err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid direction: %s", direction)
	}

	var inbound, outbound int64
	if err := tx.QueryRowContext(ctx, `
SELECT inbound_bytes, outbound_bytes
FROM quota_windows
WHERE user_id = ? AND window_start = ?;
`, userID, winStart.Format(time.RFC3339)).Scan(&inbound, &outbound); err != nil {
		return err
	}

	// counting_mode=single: only count outbound traffic
	// counting_mode=double: count inbound + outbound
	var effective int64
	if mode == "double" {
		effective = inbound + outbound
	} else {
		// single mode: only outbound counts toward quota
		effective = outbound
	}

	_, err := tx.ExecContext(ctx, `
UPDATE quota_windows
SET effective_bytes = ?
WHERE user_id = ? AND window_start = ?;
`, effective, userID, winStart.Format(time.RFC3339))
	return err
}

func (s *Store) enforceLimit(ctx context.Context, tx *sql.Tx, userID int64, winStart time.Time, limitBytes int64) error {
	var effective, creditBytes int64
	if err := tx.QueryRowContext(ctx, `
SELECT effective_bytes, credit_bytes
FROM quota_windows
WHERE user_id = ? AND window_start = ?;
`, userID, winStart.Format(time.RFC3339)).Scan(&effective, &creditBytes); err != nil {
		return err
	}
	if effective <= limitBytes+creditBytes {
		return nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `
UPDATE users
SET status = 'over_limit', removed_at = ?
WHERE id = ? AND status = 'active';
`, now, userID)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return nil
	}

	if _, err := tx.ExecContext(ctx, `UPDATE proxy_links SET active = 0 WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE quota_windows
SET over_limit_at = COALESCE(over_limit_at, ?)
WHERE user_id = ? AND window_start = ?;
`, now, userID, winStart.Format(time.RFC3339)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO enforcement_logs(user_id, action, reason, detail, created_at)
VALUES (?, 'disable_user', 'over_limit', 'auto removed proxy link after quota exceeded', ?);
`, userID, now); err != nil {
		return err
	}
	return nil
}

// RecordUsageIdempotent writes a usage event with idempotent INSERT ON CONFLICT handling.
// Returns ErrDuplicateEvent if the source_event_id already exists.
func (s *Store) RecordUsageIdempotent(ctx context.Context, in UsageInput) (User, error) {
	var err error
	in, err = normalizeUsageInput(in)
	if err != nil {
		return User{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()

	if exists, err := s.usageEventExistsTx(ctx, tx, in.Source, in.SourceEventID); err != nil {
		return User{}, err
	} else if exists {
		return User{}, ErrDuplicateEvent
	}

	mode, status, limitBytes, quotaCycle, quotaTZ, expiresAt, removedAt, err := s.loadUserUsageMetaTx(ctx, tx, in.UserID)
	if err != nil {
		return User{}, err
	}
	if in.NodeID != nil && *in.NodeID > 0 {
		if err := s.ensureUserAllowedOnNodeTx(ctx, tx, in.UserID, *in.NodeID); err != nil {
			return User{}, err
		}
	}
	now := time.Now().UTC()
	if err := validateUsageWriteTimestamp(in.At, now, status == "active"); err != nil {
		return User{}, err
	}
	if !usageAllowedForUserState(status, in.At, expiresAt, removedAt) {
		return User{}, ErrUserInactive
	}

	if err := s.applyUsageEventTx(ctx, tx, in, mode, limitBytes, quotaCycle, quotaTZ); err != nil {
		if errors.Is(err, ErrDuplicateEvent) {
			return User{}, ErrDuplicateEvent
		}
		return User{}, err
	}

	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return s.GetUser(ctx, in.UserID)
}

type UsageBatchItemResult struct {
	UserID    int64
	User      *User
	Duplicate bool
	Err       error
}

// RecordUsageBatchIdempotent applies usage events in one transaction and returns per-event outcomes.
// Validation/user-state errors are reported per item, while unexpected DB errors abort the whole batch.
func (s *Store) RecordUsageBatchIdempotent(ctx context.Context, in []UsageInput) ([]UsageBatchItemResult, error) {
	results := make([]UsageBatchItemResult, len(in))
	if len(in) == 0 {
		return results, nil
	}

	normalized := make([]UsageInput, len(in))
	valid := make([]bool, len(in))
	for i := range in {
		results[i].UserID = in[i].UserID
		ni, err := normalizeUsageInput(in[i])
		if err != nil {
			results[i].Err = err
			continue
		}
		results[i].UserID = ni.UserID
		normalized[i] = ni
		valid[i] = true
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	type userMeta struct {
		mode       string
		status     string
		limitBytes int64
		quotaCycle string
		quotaTZ    string
		expiresAt  time.Time
		removedAt  *time.Time
	}
	metaByUser := make(map[int64]userMeta, 32)
	affectedUsers := make(map[int64]struct{}, 32)
	now := time.Now().UTC()

	for i := range normalized {
		if !valid[i] {
			continue
		}
		item := normalized[i]
		duplicate, dupErr := s.usageEventExistsTx(ctx, tx, item.Source, item.SourceEventID)
		if dupErr != nil {
			return nil, dupErr
		}
		if duplicate {
			results[i].Duplicate = true
			continue
		}
		meta, ok := metaByUser[item.UserID]
		if !ok {
			mode, status, limitBytes, quotaCycle, quotaTZ, expiresAt, removedAt, metaErr := s.loadUserUsageMetaTx(ctx, tx, item.UserID)
			if metaErr != nil {
				results[i].Err = metaErr
				continue
			}
			meta = userMeta{
				mode:       mode,
				status:     status,
				limitBytes: limitBytes,
				quotaCycle: quotaCycle,
				quotaTZ:    quotaTZ,
				expiresAt:  expiresAt,
				removedAt:  removedAt,
			}
			metaByUser[item.UserID] = meta
		}
		if item.NodeID != nil && *item.NodeID > 0 {
			if accessErr := s.ensureUserAllowedOnNodeTx(ctx, tx, item.UserID, *item.NodeID); accessErr != nil {
				results[i].Err = accessErr
				continue
			}
		}

		if tsErr := validateUsageWriteTimestamp(item.At, now, meta.status == "active"); tsErr != nil {
			results[i].Err = tsErr
			continue
		}
		if !usageAllowedForUserState(meta.status, item.At, meta.expiresAt, meta.removedAt) {
			results[i].Err = ErrUserInactive
			continue
		}
		if applyErr := s.applyUsageEventTx(ctx, tx, item, meta.mode, meta.limitBytes, meta.quotaCycle, meta.quotaTZ); applyErr != nil {
			if errors.Is(applyErr, ErrDuplicateEvent) {
				results[i].Duplicate = true
				continue
			}
			return nil, applyErr
		}
		affectedUsers[item.UserID] = struct{}{}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	usersByID := make(map[int64]User, len(affectedUsers))
	for userID := range affectedUsers {
		u, getErr := s.GetUser(ctx, userID)
		if getErr != nil {
			continue
		}
		usersByID[userID] = u
	}
	for i := range results {
		if results[i].Err != nil || results[i].Duplicate {
			continue
		}
		if u, ok := usersByID[results[i].UserID]; ok {
			uc := u
			results[i].User = &uc
		}
	}
	return results, nil
}

func (s *Store) SweepQuotaWindows(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
SELECT id, quota_cycle, quota_tz
FROM users;
`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	now := time.Now().UTC()
	reactivated := 0
	for rows.Next() {
		var userID int64
		var cycleType, quotaTZ string
		if err := rows.Scan(&userID, &cycleType, &quotaTZ); err != nil {
			return 0, err
		}
		_, _, created, err := s.ensureQuotaWindowTx(ctx, tx, userID, cycleType, quotaTZ, now)
		if err != nil {
			return 0, err
		}
		if created {
			changed, err := s.reactivateOverLimitUserForNewCycleTx(ctx, tx, userID, now)
			if err != nil {
				return 0, err
			}
			if changed {
				reactivated++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return reactivated, nil
}

func (s *Store) ResetUserQuota(ctx context.Context, userID int64, reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "manual_reset"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var cycleType, quotaTZ string
	if err := tx.QueryRowContext(ctx, `SELECT quota_cycle, quota_tz FROM users WHERE id = ?`, userID).Scan(&cycleType, &quotaTZ); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		return err
	}
	now := time.Now().UTC()
	_, end, cycleKey := quotaWindowBounds(cycleType, quotaTZ, now)
	nowStr := now.Format(time.RFC3339)
	_, err = tx.ExecContext(ctx, `
UPDATE quota_windows
SET window_end = ?,
	closed_at = COALESCE(closed_at, ?)
WHERE user_id = ? AND window_start <= ? AND window_end > ?;
`, nowStr, nowStr, userID, nowStr, nowStr)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
	INSERT INTO quota_windows(user_id, window_start, window_end, cycle_type, cycle_key, reset_reason, inbound_bytes, outbound_bytes, effective_bytes, credit_bytes)
	VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0, 0);
	`, userID, nowStr, end.Format(time.RFC3339), cycleType, cycleKey+":manual", reason)
	if err != nil {
		return err
	}
	if _, err := s.reactivateOverLimitUserIfWithinQuotaTx(ctx, tx, userID, now); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
	INSERT INTO enforcement_logs(user_id, action, reason, detail, created_at)
	VALUES (?, 'quota_reset', 'manual', ?, ?);
`, userID, reason, nowStr)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreditUserQuota(ctx context.Context, userID int64, bytes int64) error {
	if bytes <= 0 {
		return errors.New("credit bytes must be > 0")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var cycleType, quotaTZ string
	if err := tx.QueryRowContext(ctx, `SELECT quota_cycle, quota_tz FROM users WHERE id = ?`, userID).Scan(&cycleType, &quotaTZ); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		return err
	}
	nowTime := time.Now().UTC()
	winStart, _, _, err := s.ensureQuotaWindowTx(ctx, tx, userID, cycleType, quotaTZ, nowTime)
	if err != nil {
		return err
	}
	now := nowTime.Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `
	UPDATE quota_windows
	SET credit_bytes = credit_bytes + ?
	WHERE user_id = ? AND window_start = ?;
	`, bytes, userID, winStart.Format(time.RFC3339))
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return errors.New("credit failed: no current quota window")
	}
	if _, err := s.reactivateOverLimitUserIfWithinQuotaTx(ctx, tx, userID, nowTime); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
	INSERT INTO enforcement_logs(user_id, action, reason, detail, created_at)
	VALUES (?, 'quota_credit', 'manual', ?, ?);
	`, userID, fmt.Sprintf("manual credit %d bytes", bytes), now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) reactivateOverLimitUserIfWithinQuotaTx(ctx context.Context, tx *sql.Tx, userID int64, now time.Time) (bool, error) {
	now = now.UTC()
	nowStr := now.Format(time.RFC3339)
	var status, expiresRaw string
	var limitBytes int64
	if err := tx.QueryRowContext(ctx, `
	SELECT status, expires_at, monthly_limit_bytes
	FROM users
	WHERE id = ?;
	`, userID).Scan(&status, &expiresRaw, &limitBytes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrUserNotFound
		}
		return false, err
	}
	if status != "over_limit" {
		return false, nil
	}
	expiresAt, err := time.Parse(time.RFC3339, expiresRaw)
	if err != nil {
		return false, err
	}
	if !expiresAt.After(now) {
		return false, nil
	}
	var effective, credit int64
	if err := tx.QueryRowContext(ctx, `
	SELECT effective_bytes, credit_bytes
	FROM quota_windows
	WHERE user_id = ? AND window_start <= ? AND window_end > ?
	ORDER BY window_start DESC
	LIMIT 1;
	`, userID, nowStr, nowStr).Scan(&effective, &credit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if effective > limitBytes+credit {
		return false, nil
	}
	res, err := tx.ExecContext(ctx, `
	UPDATE users
	SET status = 'active', removed_at = NULL
	WHERE id = ? AND status = 'over_limit';
	`, userID)
	if err != nil {
		return false, err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed == 0 {
		return false, nil
	}
	if err := s.activateLatestProxyLinkTx(ctx, tx, userID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
	INSERT INTO enforcement_logs(user_id, action, reason, detail, created_at)
	VALUES (?, 'enable_user', 'quota_manual_relief', 'auto re-enabled after quota reset or credit', ?);
	`, userID, nowStr); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ExtendUserPlan(ctx context.Context, userID int64, days int) error {
	if days <= 0 {
		return errors.New("days must be > 0")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var expiresRaw, status string
	if err := tx.QueryRowContext(ctx, `SELECT expires_at, status FROM users WHERE id = ?`, userID).Scan(&expiresRaw, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		return err
	}
	expiresAt, err := time.Parse(time.RFC3339, expiresRaw)
	if err != nil {
		return err
	}
	nowTime := time.Now().UTC()
	base := expiresAt.UTC()
	if base.Before(nowTime) {
		base = nowTime
	}
	next := base.AddDate(0, 0, days).UTC().Format(time.RFC3339)
	now := nowTime.Format(time.RFC3339)
	_, err = tx.ExecContext(ctx, `
	UPDATE users
	SET expires_at = ?,
		status = CASE WHEN status = 'expired' THEN 'active' ELSE status END,
		removed_at = CASE WHEN status = 'expired' THEN NULL ELSE removed_at END
	WHERE id = ?;
	`, next, userID)
	if err != nil {
		return err
	}
	if status == "expired" {
		if err := s.activateLatestProxyLinkTx(ctx, tx, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
	INSERT INTO enforcement_logs(user_id, action, reason, detail, created_at)
	VALUES (?, 'enable_user', 'manual', 'reactivated by plan extend', ?);
	`, userID, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO enforcement_logs(user_id, action, reason, detail, created_at)
		VALUES (?, 'extend_plan', 'manual', ?, ?);
		`, userID, fmt.Sprintf("extend %d days", days), now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) queueQuotaAlertsTx(ctx context.Context, tx *sql.Tx, userID int64, winStart time.Time, limitBytes int64) error {
	var effective, credit int64
	var cycleKey string
	var alert80, alert90 sql.NullString
	err := tx.QueryRowContext(ctx, `
SELECT effective_bytes, credit_bytes, cycle_key, alert_80_sent_at, alert_90_sent_at
FROM quota_windows
WHERE user_id = ? AND window_start = ?;
`, userID, winStart.Format(time.RFC3339)).Scan(&effective, &credit, &cycleKey, &alert80, &alert90)
	if err != nil {
		return err
	}
	totalLimit := limitBytes + credit
	if totalLimit <= 0 {
		return nil
	}
	ratio := (float64(effective) / float64(totalLimit)) * 100.0
	now := time.Now().UTC().Format(time.RFC3339)

	insertAlert := func(th string, message string) error {
		dedupeKey := fmt.Sprintf("quota:%s:%d:%s", th, userID, cycleKey)
		_, err := tx.ExecContext(ctx, `
INSERT INTO alerts(user_id, type, threshold, channel, message, dedupe_key, created_at)
VALUES (?, 'quota', ?, 'telegram', ?, ?, ?)
ON CONFLICT(dedupe_key) DO NOTHING;
`, userID, th, message, dedupeKey, now)
		return err
	}

	if ratio >= 80 && !alert80.Valid {
		if err := insertAlert("80", fmt.Sprintf("user=%d reached %.1f%% quota", userID, ratio)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE quota_windows SET alert_80_sent_at = ? WHERE user_id = ? AND window_start = ?;
`, now, userID, winStart.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	if ratio >= 90 && !alert90.Valid {
		if err := insertAlert("90", fmt.Sprintf("user=%d reached %.1f%% quota", userID, ratio)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE quota_windows SET alert_90_sent_at = ? WHERE user_id = ? AND window_start = ?;
`, now, userID, winStart.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListPendingAlerts(ctx context.Context, limit int) ([]Alert, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, user_id, type, threshold, channel, message, dedupe_key, sent_at, created_at
FROM alerts
WHERE sent_at IS NULL
ORDER BY id ASC
LIMIT ?;
`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Alert, 0, limit)
	for rows.Next() {
		var a Alert
		var userID sql.NullInt64
		var th sql.NullString
		var sentAt sql.NullString
		var createdAt string
		if err := rows.Scan(&a.ID, &userID, &a.Type, &th, &a.Channel, &a.Message, &a.DedupeKey, &sentAt, &createdAt); err != nil {
			return nil, err
		}
		if userID.Valid {
			v := userID.Int64
			a.UserID = &v
		}
		a.Threshold = th.String
		a.SentAt = parseOptionalRFC3339(sentAt)
		a.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) MarkAlertSent(ctx context.Context, alertID int64) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE alerts
SET sent_at = ?
WHERE id = ?;
`, time.Now().UTC().Format(time.RFC3339), alertID)
	return err
}
