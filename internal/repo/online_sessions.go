package repo

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

type UserOnlineStats struct {
	UserID       int64
	SessionCount int
	DeviceCount  int
}

type OnlineSnapshotInput struct {
	ObservedAt time.Time                 `json:"observed_at,omitempty"`
	Items      []OnlineSnapshotItemInput `json:"items,omitempty"`
}

type OnlineSnapshotItemInput struct {
	UserID     int64     `json:"user_id"`
	ClientIP   string    `json:"client_ip"`
	LastSeenAt time.Time `json:"last_seen_at,omitempty"`
}

const onlineSnapshotReasonableWindow = 5 * time.Minute

func (s *Store) ApplyOnlineSnapshot(ctx context.Context, nodeID int64, in OnlineSnapshotInput) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	observedAt := in.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	} else {
		observedAt = observedAt.UTC()
	}

	type normalizedItem struct {
		userID     int64
		clientIP   string
		lastSeenAt time.Time
	}
	items := make([]normalizedItem, 0, len(in.Items))
	keep := make(map[string]struct{}, len(in.Items))
	for _, item := range in.Items {
		if item.UserID <= 0 {
			return fmt.Errorf("invalid user id")
		}
		rawIP := strings.TrimSpace(item.ClientIP)
		ip := net.ParseIP(rawIP)
		if ip == nil {
			return fmt.Errorf("invalid client ip: %q", item.ClientIP)
		}
		lastSeenAt := item.LastSeenAt
		if lastSeenAt.IsZero() {
			lastSeenAt = observedAt
		} else {
			lastSeenAt = lastSeenAt.UTC()
			minSeen := observedAt.Add(-onlineSnapshotReasonableWindow)
			maxSeen := observedAt.Add(onlineSnapshotReasonableWindow)
			if lastSeenAt.Before(minSeen) || lastSeenAt.After(maxSeen) {
				lastSeenAt = observedAt
			}
		}
		clientIP := ip.String()
		items = append(items, normalizedItem{
			userID:     item.UserID,
			clientIP:   clientIP,
			lastSeenAt: lastSeenAt,
		})
		keep[onlineSnapshotKey(item.UserID, clientIP)] = struct{}{}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, item := range items {
		ts := item.lastSeenAt.Format(time.RFC3339)
		if _, err := tx.ExecContext(ctx, `
INSERT INTO online_sessions(user_id, client_ip, node_id, first_seen_at, last_seen_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(user_id, client_ip, node_id) DO UPDATE SET
	first_seen_at = CASE
		WHEN excluded.first_seen_at < online_sessions.first_seen_at THEN excluded.first_seen_at
		ELSE online_sessions.first_seen_at
	END,
	last_seen_at = CASE
		WHEN excluded.last_seen_at > online_sessions.last_seen_at THEN excluded.last_seen_at
		ELSE online_sessions.last_seen_at
	END;
`, item.userID, item.clientIP, nodeID, ts, ts); err != nil {
			return err
		}
	}

	rows, err := tx.QueryContext(ctx, `
SELECT id, user_id, client_ip
FROM online_sessions
WHERE node_id = ?;
`, nodeID)
	if err != nil {
		return err
	}
	var staleIDs []int64
	for rows.Next() {
		var id, userID int64
		var clientIP string
		if err := rows.Scan(&id, &userID, &clientIP); err != nil {
			_ = rows.Close()
			return err
		}
		if _, ok := keep[onlineSnapshotKey(userID, clientIP)]; !ok {
			staleIDs = append(staleIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, id := range staleIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM online_sessions WHERE id = ?;`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func onlineSnapshotKey(userID int64, clientIP string) string {
	return fmt.Sprintf("%d\x00%s", userID, clientIP)
}

func (s *Store) ListOnlineUsers(ctx context.Context, windowSec int) ([]OnlineUser, error) {
	if windowSec <= 0 {
		windowSec = 120
	}
	cutoff := time.Now().UTC().Add(-time.Duration(windowSec) * time.Second).Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, `
SELECT os.user_id, u.username, os.client_ip, os.node_id, os.first_seen_at, os.last_seen_at
FROM online_sessions os
JOIN users u ON u.id = os.user_id
WHERE os.last_seen_at >= ?
ORDER BY os.last_seen_at DESC;
`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]OnlineUser, 0, 64)
	for rows.Next() {
		var item OnlineUser
		var nodeID sql.NullInt64
		var firstRaw, lastRaw string
		if err := rows.Scan(&item.UserID, &item.Username, &item.ClientIP, &nodeID, &firstRaw, &lastRaw); err != nil {
			return nil, err
		}
		if nodeID.Valid {
			v := nodeID.Int64
			item.NodeID = &v
		}
		item.FirstSeen, _ = time.Parse(time.RFC3339, firstRaw)
		item.LastSeen, _ = time.Parse(time.RFC3339, lastRaw)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ListUserOnlineStats(ctx context.Context, windowSec int) (map[int64]UserOnlineStats, error) {
	if windowSec <= 0 {
		windowSec = 120
	}
	cutoff := time.Now().UTC().Add(-time.Duration(windowSec) * time.Second).Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, `
SELECT user_id, COUNT(*), COUNT(DISTINCT client_ip)
FROM online_sessions
WHERE last_seen_at >= ?
GROUP BY user_id;
`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]UserOnlineStats)
	for rows.Next() {
		var item UserOnlineStats
		if err := rows.Scan(&item.UserID, &item.SessionCount, &item.DeviceCount); err != nil {
			return nil, err
		}
		out[item.UserID] = item
	}
	return out, rows.Err()
}

func (s *Store) ListUserOnlineSessions(ctx context.Context, userID int64, windowSec int) ([]OnlineUser, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("invalid user id")
	}
	if windowSec <= 0 {
		windowSec = 120
	}
	cutoff := time.Now().UTC().Add(-time.Duration(windowSec) * time.Second).Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, `
SELECT os.user_id, u.username, os.client_ip, os.node_id, os.first_seen_at, os.last_seen_at
FROM online_sessions os
JOIN users u ON u.id = os.user_id
WHERE os.user_id = ? AND os.last_seen_at >= ?
ORDER BY os.last_seen_at DESC;
`, userID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]OnlineUser, 0, 8)
	for rows.Next() {
		var item OnlineUser
		var nodeID sql.NullInt64
		var firstRaw, lastRaw string
		if err := rows.Scan(&item.UserID, &item.Username, &item.ClientIP, &nodeID, &firstRaw, &lastRaw); err != nil {
			return nil, err
		}
		if nodeID.Valid {
			v := nodeID.Int64
			item.NodeID = &v
		}
		item.FirstSeen, _ = time.Parse(time.RFC3339, firstRaw)
		item.LastSeen, _ = time.Parse(time.RFC3339, lastRaw)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) EnforceIPLimit(ctx context.Context, windowSec, strikes int) ([]User, error) {
	if windowSec <= 0 {
		windowSec = 120
	}
	if strikes <= 0 {
		strikes = 3
	}
	cutoff := time.Now().UTC().Add(-time.Duration(windowSec) * time.Second).Format(time.RFC3339)
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
SELECT id, device_limit
FROM users
WHERE status = 'active';
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	affectedIDs := make([]int64, 0, 8)
	for rows.Next() {
		var userID int64
		var deviceLimit int
		if err := rows.Scan(&userID, &deviceLimit); err != nil {
			return nil, err
		}
		if deviceLimit <= 0 {
			continue
		}
		var activeIPs int
		if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM (
	SELECT DISTINCT client_ip
	FROM online_sessions
	WHERE user_id = ? AND last_seen_at >= ?
);
`, userID, cutoff).Scan(&activeIPs); err != nil {
			return nil, err
		}
		key := fmt.Sprintf("ip_limit_streak:%d", userID)
		streak, ok, err := s.getXrayStateInt64Tx(ctx, tx, key)
		if err != nil {
			return nil, err
		}
		if !ok {
			streak = 0
		}

		if activeIPs > deviceLimit {
			streak++
			if err := s.setXrayStateInt64Tx(ctx, tx, key, streak, now); err != nil {
				return nil, err
			}
			if int(streak) >= strikes {
				res, err := tx.ExecContext(ctx, `
UPDATE users
SET status = 'over_ip_limit', removed_at = ?
WHERE id = ? AND status = 'active';
`, nowStr, userID)
				if err != nil {
					return nil, err
				}
				changed, _ := res.RowsAffected()
				if changed > 0 {
					affectedIDs = append(affectedIDs, userID)
					if _, err := tx.ExecContext(ctx, `UPDATE proxy_links SET active = 0 WHERE user_id = ?`, userID); err != nil {
						return nil, err
					}
					if _, err := tx.ExecContext(ctx, `
INSERT INTO enforcement_logs(user_id, action, reason, detail, created_at)
VALUES (?, 'disable_user', 'over_ip_limit', ?, ?);
`, userID, fmt.Sprintf("ip_limit triggered with %d active ips", activeIPs), nowStr); err != nil {
						return nil, err
					}
					dedupe := fmt.Sprintf("ip:%d:%s", userID, now.Format("2006-01-02T15:04"))
					_, _ = tx.ExecContext(ctx, `
INSERT INTO alerts(user_id, type, threshold, channel, message, dedupe_key, created_at)
VALUES (?, 'ip', ?, 'telegram', ?, ?, ?)
ON CONFLICT(dedupe_key) DO NOTHING;
`, userID, strconv.Itoa(deviceLimit), fmt.Sprintf("user=%d exceeded ip limit (%d)", userID, deviceLimit), dedupe, nowStr)
				}
			}
		} else if streak > 0 {
			if err := s.setXrayStateInt64Tx(ctx, tx, key, 0, now); err != nil {
				return nil, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	users := make([]User, 0, len(affectedIDs))
	for _, userID := range affectedIDs {
		u, err := s.GetUser(ctx, userID)
		if err != nil {
			continue
		}
		users = append(users, u)
	}
	return users, nil
}

func (s *Store) PruneOnlineSessions(ctx context.Context, olderThan time.Time, batchSize int) (int64, error) {
	if olderThan.IsZero() {
		return 0, nil
	}
	if batchSize <= 0 || batchSize > 5000 {
		batchSize = 1000
	}
	cutoff := olderThan.UTC().Format(time.RFC3339)

	q := `
DELETE FROM online_sessions
WHERE id IN (
	SELECT id
	FROM online_sessions
	WHERE last_seen_at < ?
	ORDER BY id
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
			break
		}
	}
	return total, nil
}
