package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var cleanupSQLIdentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type NodeMetadata struct {
	NodeID           int64      `json:"node_id"`
	Provider         string     `json:"provider"`
	Region           string     `json:"region"`
	Country          string     `json:"country"`
	City             string     `json:"city"`
	Latitude         *float64   `json:"latitude,omitempty"`
	Longitude        *float64   `json:"longitude,omitempty"`
	Tags             []string   `json:"tags"`
	MonthlyCostCents int64      `json:"monthly_cost_cents"`
	Currency         string     `json:"currency"`
	RenewCycle       string     `json:"renew_cycle"`
	RenewAt          *time.Time `json:"renew_at,omitempty"`
	Note             string     `json:"note"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type UpsertNodeMetadataInput struct {
	Provider         string     `json:"provider"`
	Region           string     `json:"region"`
	Country          string     `json:"country"`
	City             string     `json:"city"`
	Latitude         *float64   `json:"latitude,omitempty"`
	Longitude        *float64   `json:"longitude,omitempty"`
	Tags             []string   `json:"tags"`
	MonthlyCostCents int64      `json:"monthly_cost_cents"`
	Currency         string     `json:"currency"`
	RenewCycle       string     `json:"renew_cycle"`
	RenewAt          *time.Time `json:"renew_at,omitempty"`
	Note             string     `json:"note"`
}

type OpsAlert struct {
	ID          int64      `json:"id"`
	NodeID      *int64     `json:"node_id,omitempty"`
	Kind        string     `json:"kind"`
	Severity    string     `json:"severity"`
	Status      string     `json:"status"`
	Message     string     `json:"message"`
	DedupeKey   string     `json:"dedupe_key"`
	FirstSeenAt time.Time  `json:"first_seen_at"`
	LastSeenAt  time.Time  `json:"last_seen_at"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
}

type OpsCleanupCounts struct {
	MetricSamples int64
	MetricDetails int64
	ProbeResults  int64
	OpsAlerts     int64
}

func (s *Store) UpsertNodeMetadata(ctx context.Context, nodeID int64, in UpsertNodeMetadataInput) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	in.Provider = trimLimit(in.Provider, 128)
	in.Region = trimLimit(in.Region, 128)
	in.Country = trimLimit(in.Country, 64)
	in.City = trimLimit(in.City, 128)
	in.Currency = strings.ToUpper(trimLimit(defaultText(in.Currency, "USD"), 8))
	in.RenewCycle = trimLimit(in.RenewCycle, 64)
	in.Note = trimLimit(in.Note, 2048)
	if in.MonthlyCostCents < 0 {
		in.MonthlyCostCents = 0
	}
	tags := normalizeTags(in.Tags)
	tagsJSON, _ := json.Marshal(tags)
	renewAt := sql.NullString{}
	if in.RenewAt != nil && !in.RenewAt.IsZero() {
		renewAt = sql.NullString{String: in.RenewAt.UTC().Format(time.RFC3339), Valid: true}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO node_metadata(node_id, provider, region, country, city, latitude, longitude, tags_json, monthly_cost_cents, currency, renew_cycle, renew_at, note, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(node_id) DO UPDATE SET
	provider = excluded.provider,
	region = excluded.region,
	country = excluded.country,
	city = excluded.city,
	latitude = excluded.latitude,
	longitude = excluded.longitude,
	tags_json = excluded.tags_json,
	monthly_cost_cents = excluded.monthly_cost_cents,
	currency = excluded.currency,
	renew_cycle = excluded.renew_cycle,
	renew_at = excluded.renew_at,
	note = excluded.note,
	updated_at = excluded.updated_at;
`, nodeID, in.Provider, in.Region, in.Country, in.City, in.Latitude, in.Longitude, string(tagsJSON), in.MonthlyCostCents, in.Currency, in.RenewCycle, renewAt, in.Note, now)
	return err
}

func (s *Store) GetNodeMetadata(ctx context.Context, nodeID int64) (NodeMetadata, bool, error) {
	if nodeID <= 0 {
		return NodeMetadata{}, false, fmt.Errorf("invalid node id")
	}
	var out NodeMetadata
	var tagsJSON string
	var renewAt sql.NullString
	var updatedAt string
	err := s.db.QueryRowContext(ctx, `
SELECT node_id, provider, region, country, city, latitude, longitude, tags_json, monthly_cost_cents, currency, renew_cycle, renew_at, note, updated_at
FROM node_metadata
WHERE node_id = ?;
`, nodeID).Scan(&out.NodeID, &out.Provider, &out.Region, &out.Country, &out.City, &out.Latitude, &out.Longitude, &tagsJSON, &out.MonthlyCostCents, &out.Currency, &out.RenewCycle, &renewAt, &out.Note, &updatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return NodeMetadata{}, false, nil
		}
		return NodeMetadata{}, false, err
	}
	_ = json.Unmarshal([]byte(tagsJSON), &out.Tags)
	if renewAt.Valid && renewAt.String != "" {
		if t, err := time.Parse(time.RFC3339, renewAt.String); err == nil {
			out.RenewAt = &t
		}
	}
	out.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return out, true, nil
}

func (s *Store) ListNodeMetadata(ctx context.Context) (map[int64]NodeMetadata, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT node_id, provider, region, country, city, latitude, longitude, tags_json, monthly_cost_cents, currency, renew_cycle, renew_at, note, updated_at
FROM node_metadata;
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]NodeMetadata)
	for rows.Next() {
		item, err := scanNodeMetadata(rows)
		if err != nil {
			return nil, err
		}
		out[item.NodeID] = item
	}
	return out, rows.Err()
}

type nodeMetadataScanner interface {
	Scan(dest ...any) error
}

func scanNodeMetadata(row nodeMetadataScanner) (NodeMetadata, error) {
	var out NodeMetadata
	var tagsJSON string
	var renewAt sql.NullString
	var updatedAt string
	if err := row.Scan(&out.NodeID, &out.Provider, &out.Region, &out.Country, &out.City, &out.Latitude, &out.Longitude, &tagsJSON, &out.MonthlyCostCents, &out.Currency, &out.RenewCycle, &renewAt, &out.Note, &updatedAt); err != nil {
		return NodeMetadata{}, err
	}
	_ = json.Unmarshal([]byte(tagsJSON), &out.Tags)
	if renewAt.Valid && renewAt.String != "" {
		if t, err := time.Parse(time.RFC3339, renewAt.String); err == nil {
			out.RenewAt = &t
		}
	}
	out.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return out, nil
}

func (s *Store) UpsertOpsAlert(ctx context.Context, nodeID *int64, kind, severity, message, dedupeKey string, at time.Time) (int64, error) {
	kind = trimLimit(kind, 128)
	severity = trimLimit(defaultText(severity, "warning"), 32)
	message = trimLimit(message, 2048)
	dedupeKey = trimLimit(dedupeKey, 512)
	if kind == "" || dedupeKey == "" {
		return 0, fmt.Errorf("invalid alert")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	node := sql.NullInt64{}
	if nodeID != nil && *nodeID > 0 {
		node = sql.NullInt64{Int64: *nodeID, Valid: true}
	}
	now := at.UTC().Format(time.RFC3339)
	var id int64
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	err = tx.QueryRowContext(ctx, `
SELECT id
FROM ops_alerts
WHERE dedupe_key = ? AND status = 'active'
ORDER BY id DESC
LIMIT 1;
`, dedupeKey).Scan(&id)
	if err == nil && id > 0 {
		_, err = tx.ExecContext(ctx, `
UPDATE ops_alerts
SET node_id = ?,
	kind = ?,
	severity = ?,
	message = ?,
	last_seen_at = ?
WHERE id = ?;
`, node, kind, severity, message, now, id)
		if err != nil {
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		_ = s.queueOpsAlertNotification(ctx, id, kind, severity, message, now)
		return id, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	if err = tx.QueryRowContext(ctx, `
INSERT INTO ops_alerts(node_id, kind, severity, status, message, dedupe_key, first_seen_at, last_seen_at)
VALUES (?, ?, ?, 'active', ?, ?, ?, ?)
RETURNING id;
`, node, kind, severity, message, dedupeKey, now, now).Scan(&id); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	_ = s.queueOpsAlertNotification(ctx, id, kind, severity, message, now)
	return id, nil
}

func (s *Store) queueOpsAlertNotification(ctx context.Context, opsAlertID int64, kind, severity, message, createdAt string) error {
	if opsAlertID <= 0 {
		return nil
	}
	kind = trimLimit(kind, 128)
	severity = trimLimit(defaultText(severity, "warning"), 32)
	message = trimLimit(message, 1800)
	if message == "" {
		message = kind
	}
	body := trimLimit(fmt.Sprintf("ops %s: %s", kind, message), 2048)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO alerts(type, threshold, channel, message, dedupe_key, created_at)
VALUES ('system', ?, 'telegram', ?, ?, ?)
ON CONFLICT(dedupe_key) DO UPDATE SET
	threshold = excluded.threshold,
	message = excluded.message
WHERE alerts.sent_at IS NULL;
`, severity, body, fmt.Sprintf("ops_alert:%d", opsAlertID), createdAt)
	return err
}

func (s *Store) ResolveOpsAlert(ctx context.Context, dedupeKey string, at time.Time) (bool, error) {
	dedupeKey = strings.TrimSpace(dedupeKey)
	if dedupeKey == "" {
		return false, fmt.Errorf("invalid dedupe key")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	now := at.UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
UPDATE ops_alerts
SET status = 'resolved', resolved_at = ?, last_seen_at = ?
WHERE dedupe_key = ? AND status = 'active';
`, now, now, dedupeKey)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) ListOpsAlerts(ctx context.Context, status string, limit int) ([]OpsAlert, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	status = strings.TrimSpace(status)
	args := []any{}
	query := `
SELECT id, node_id, kind, severity, status, message, dedupe_key, first_seen_at, last_seen_at, resolved_at
FROM ops_alerts
`
	if status != "" {
		query += `WHERE status = ? `
		args = append(args, status)
	}
	query += `ORDER BY last_seen_at DESC, id DESC LIMIT ?;`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]OpsAlert, 0, limit)
	for rows.Next() {
		var a OpsAlert
		var nodeID sql.NullInt64
		var firstSeen, lastSeen string
		var resolved sql.NullString
		if err := rows.Scan(&a.ID, &nodeID, &a.Kind, &a.Severity, &a.Status, &a.Message, &a.DedupeKey, &firstSeen, &lastSeen, &resolved); err != nil {
			return nil, err
		}
		if nodeID.Valid {
			v := nodeID.Int64
			a.NodeID = &v
		}
		a.FirstSeenAt, _ = time.Parse(time.RFC3339, firstSeen)
		a.LastSeenAt, _ = time.Parse(time.RFC3339, lastSeen)
		if resolved.Valid && resolved.String != "" {
			if t, err := time.Parse(time.RFC3339, resolved.String); err == nil {
				a.ResolvedAt = &t
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// opsCleanupMaxBatches bounds how many LIMIT-batchSize delete batches a single
// CleanupOpsData call runs per table, so a large backlog drains across prune
// ticks without one tick monopolizing the write lock.
const opsCleanupMaxBatches = 20

func (s *Store) CleanupOpsData(ctx context.Context, now time.Time, sampleDays int, detailHours int, probeDays int, alertResolvedDays int, batchSize int) (OpsCleanupCounts, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if batchSize <= 0 || batchSize > 10000 {
		batchSize = 5000
	}
	var out OpsCleanupCounts
	var err error
	if sampleDays > 0 {
		out.MetricSamples, err = deleteOlderThanBatched(ctx, s.db, "node_metric_samples", "sampled_at", now.AddDate(0, 0, -sampleDays), batchSize, "")
		if err != nil {
			return out, err
		}
	}
	if detailHours > 0 {
		out.MetricDetails, err = deleteOlderThanBatched(ctx, s.db, "node_metric_details", "sampled_at", now.Add(-time.Duration(detailHours)*time.Hour), batchSize, "")
		if err != nil {
			return out, err
		}
	}
	if probeDays > 0 {
		out.ProbeResults, err = deleteOlderThanBatched(ctx, s.db, "node_probe_results", "checked_at", now.AddDate(0, 0, -probeDays), batchSize, "")
		if err != nil {
			return out, err
		}
	}
	if alertResolvedDays > 0 {
		out.OpsAlerts, err = deleteOlderThanBatched(ctx, s.db, "ops_alerts", "resolved_at", now.AddDate(0, 0, -alertResolvedDays), batchSize, "status = 'resolved' AND resolved_at IS NOT NULL")
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// deleteOlderThanBatched repeats the single-batch delete until a batch comes
// back smaller than batchSize (backlog drained) or opsCleanupMaxBatches is
// reached (latency bound; the remainder drains on subsequent prune ticks).
func deleteOlderThanBatched(ctx context.Context, db *sql.DB, table, column string, cutoff time.Time, batchSize int, extraWhere string) (int64, error) {
	var total int64
	for i := 0; i < opsCleanupMaxBatches; i++ {
		n, err := deleteOlderThan(ctx, db, table, column, cutoff, batchSize, extraWhere)
		total += n
		if err != nil {
			return total, err
		}
		if n < int64(batchSize) {
			break
		}
	}
	return total, nil
}

func deleteOlderThan(ctx context.Context, db *sql.DB, table, column string, cutoff time.Time, limit int, extraWhere string) (int64, error) {
	if !cleanupSQLIdentRE.MatchString(table) || !cleanupSQLIdentRE.MatchString(column) {
		return 0, fmt.Errorf("invalid cleanup identifier")
	}
	where := fmt.Sprintf("%s < ?", column)
	if strings.TrimSpace(extraWhere) != "" {
		where = extraWhere + " AND " + where
	}
	query := fmt.Sprintf(`DELETE FROM %s WHERE id IN (SELECT id FROM %s WHERE %s ORDER BY %s ASC LIMIT ?);`, table, table, where, column)
	args := []any{}
	if strings.TrimSpace(extraWhere) != "" {
		args = append(args, cutoff.UTC().Format(time.RFC3339))
	} else {
		args = append(args, cutoff.UTC().Format(time.RFC3339))
	}
	args = append(args, limit)
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func normalizeTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	seen := map[string]struct{}{}
	for _, tag := range tags {
		tag = trimLimit(tag, 48)
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
		if len(out) >= 20 {
			break
		}
	}
	return out
}

func defaultText(v, fallback string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}
	return v
}
