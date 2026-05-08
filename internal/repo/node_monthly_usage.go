package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

type NodeMonthlyUsage struct {
	NodeID         int64
	MonthKey       string
	TimezoneName   string
	RXBytes        int64
	TXBytes        int64
	BaselineRX     int64
	BaselineTX     int64
	LastRXTotal    int64
	LastTXTotal    int64
	CounterSource  string
	LastReportedAt *time.Time
	UpdatedAt      time.Time
}

func normalizeNodeMonthKey(v string) string {
	v = strings.TrimSpace(v)
	if len(v) != len("2006-01") {
		return ""
	}
	if v[4] != '-' {
		return ""
	}
	for i := range v {
		if i == 4 {
			continue
		}
		if v[i] < '0' || v[i] > '9' {
			return ""
		}
	}
	if _, err := time.Parse("2006-01", v); err != nil {
		return ""
	}
	return v
}

func normalizeTimezoneName(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 128 {
		v = v[:128]
	}
	return v
}

func normalizeCounterSource(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 255 {
		v = v[:255]
	}
	return v
}

func clampNonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

func addNonNegativeSaturating(base, delta int64) int64 {
	if delta <= 0 {
		return base
	}
	if base > math.MaxInt64-delta {
		return math.MaxInt64
	}
	return base + delta
}

func nodeMonthLocation(timezoneName string) (*time.Location, bool) {
	timezoneName = strings.TrimSpace(timezoneName)
	if timezoneName == "" {
		return nil, false
	}
	if timezoneName == "UTC" {
		return time.UTC, true
	}
	if loc, err := time.LoadLocation(timezoneName); err == nil && loc != nil {
		return loc, true
	}
	if !strings.HasPrefix(timezoneName, "UTC+") && !strings.HasPrefix(timezoneName, "UTC-") {
		return nil, false
	}
	sign := 1
	if timezoneName[3] == '-' {
		sign = -1
	}
	parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(timezoneName, "UTC+"), "UTC-"), ":")
	if len(parts) != 2 {
		return nil, false
	}
	hours, errH := strconv.Atoi(parts[0])
	minutes, errM := strconv.Atoi(parts[1])
	if errH != nil || errM != nil || hours < 0 || hours > 23 || minutes < 0 || minutes > 59 {
		return nil, false
	}
	return time.FixedZone(timezoneName, sign*(hours*3600+minutes*60)), true
}

func expectedNodeMonthKey(reportedAt time.Time, timezoneName string) (string, bool) {
	loc, ok := nodeMonthLocation(timezoneName)
	if !ok {
		return "", false
	}
	return reportedAt.In(loc).Format("2006-01"), true
}

func (s *Store) UpsertNodeMonthlyUsageFromReport(ctx context.Context, nodeID int64, in NodeReportMetricsInput, reportedAt time.Time) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	counterSource := normalizeCounterSource(in.NetSource)
	if counterSource == "" {
		// Old agents reported already-aggregated month totals. The panel-side
		// accumulator is intentionally driven only by raw host counters.
		return nil
	}
	if reportedAt.IsZero() {
		reportedAt = time.Now().UTC()
	}
	reportedAt = reportedAt.UTC()
	now := time.Now().UTC()
	timezoneName := normalizeTimezoneName(in.MonthTimezone)
	monthKey := normalizeNodeMonthKey(in.MonthKey)
	if monthKey == "" {
		return nil
	}
	if expected, ok := expectedNodeMonthKey(reportedAt, timezoneName); ok && expected != "" && monthKey != expected {
		monthKey = expected
	}
	rxTotal := clampNonNegative(in.NetRXTotal)
	txTotal := clampNonNegative(in.NetTXTotal)
	reportedAtStr := reportedAt.Format(time.RFC3339)
	nowStr := now.Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var row NodeMonthlyUsage
	var lastReportedRaw sql.NullString
	err = tx.QueryRowContext(ctx, `
SELECT
	timezone_name,
	rx_bytes,
	tx_bytes,
	baseline_rx_total,
	baseline_tx_total,
	last_rx_total,
	last_tx_total,
	counter_source,
	last_reported_at
FROM node_monthly_usage
WHERE node_id = ? AND month_key = ?;
`, nodeID, monthKey).Scan(
		&row.TimezoneName,
		&row.RXBytes,
		&row.TXBytes,
		&row.BaselineRX,
		&row.BaselineTX,
		&row.LastRXTotal,
		&row.LastTXTotal,
		&row.CounterSource,
		&lastReportedRaw,
	)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `
	INSERT INTO node_monthly_usage(
		node_id,
		month_key,
		timezone_name,
		rx_bytes,
		tx_bytes,
		baseline_rx_total,
		baseline_tx_total,
		last_rx_total,
		last_tx_total,
		counter_source,
		last_reported_at,
		updated_at
	)
	VALUES (?, ?, ?, 0, 0, ?, ?, ?, ?, ?, ?, ?);
	`, nodeID, monthKey, timezoneName, rxTotal, txTotal, rxTotal, txTotal, counterSource, reportedAtStr, nowStr)
		if err != nil {
			return err
		}
		return tx.Commit()
	}

	if lastReportedRaw.Valid && strings.TrimSpace(lastReportedRaw.String) != "" {
		if lastReportedAt, parseErr := time.Parse(time.RFC3339, lastReportedRaw.String); parseErr == nil && !reportedAt.After(lastReportedAt) {
			return tx.Commit()
		}
	}

	baselineRX := row.BaselineRX
	baselineTX := row.BaselineTX
	lastRX := row.LastRXTotal
	lastTX := row.LastTXTotal
	accRX := row.RXBytes
	accTX := row.TXBytes
	if baselineRX < 0 {
		baselineRX = 0
	}
	if baselineTX < 0 {
		baselineTX = 0
	}
	if lastRX < 0 {
		lastRX = 0
	}
	if lastTX < 0 {
		lastTX = 0
	}
	if accRX < 0 {
		accRX = 0
	}
	if accTX < 0 {
		accTX = 0
	}

	rebased := false
	if normalizeCounterSource(row.CounterSource) != counterSource {
		rebased = true
	} else if rxTotal < lastRX || txTotal < lastTX {
		rebased = true
	}

	if rebased {
		baselineRX = rxTotal
		baselineTX = txTotal
	} else {
		accRX = addNonNegativeSaturating(accRX, rxTotal-lastRX)
		accTX = addNonNegativeSaturating(accTX, txTotal-lastTX)
	}
	lastRX = rxTotal
	lastTX = txTotal
	if timezoneName == "" {
		timezoneName = row.TimezoneName
	}

	_, err = tx.ExecContext(ctx, `
UPDATE node_monthly_usage
SET
	timezone_name = ?,
	rx_bytes = ?,
	tx_bytes = ?,
	baseline_rx_total = ?,
	baseline_tx_total = ?,
	last_rx_total = ?,
	last_tx_total = ?,
	counter_source = ?,
	last_reported_at = ?,
	updated_at = ?
WHERE node_id = ? AND month_key = ?;
`, timezoneName, accRX, accTX, baselineRX, baselineTX, lastRX, lastTX, counterSource, reportedAtStr, nowStr, nodeID, monthKey)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListLatestNodeMonthlyUsage(ctx context.Context) (map[int64]NodeMonthlyUsage, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT
	node_id,
	month_key,
	timezone_name,
	rx_bytes,
	tx_bytes,
	baseline_rx_total,
	baseline_tx_total,
	last_rx_total,
	last_tx_total,
	counter_source,
	last_reported_at,
	updated_at
FROM node_monthly_usage
ORDER BY node_id ASC, month_key DESC, COALESCE(last_reported_at, '') DESC;
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]NodeMonthlyUsage)
	for rows.Next() {
		item, err := scanNodeMonthlyUsage(rows)
		if err != nil {
			return nil, err
		}
		if _, exists := out[item.NodeID]; exists {
			continue
		}
		if normalizeNodeMonthKey(item.MonthKey) == "" {
			continue
		}
		out[item.NodeID] = item
	}
	return out, rows.Err()
}

func (s *Store) GetLatestNodeMonthlyUsage(ctx context.Context, nodeID int64) (NodeMonthlyUsage, bool, error) {
	if nodeID <= 0 {
		return NodeMonthlyUsage{}, false, fmt.Errorf("invalid node id")
	}
	row := s.db.QueryRowContext(ctx, `
SELECT
	node_id,
	month_key,
	timezone_name,
	rx_bytes,
	tx_bytes,
	baseline_rx_total,
	baseline_tx_total,
	last_rx_total,
	last_tx_total,
	counter_source,
	last_reported_at,
	updated_at
FROM node_monthly_usage
WHERE node_id = ?
ORDER BY month_key DESC, COALESCE(last_reported_at, '') DESC
LIMIT 1;
`, nodeID)
	item, err := scanNodeMonthlyUsage(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NodeMonthlyUsage{}, false, nil
		}
		return NodeMonthlyUsage{}, false, err
	}
	if normalizeNodeMonthKey(item.MonthKey) == "" {
		return NodeMonthlyUsage{}, false, nil
	}
	return item, true, nil
}

type nodeMonthlyUsageScanner interface {
	Scan(dest ...any) error
}

func scanNodeMonthlyUsage(scanner nodeMonthlyUsageScanner) (NodeMonthlyUsage, error) {
	var item NodeMonthlyUsage
	var lastReportedAt sql.NullString
	var updatedAt string
	if err := scanner.Scan(
		&item.NodeID,
		&item.MonthKey,
		&item.TimezoneName,
		&item.RXBytes,
		&item.TXBytes,
		&item.BaselineRX,
		&item.BaselineTX,
		&item.LastRXTotal,
		&item.LastTXTotal,
		&item.CounterSource,
		&lastReportedAt,
		&updatedAt,
	); err != nil {
		return NodeMonthlyUsage{}, err
	}
	if lastReportedAt.Valid && strings.TrimSpace(lastReportedAt.String) != "" {
		if parsed, err := time.Parse(time.RFC3339, lastReportedAt.String); err == nil {
			item.LastReportedAt = &parsed
		}
	}
	item.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return item, nil
}
