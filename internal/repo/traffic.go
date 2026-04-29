package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

func (s *Store) ListUserEvents(ctx context.Context, userID int64, limit int) ([]UserEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT
	id, user_id, node_id, direction, bytes, event_at, source, source_event_id,
	target_host, target_ip, target_port, sni, destination, client_ip, inbound_tag
FROM traffic_events
WHERE user_id = ?
ORDER BY event_at DESC, id DESC
LIMIT ?;
`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := make([]UserEvent, 0, limit)
	for rows.Next() {
		var e UserEvent
		var eventAt string
		var nodeID sql.NullInt64
		var sourceEventID, targetHost, targetIP, sni, destination, clientIP, inboundTag sql.NullString
		var targetPort sql.NullInt64

		if err := rows.Scan(
			&e.ID, &e.UserID, &nodeID, &e.Direction, &e.Bytes, &eventAt, &e.Source, &sourceEventID,
			&targetHost, &targetIP, &targetPort, &sni, &destination, &clientIP, &inboundTag,
		); err != nil {
			return nil, err
		}
		if nodeID.Valid && nodeID.Int64 > 0 {
			v := nodeID.Int64
			e.NodeID = &v
		}

		parsed, err := time.Parse(time.RFC3339, eventAt)
		if err != nil {
			return nil, err
		}
		e.EventAt = parsed
		e.SourceEventID = sourceEventID.String
		e.TargetHost = targetHost.String
		e.TargetIP = targetIP.String
		if targetPort.Valid {
			v := targetPort.Int64
			e.TargetPort = &v
		}
		e.SNI = sni.String
		e.Destination = destination.String
		e.ClientIP = clientIP.String
		e.InboundTag = inboundTag.String

		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) ListUserEventsFiltered(ctx context.Context, userID int64, limit int, source string, nodeID *int64) ([]UserEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	source = strings.TrimSpace(source)
	if source == "*" || strings.EqualFold(source, "all") {
		source = ""
	}

	args := []any{userID}
	where := "WHERE user_id = ?"
	if source != "" {
		where += " AND source = ?"
		args = append(args, source)
	}
	if nodeID != nil && *nodeID > 0 {
		where += " AND node_id = ?"
		args = append(args, *nodeID)
	}
	args = append(args, limit)

	q := fmt.Sprintf(`
SELECT
	id, user_id, node_id, direction, bytes, event_at, source, source_event_id,
	target_host, target_ip, target_port, sni, destination, client_ip, inbound_tag
FROM traffic_events
%s
ORDER BY event_at DESC, id DESC
LIMIT ?;
`, where)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := make([]UserEvent, 0, limit)
	for rows.Next() {
		var e UserEvent
		var eventAt string
		var nid sql.NullInt64
		var sourceEventID, targetHost, targetIP, sni, destination, clientIP, inboundTag sql.NullString
		var targetPort sql.NullInt64
		if err := rows.Scan(
			&e.ID, &e.UserID, &nid, &e.Direction, &e.Bytes, &eventAt, &e.Source, &sourceEventID,
			&targetHost, &targetIP, &targetPort, &sni, &destination, &clientIP, &inboundTag,
		); err != nil {
			return nil, err
		}
		if nid.Valid && nid.Int64 > 0 {
			v := nid.Int64
			e.NodeID = &v
		}
		parsed, err := time.Parse(time.RFC3339, eventAt)
		if err != nil {
			return nil, err
		}
		e.EventAt = parsed
		e.SourceEventID = sourceEventID.String
		e.TargetHost = targetHost.String
		e.TargetIP = targetIP.String
		if targetPort.Valid {
			v := targetPort.Int64
			e.TargetPort = &v
		}
		e.SNI = sni.String
		e.Destination = destination.String
		e.ClientIP = clientIP.String
		e.InboundTag = inboundTag.String
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) GetTrafficSeries(ctx context.Context, userID int64, period string) ([]TrafficBucket, error) {
	var (
		start     time.Time
		end       time.Time
		stepCount int
	)

	now := time.Now().UTC()
	switch period {
	case "hourly":
		end = now.Truncate(time.Hour).Add(time.Hour)
		start = end.Add(-24 * time.Hour)
		stepCount = 24
	case "daily":
		end = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
		start = end.AddDate(0, 0, -30)
		stepCount = 30
	case "monthly":
		end = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
		start = end.AddDate(0, -12, 0)
		stepCount = 12
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedTrafficPeriod, period)
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT bucket_start, inbound_bytes, outbound_bytes
FROM traffic_rollups_hourly
WHERE user_id = ? AND bucket_start >= ? AND bucket_start < ?;
`, userID, start.Format(time.RFC3339), end.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type sumPair struct {
		in  int64
		out int64
	}
	raw := make(map[string]sumPair, stepCount)
	for rows.Next() {
		var bucketStart string
		var inBytes, outBytes int64
		if err := rows.Scan(&bucketStart, &inBytes, &outBytes); err != nil {
			return nil, err
		}
		parsedAt, err := time.Parse(time.RFC3339, bucketStart)
		if err != nil {
			continue
		}
		bucketKey := trafficBucketKey(parsedAt, period)
		sum := raw[bucketKey]
		sum.in += inBytes
		sum.out += outBytes
		raw[bucketKey] = sum
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	series := make([]TrafficBucket, 0, stepCount)
	t := start
	for i := 0; i < stepCount; i++ {
		key := trafficBucketKey(t, period)
		sum := raw[key]
		series = append(series, TrafficBucket{
			BucketStart:   t,
			InboundBytes:  sum.in,
			OutboundBytes: sum.out,
		})
		if period == "monthly" {
			t = t.AddDate(0, 1, 0)
		} else if period == "daily" {
			t = t.AddDate(0, 0, 1)
		} else {
			t = t.Add(time.Hour)
		}
	}
	return series, nil
}

// TrafficSummary represents aggregated traffic data for analysis
type TrafficSummary struct {
	KPI             TrafficKPI              `json:"kpi"`
	Series          []TrafficBucket         `json:"series"`
	TopUsers        []TopUserTraffic        `json:"top_users"`
	TopDestinations []TopDestinationTraffic `json:"top_destinations"`
	TopSNI          []TopSNITraffic         `json:"top_sni"`
}

type TrafficKPI struct {
	TotalIn     int64 `json:"total_in"`
	TotalOut    int64 `json:"total_out"`
	EventCount  int64 `json:"events"`
	ActiveUsers int64 `json:"active_users"`
}

type TopUserTraffic struct {
	UserID     int64  `json:"user_id"`
	Username   string `json:"username"`
	TotalBytes int64  `json:"total_bytes"`
}

type TopDestinationTraffic struct {
	HostOrIP   string `json:"host_or_ip"`
	TotalBytes int64  `json:"total_bytes"`
	Count      int64  `json:"count"`
}

type TopSNITraffic struct {
	SNI   string `json:"sni"`
	Count int64  `json:"count"`
}

// GetTrafficSummary returns aggregated traffic data for a time range.
func (s *Store) GetTrafficSummary(ctx context.Context, rangeParam string, nodeID *int64, userID *int64) (TrafficSummary, error) {
	return s.getTrafficSummaryAt(ctx, rangeParam, nodeID, userID, time.Now().UTC())
}

func (s *Store) getTrafficSummaryAt(ctx context.Context, rangeParam string, nodeID *int64, userID *int64, now time.Time) (TrafficSummary, error) {
	start, endExclusive, step, stepCount, bucketFn, useRollups := trafficSummaryRange(rangeParam, now)

	summary := TrafficSummary{
		Series:          make([]TrafficBucket, 0, stepCount),
		TopUsers:        make([]TopUserTraffic, 0, 10),
		TopDestinations: make([]TopDestinationTraffic, 0, 10),
		TopSNI:          make([]TopSNITraffic, 0, 10),
	}

	rollupFilter := ""
	rollupArgs := []any{start.Format(time.RFC3339), endExclusive.Format(time.RFC3339)}
	if nodeID != nil {
		rollupFilter += " AND node_id = ?"
		rollupArgs = append(rollupArgs, *nodeID)
	}
	if userID != nil {
		rollupFilter += " AND user_id = ?"
		rollupArgs = append(rollupArgs, *userID)
	}

	eventFilter := ""
	eventArgs := []any{start.Format(time.RFC3339), endExclusive.Format(time.RFC3339)}
	if nodeID != nil {
		eventFilter += " AND node_id = ?"
		eventArgs = append(eventArgs, *nodeID)
	}
	if userID != nil {
		eventFilter += " AND user_id = ?"
		eventArgs = append(eventArgs, *userID)
	}

	bucketData := make(map[string]struct{ in, out int64 })
	if useRollups {
		bucketExpr := trafficSummaryBucketExpr(rangeParam, true)
		kpiQuery := fmt.Sprintf(`
SELECT
	COALESCE(SUM(inbound_bytes), 0),
	COALESCE(SUM(outbound_bytes), 0),
	COUNT(*),
	COUNT(DISTINCT user_id)
FROM traffic_rollups_hourly
WHERE bucket_start >= ? AND bucket_start < ?%s;
`, rollupFilter)
		if err := s.db.QueryRowContext(ctx, kpiQuery, rollupArgs...).Scan(
			&summary.KPI.TotalIn,
			&summary.KPI.TotalOut,
			&summary.KPI.EventCount,
			&summary.KPI.ActiveUsers,
		); err != nil {
			return summary, err
		}

		seriesQuery := fmt.Sprintf(`
SELECT
	%s as bucket_start,
	COALESCE(SUM(inbound_bytes), 0),
	COALESCE(SUM(outbound_bytes), 0)
FROM traffic_rollups_hourly
WHERE bucket_start >= ? AND bucket_start < ?%s
GROUP BY bucket_start
ORDER BY bucket_start;
`, bucketExpr, rollupFilter)
		rows, err := s.db.QueryContext(ctx, seriesQuery, rollupArgs...)
		if err != nil {
			return summary, err
		}
		defer rows.Close()

		for rows.Next() {
			var bucketStart string
			var inBytes, outBytes int64
			if err := rows.Scan(&bucketStart, &inBytes, &outBytes); err != nil {
				return summary, err
			}
			bucketData[bucketStart] = struct{ in, out int64 }{in: inBytes, out: outBytes}
		}
		if err := rows.Err(); err != nil {
			return summary, err
		}

		topUsersQuery := fmt.Sprintf(`
SELECT
	tr.user_id,
	u.username,
	SUM(tr.inbound_bytes + tr.outbound_bytes) as total
FROM traffic_rollups_hourly tr
JOIN users u ON u.id = tr.user_id
WHERE tr.bucket_start >= ? AND tr.bucket_start < ?%s
GROUP BY tr.user_id
ORDER BY total DESC
LIMIT 10;
`, rollupFilter)
		userRows, err := s.db.QueryContext(ctx, topUsersQuery, rollupArgs...)
		if err != nil {
			return summary, err
		}
		defer userRows.Close()
		for userRows.Next() {
			var tu TopUserTraffic
			if err := userRows.Scan(&tu.UserID, &tu.Username, &tu.TotalBytes); err != nil {
				return summary, err
			}
			summary.TopUsers = append(summary.TopUsers, tu)
		}
		if err := userRows.Err(); err != nil {
			return summary, err
		}
	} else {
		bucketExpr := trafficSummaryBucketExpr(rangeParam, false)
		kpiQuery := fmt.Sprintf(`
SELECT
	COALESCE(SUM(CASE WHEN direction = 'inbound' THEN bytes ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN direction = 'outbound' THEN bytes ELSE 0 END), 0),
	COUNT(*),
	COUNT(DISTINCT user_id)
FROM traffic_events
WHERE event_at >= ? AND event_at < ?%s;
`, eventFilter)
		if err := s.db.QueryRowContext(ctx, kpiQuery, eventArgs...).Scan(
			&summary.KPI.TotalIn,
			&summary.KPI.TotalOut,
			&summary.KPI.EventCount,
			&summary.KPI.ActiveUsers,
		); err != nil {
			return summary, err
		}

		seriesQuery := fmt.Sprintf(`
SELECT
	%s as bucket_start,
	COALESCE(SUM(CASE WHEN direction = 'inbound' THEN bytes ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN direction = 'outbound' THEN bytes ELSE 0 END), 0)
FROM traffic_events
WHERE event_at >= ? AND event_at < ?%s
GROUP BY bucket_start
ORDER BY bucket_start;
`, bucketExpr, eventFilter)
		rows, err := s.db.QueryContext(ctx, seriesQuery, eventArgs...)
		if err != nil {
			return summary, err
		}
		defer rows.Close()
		for rows.Next() {
			var bucketStart string
			var inBytes, outBytes int64
			if err := rows.Scan(&bucketStart, &inBytes, &outBytes); err != nil {
				return summary, err
			}
			bucketData[bucketStart] = struct{ in, out int64 }{in: inBytes, out: outBytes}
		}
		if err := rows.Err(); err != nil {
			return summary, err
		}

		topUsersQuery := fmt.Sprintf(`
SELECT
	te.user_id,
	u.username,
	SUM(te.bytes) as total
FROM traffic_events te
JOIN users u ON u.id = te.user_id
WHERE te.event_at >= ? AND te.event_at < ?%s
GROUP BY te.user_id
ORDER BY total DESC
LIMIT 10;
`, eventFilter)
		userRows, err := s.db.QueryContext(ctx, topUsersQuery, eventArgs...)
		if err != nil {
			return summary, err
		}
		defer userRows.Close()
		for userRows.Next() {
			var tu TopUserTraffic
			if err := userRows.Scan(&tu.UserID, &tu.Username, &tu.TotalBytes); err != nil {
				return summary, err
			}
			summary.TopUsers = append(summary.TopUsers, tu)
		}
		if err := userRows.Err(); err != nil {
			return summary, err
		}
	}

	// Fill series.
	t := start
	for i := 0; i < stepCount; i++ {
		key := bucketFn(t).Format(time.RFC3339)
		d := bucketData[key]
		summary.Series = append(summary.Series, TrafficBucket{
			BucketStart:   t,
			InboundBytes:  d.in,
			OutboundBytes: d.out,
		})
		t = t.Add(step)
	}

	// Top destinations query
	topDestQuery := fmt.Sprintf(`
SELECT
	COALESCE(NULLIF(target_host, ''), target_ip) as dest,
	SUM(bytes) as total,
	COUNT(*) as cnt
FROM traffic_events
WHERE event_at >= ? AND event_at < ?%s AND source = 'xray-access' AND (target_host != '' OR target_ip != '')
GROUP BY dest
ORDER BY cnt DESC
LIMIT 10;
`, eventFilter)

	destRows, err := s.db.QueryContext(ctx, topDestQuery, eventArgs...)
	if err != nil {
		return summary, err
	}
	defer destRows.Close()

	for destRows.Next() {
		var td TopDestinationTraffic
		if err := destRows.Scan(&td.HostOrIP, &td.TotalBytes, &td.Count); err != nil {
			return summary, err
		}
		summary.TopDestinations = append(summary.TopDestinations, td)
	}
	if err := destRows.Err(); err != nil {
		return summary, err
	}

	// Top SNI query
	topSNIQuery := fmt.Sprintf(`
SELECT
	sni,
	COUNT(*) as cnt
FROM traffic_events
WHERE event_at >= ? AND event_at < ?%s AND source = 'xray-access' AND sni != '' AND sni IS NOT NULL
GROUP BY sni
ORDER BY cnt DESC
LIMIT 10;
`, eventFilter)

	sniRows, err := s.db.QueryContext(ctx, topSNIQuery, eventArgs...)
	if err != nil {
		return summary, err
	}
	defer sniRows.Close()

	for sniRows.Next() {
		var ts TopSNITraffic
		if err := sniRows.Scan(&ts.SNI, &ts.Count); err != nil {
			return summary, err
		}
		summary.TopSNI = append(summary.TopSNI, ts)
	}
	if err := sniRows.Err(); err != nil {
		return summary, err
	}

	return summary, nil
}

func trafficSummaryBucketExpr(rangeParam string, useRollups bool) string {
	if useRollups {
		return "strftime('%Y-%m-%dT00:00:00Z', bucket_start)"
	}
	switch rangeParam {
	case "1h":
		return "strftime('%Y-%m-%dT%H:%M:00Z', (CAST(strftime('%s', event_at) AS INTEGER) / 300) * 300, 'unixepoch')"
	case "24h":
		return "strftime('%Y-%m-%dT%H:00:00Z', event_at)"
	default:
		return "strftime('%Y-%m-%dT%H:00:00Z', event_at)"
	}
}

func trafficSummaryRange(rangeParam string, now time.Time) (start time.Time, endExclusive time.Time, step time.Duration, stepCount int, bucketFn func(time.Time) time.Time, useRollups bool) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	switch rangeParam {
	case "1h":
		// Include the current (partial) 5-min bucket.
		endExclusive = now.Truncate(5 * time.Minute).Add(5 * time.Minute)
		start = endExclusive.Add(-1 * time.Hour)
		step = 5 * time.Minute
		stepCount = 12
		bucketFn = func(t time.Time) time.Time { return t.UTC().Truncate(5 * time.Minute) }
		useRollups = false
	case "24h":
		// Include the current (partial) hour bucket.
		endExclusive = now.Truncate(time.Hour).Add(time.Hour)
		start = endExclusive.Add(-24 * time.Hour)
		step = time.Hour
		stepCount = 24
		bucketFn = func(t time.Time) time.Time { return t.UTC().Truncate(time.Hour) }
		useRollups = false
	case "7d":
		dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		endExclusive = dayStart.AddDate(0, 0, 1)
		start = endExclusive.AddDate(0, 0, -7)
		step = 24 * time.Hour
		stepCount = 7
		bucketFn = func(t time.Time) time.Time {
			tt := t.UTC()
			return time.Date(tt.Year(), tt.Month(), tt.Day(), 0, 0, 0, 0, time.UTC)
		}
		useRollups = true
	case "30d":
		dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		endExclusive = dayStart.AddDate(0, 0, 1)
		start = endExclusive.AddDate(0, 0, -30)
		step = 24 * time.Hour
		stepCount = 30
		bucketFn = func(t time.Time) time.Time {
			tt := t.UTC()
			return time.Date(tt.Year(), tt.Month(), tt.Day(), 0, 0, 0, 0, time.UTC)
		}
		useRollups = true
	default:
		// Default to 24h.
		endExclusive = now.Truncate(time.Hour).Add(time.Hour)
		start = endExclusive.Add(-24 * time.Hour)
		step = time.Hour
		stepCount = 24
		bucketFn = func(t time.Time) time.Time { return t.UTC().Truncate(time.Hour) }
		useRollups = false
	}
	return start, endExclusive, step, stepCount, bucketFn, useRollups
}

func (s *Store) PruneTrafficEvents(ctx context.Context, source string, olderThan time.Time, batchSize int) (int64, error) {
	if olderThan.IsZero() {
		return 0, nil
	}
	if batchSize <= 0 || batchSize > 5000 {
		batchSize = 1000
	}
	source = strings.TrimSpace(source)
	cutoff := olderThan.UTC().Format(time.RFC3339)

	var q string
	var argsBase []any
	if source == "" {
		q = `
DELETE FROM traffic_events
WHERE id IN (
	SELECT id
	FROM traffic_events
	WHERE event_at < ?
	ORDER BY id
	LIMIT ?
);
`
		argsBase = []any{cutoff, batchSize}
	} else {
		q = `
DELETE FROM traffic_events
WHERE id IN (
	SELECT id
	FROM traffic_events
	WHERE source = ? AND event_at < ?
	ORDER BY id
	LIMIT ?
);
`
		argsBase = []any{source, cutoff, batchSize}
	}

	var total int64
	for {
		res, err := s.db.ExecContext(ctx, q, argsBase...)
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
