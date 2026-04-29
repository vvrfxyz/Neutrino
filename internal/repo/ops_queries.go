package repo

import (
	"context"
	"database/sql"
	"time"
)

func (s *Store) ListAlerts(ctx context.Context, limit int) ([]AlertListItem, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT
	a.id, a.user_id, COALESCE(u.username, ''),
	a.type, COALESCE(a.threshold, ''), a.channel, a.message, a.dedupe_key,
	a.sent_at, a.created_at
FROM alerts a
LEFT JOIN users u ON u.id = a.user_id
ORDER BY a.created_at DESC
LIMIT ?;
`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]AlertListItem, 0, limit)
	for rows.Next() {
		var it AlertListItem
		var userID sql.NullInt64
		var sentAt sql.NullString
		var createdAt string
		if err := rows.Scan(
			&it.ID, &userID, &it.Username,
			&it.Type, &it.Threshold, &it.Channel, &it.Message, &it.DedupeKey,
			&sentAt, &createdAt,
		); err != nil {
			return nil, err
		}
		if userID.Valid {
			it.UserID = &userID.Int64
		}
		it.SentAt = parseOptionalRFC3339(sentAt)
		it.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) ListEnforcementLogs(ctx context.Context, limit int) ([]EnforcementLog, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT
	e.id, e.user_id, COALESCE(u.username, ''),
	e.action, e.reason, COALESCE(e.detail, ''), e.created_at
FROM enforcement_logs e
LEFT JOIN users u ON u.id = e.user_id
ORDER BY e.created_at DESC
LIMIT ?;
`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]EnforcementLog, 0, limit)
	for rows.Next() {
		var it EnforcementLog
		var createdAt string
		if err := rows.Scan(&it.ID, &it.UserID, &it.Username, &it.Action, &it.Reason, &it.Detail, &createdAt); err != nil {
			return nil, err
		}
		it.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) GetTrafficTotals(ctx context.Context) (int64, int64, error) {
	var in, out int64
	err := s.db.QueryRowContext(ctx, `
	SELECT COALESCE(SUM(inbound_bytes), 0), COALESCE(SUM(outbound_bytes), 0)
FROM traffic_stats;
`).Scan(&in, &out)
	if err != nil {
		return 0, 0, err
	}
	return in, out, nil
}
