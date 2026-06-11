package repo

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type NodeMetricSample struct {
	ID                   int64      `json:"id"`
	NodeID               int64      `json:"node_id"`
	SampledAt            time.Time  `json:"sampled_at"`
	CPUPercent           float64    `json:"cpu_percent"`
	Load1                float64    `json:"load1"`
	Load5                float64    `json:"load5"`
	Load15               float64    `json:"load15"`
	MemoryUsedBytes      int64      `json:"memory_used_bytes"`
	MemoryTotalBytes     int64      `json:"memory_total_bytes"`
	MemoryAvailableBytes int64      `json:"memory_available_bytes"`
	SwapUsedBytes        int64      `json:"swap_used_bytes"`
	SwapTotalBytes       int64      `json:"swap_total_bytes"`
	DiskTotalBytes       int64      `json:"disk_total_bytes"`
	DiskUsedBytes        int64      `json:"disk_used_bytes"`
	DiskFreeBytes        int64      `json:"disk_free_bytes"`
	DiskUsedPercent      float64    `json:"disk_used_percent"`
	DiskReadBPS          float64    `json:"disk_read_bps"`
	DiskWriteBPS         float64    `json:"disk_write_bps"`
	InboundBPS           float64    `json:"inbound_bps"`
	OutboundBPS          float64    `json:"outbound_bps"`
	TCPConnections       int        `json:"tcp_connections"`
	UDPConnections       int        `json:"udp_connections"`
	ProcessCount         int        `json:"process_count"`
	SystemUptimeSec      int64      `json:"system_uptime_sec"`
	AgentUptimeSec       int64      `json:"agent_uptime_sec"`
	BootTime             *time.Time `json:"boot_time,omitempty"`
	QueueBytes           int64      `json:"queue_bytes"`
	QueueBatches         int64      `json:"queue_batches"`
	Goroutines           int        `json:"goroutines"`
}

type NodeMetricSeriesPoint struct {
	BucketStart          time.Time `json:"bucket_start"`
	SampledAt            time.Time `json:"sampled_at,omitempty"`
	CPUAvg               float64   `json:"cpu_avg"`
	CPUMax               float64   `json:"cpu_max"`
	MemoryUsedBytesAvg   float64   `json:"memory_used_bytes_avg"`
	MemoryUsedBytesMax   int64     `json:"memory_used_bytes_max"`
	MemoryUsedPercentAvg float64   `json:"memory_used_percent_avg"`
	MemoryUsedPercentMax float64   `json:"memory_used_percent_max"`
	DiskUsedPercentAvg   float64   `json:"disk_used_percent_avg"`
	DiskUsedPercentMax   float64   `json:"disk_used_percent_max"`
	InboundBPSAvg        float64   `json:"inbound_bps_avg"`
	InboundBPSMax        float64   `json:"inbound_bps_max"`
	OutboundBPSAvg       float64   `json:"outbound_bps_avg"`
	OutboundBPSMax       float64   `json:"outbound_bps_max"`
	TCPConnectionsAvg    float64   `json:"tcp_connections_avg"`
	TCPConnectionsMax    int       `json:"tcp_connections_max"`
	UDPConnectionsAvg    float64   `json:"udp_connections_avg"`
	UDPConnectionsMax    int       `json:"udp_connections_max"`
	QueueBatchesMax      int       `json:"queue_batches_max"`
	SampleCount          int       `json:"sample_count"`
}

type NodeMetricDetails struct {
	ID        int64           `json:"id"`
	NodeID    int64           `json:"node_id"`
	SampledAt time.Time       `json:"sampled_at"`
	Data      json.RawMessage `json:"data"`
}

type NodeStaticFacts struct {
	ID               int64           `json:"id"`
	NodeID           int64           `json:"node_id"`
	ReportedAt       time.Time       `json:"reported_at"`
	DataHash         string          `json:"data_hash"`
	OSName           string          `json:"os_name"`
	OSVersion        string          `json:"os_version"`
	Kernel           string          `json:"kernel"`
	KernelVersion    string          `json:"kernel_version"`
	Arch             string          `json:"arch"`
	Hostname         string          `json:"hostname"`
	Virtualization   string          `json:"virtualization"`
	CPUModel         string          `json:"cpu_model"`
	CPUPhysicalCores int             `json:"cpu_physical_cores"`
	CPULogicalCores  int             `json:"cpu_logical_cores"`
	AgentVersion     string          `json:"agent_version"`
	XrayVersion      string          `json:"xray_version"`
	FactsJSON        json.RawMessage `json:"facts_json"`
}

type NodeProbeResult struct {
	ID          int64     `json:"id"`
	NodeID      int64     `json:"node_id"`
	Kind        string    `json:"kind"`
	Target      string    `json:"target"`
	Success     bool      `json:"success"`
	LatencyMS   float64   `json:"latency_ms"`
	StatusCode  *int      `json:"status_code,omitempty"`
	Error       string    `json:"error,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
	SourceJobID *int64    `json:"source_job_id,omitempty"`
}

type InsertNodeProbeResultInput struct {
	Kind        string
	Target      string
	Success     bool
	LatencyMS   float64
	StatusCode  *int
	Error       string
	CheckedAt   time.Time
	SourceJobID *int64
}

type NodeMetricHistoryInput struct {
	NodeID    int64
	SampledAt time.Time
	Metrics   NodeReportMetricsInput
}

func (s *Store) InsertNodeMetricSample(ctx context.Context, nodeID int64, sampledAt time.Time, in NodeReportMetricsInput) error {
	return insertNodeMetricSampleExec(ctx, s.db, nodeID, sampledAt, in)
}

func insertNodeMetricSampleExec(ctx context.Context, q execContext, nodeID int64, sampledAt time.Time, in NodeReportMetricsInput) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	if sampledAt.IsZero() {
		sampledAt = time.Now().UTC()
	}
	if in.AgentUptimeSec == 0 && in.UptimeSec > 0 {
		in.AgentUptimeSec = in.UptimeSec
	}
	if in.UptimeSec == 0 && in.AgentUptimeSec > 0 {
		in.UptimeSec = in.AgentUptimeSec
	}
	bootTime := sql.NullString{}
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(in.BootTime)); err == nil && !t.IsZero() {
		bootTime = sql.NullString{String: t.UTC().Format(time.RFC3339), Valid: true}
	}
	_, err := q.ExecContext(ctx, `
INSERT INTO node_metric_samples(
	node_id, sampled_at, cpu_percent, load1, load5, load15,
	memory_used_bytes, memory_total_bytes, memory_available_bytes,
	swap_used_bytes, swap_total_bytes,
	disk_total_bytes, disk_used_bytes, disk_free_bytes, disk_used_percent,
	disk_read_bps, disk_write_bps, inbound_bps, outbound_bps,
	tcp_connections, udp_connections, process_count,
	system_uptime_sec, agent_uptime_sec, boot_time,
	queue_bytes, queue_batches, goroutines
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
`, nodeID, sampledAt.UTC().Format(time.RFC3339),
		in.CPUPercent, in.Load1, in.Load5, in.Load15,
		in.MemoryBytes, in.MemoryTotalBytes, in.MemoryAvailableBytes,
		in.SwapUsedBytes, in.SwapTotalBytes,
		in.DiskTotalBytes, in.DiskUsedBytes, in.DiskFreeBytes, in.DiskUsedPercent,
		in.DiskReadBPS, in.DiskWriteBPS, in.InboundBPS, in.OutboundBPS,
		in.TCPConnections, in.UDPConnections, in.ProcessCount,
		in.SystemUptimeSec, in.AgentUptimeSec, bootTime,
		in.QueueBytes, in.QueueBatches, in.Goroutines)
	return err
}

func (s *Store) InsertNodeMetricDetails(ctx context.Context, nodeID int64, sampledAt time.Time, raw json.RawMessage) error {
	return insertNodeMetricDetailsExec(ctx, s.db, nodeID, sampledAt, raw)
}

func insertNodeMetricDetailsExec(ctx context.Context, q execContext, nodeID int64, sampledAt time.Time, raw json.RawMessage) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return nil
	}
	if !json.Valid(raw) {
		return fmt.Errorf("invalid metric details json")
	}
	if sampledAt.IsZero() {
		sampledAt = time.Now().UTC()
	}
	_, err := q.ExecContext(ctx, `
INSERT INTO node_metric_details(node_id, sampled_at, data_json)
VALUES (?, ?, ?);
`, nodeID, sampledAt.UTC().Format(time.RFC3339), string(raw))
	return err
}

// ErrBatchCommittedWithItemErrors marks a batch insert whose transaction
// committed even though some items failed. Callers deciding between retry and
// dequeue must treat this as "written": retrying would duplicate the items
// that succeeded.
var ErrBatchCommittedWithItemErrors = errors.New("batch committed with item errors")

func (s *Store) InsertNodeMetricHistoryBatch(ctx context.Context, items []NodeMetricHistoryInput) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var errs []error
	for _, item := range items {
		if err := insertNodeMetricSampleExec(ctx, tx, item.NodeID, item.SampledAt, item.Metrics); err != nil {
			errs = append(errs, fmt.Errorf("sample node_id=%d: %w", item.NodeID, err))
			continue
		}
		if len(item.Metrics.Details) == 0 {
			continue
		}
		if err := insertNodeMetricDetailsExec(ctx, tx, item.NodeID, item.SampledAt, item.Metrics.Details); err != nil {
			errs = append(errs, fmt.Errorf("details node_id=%d: %w", item.NodeID, err))
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrBatchCommittedWithItemErrors, errors.Join(errs...))
	}
	return nil
}

func (s *Store) UpsertNodeStaticFacts(ctx context.Context, nodeID int64, reportedAt time.Time, in NodeStaticFactsInput) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	if reportedAt.IsZero() {
		reportedAt = time.Now().UTC()
	}
	normalized := normalizeStaticFactsInput(in)
	hash, factsJSON, err := staticFactsHash(normalized)
	if err != nil {
		return err
	}
	now := reportedAt.UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(ctx, `
INSERT INTO node_static_facts(
	node_id, reported_at, data_hash, os_name, os_version, kernel, kernel_version,
	arch, hostname, virtualization, cpu_model, cpu_physical_cores, cpu_logical_cores,
	agent_version, xray_version, facts_json
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(node_id, data_hash) DO UPDATE SET
	reported_at = excluded.reported_at;
`, nodeID, now, hash,
		normalized.OSName, normalized.OSVersion, normalized.Kernel, normalized.KernelVersion,
		normalized.Arch, normalized.Hostname, normalized.Virtualization, normalized.CPUModel,
		normalized.CPUPhysicalCores, normalized.CPULogicalCores, normalized.AgentVersion, normalized.XrayVersion, factsJSON)
	return err
}

func (s *Store) ListNodeMetricSeries(ctx context.Context, nodeID int64, rangeName string, step string, now time.Time) ([]NodeMetricSeriesPoint, error) {
	if nodeID <= 0 {
		return nil, fmt.Errorf("invalid node id")
	}
	duration, err := metricRangeDuration(rangeName)
	if err != nil {
		return nil, err
	}
	stepSec, raw, err := metricStepSeconds(step)
	if err != nil {
		return nil, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := now.UTC().Add(-duration).Format(time.RFC3339)
	if raw {
		return s.listRawNodeMetricSeries(ctx, nodeID, cutoff)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT
	datetime((unixepoch(sampled_at) / ?) * ?, 'unixepoch') AS bucket_start,
	AVG(cpu_percent), MAX(cpu_percent),
	AVG(memory_used_bytes), MAX(memory_used_bytes),
	AVG(CASE WHEN memory_total_bytes > 0 THEN memory_used_bytes * 100.0 / memory_total_bytes ELSE 0 END),
	MAX(CASE WHEN memory_total_bytes > 0 THEN memory_used_bytes * 100.0 / memory_total_bytes ELSE 0 END),
	AVG(disk_used_percent), MAX(disk_used_percent),
	AVG(inbound_bps), MAX(inbound_bps),
	AVG(outbound_bps), MAX(outbound_bps),
	AVG(tcp_connections), MAX(tcp_connections),
	AVG(udp_connections), MAX(udp_connections),
	MAX(queue_batches),
	COUNT(1)
FROM node_metric_samples
WHERE node_id = ? AND sampled_at >= ?
GROUP BY bucket_start
ORDER BY bucket_start ASC
LIMIT 5000;
`, stepSec, stepSec, nodeID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMetricSeriesRows(rows)
}

func (s *Store) listRawNodeMetricSeries(ctx context.Context, nodeID int64, cutoff string) ([]NodeMetricSeriesPoint, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT sampled_at,
	cpu_percent, cpu_percent,
	memory_used_bytes, memory_used_bytes,
	CASE WHEN memory_total_bytes > 0 THEN memory_used_bytes * 100.0 / memory_total_bytes ELSE 0 END,
	CASE WHEN memory_total_bytes > 0 THEN memory_used_bytes * 100.0 / memory_total_bytes ELSE 0 END,
	disk_used_percent, disk_used_percent,
	inbound_bps, inbound_bps,
	outbound_bps, outbound_bps,
	tcp_connections, tcp_connections,
	udp_connections, udp_connections,
	queue_batches,
	1
FROM node_metric_samples
WHERE node_id = ? AND sampled_at >= ?
ORDER BY sampled_at ASC
LIMIT 5000;
`, nodeID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMetricSeriesRows(rows)
}

func (s *Store) GetLatestNodeMetricDetails(ctx context.Context, nodeID int64) (NodeMetricDetails, bool, error) {
	if nodeID <= 0 {
		return NodeMetricDetails{}, false, fmt.Errorf("invalid node id")
	}
	var out NodeMetricDetails
	var sampledAt string
	var raw string
	err := s.db.QueryRowContext(ctx, `
SELECT id, node_id, sampled_at, data_json
FROM node_metric_details
WHERE node_id = ?
ORDER BY sampled_at DESC, id DESC
LIMIT 1;
`, nodeID).Scan(&out.ID, &out.NodeID, &sampledAt, &raw)
	if err != nil {
		if err == sql.ErrNoRows {
			return NodeMetricDetails{}, false, nil
		}
		return NodeMetricDetails{}, false, err
	}
	out.SampledAt, _ = time.Parse(time.RFC3339, sampledAt)
	out.Data = json.RawMessage(raw)
	return out, true, nil
}

func (s *Store) GetLatestNodeStaticFacts(ctx context.Context, nodeID int64) (NodeStaticFacts, bool, error) {
	items, err := s.ListNodeStaticFacts(ctx, nodeID, 1)
	if err != nil || len(items) == 0 {
		return NodeStaticFacts{}, false, err
	}
	return items[0], true, nil
}

func (s *Store) ListNodeStaticFacts(ctx context.Context, nodeID int64, limit int) ([]NodeStaticFacts, error) {
	if nodeID <= 0 {
		return nil, fmt.Errorf("invalid node id")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, node_id, reported_at, data_hash, os_name, os_version, kernel, kernel_version,
	arch, hostname, virtualization, cpu_model, cpu_physical_cores, cpu_logical_cores,
	agent_version, xray_version, facts_json
FROM node_static_facts
WHERE node_id = ?
ORDER BY reported_at DESC, id DESC
LIMIT ?;
`, nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NodeStaticFacts, 0, limit)
	for rows.Next() {
		item, err := scanNodeStaticFacts(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) InsertNodeProbeResult(ctx context.Context, nodeID int64, in InsertNodeProbeResultInput) (int64, error) {
	if nodeID <= 0 {
		return 0, fmt.Errorf("invalid node id")
	}
	kind := trimLimit(in.Kind, 64)
	target := trimLimit(in.Target, 512)
	if kind == "" || target == "" {
		return 0, fmt.Errorf("invalid probe result")
	}
	if in.LatencyMS < 0 {
		in.LatencyMS = 0
	}
	if in.CheckedAt.IsZero() {
		in.CheckedAt = time.Now().UTC()
	}
	success := 0
	if in.Success {
		success = 1
	}
	statusCode := sql.NullInt64{}
	if in.StatusCode != nil {
		statusCode = sql.NullInt64{Int64: int64(*in.StatusCode), Valid: true}
	}
	sourceJobID := sql.NullInt64{}
	if in.SourceJobID != nil && *in.SourceJobID > 0 {
		sourceJobID = sql.NullInt64{Int64: *in.SourceJobID, Valid: true}
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `
INSERT INTO node_probe_results(node_id, kind, target, success, latency_ms, status_code, error, checked_at, source_job_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;
`, nodeID, kind, target, success, in.LatencyMS, statusCode, trimLimit(in.Error, 2048), in.CheckedAt.UTC().Format(time.RFC3339), sourceJobID).Scan(&id)
	return id, err
}

func (s *Store) ListNodeProbeResults(ctx context.Context, nodeID int64, limit int) ([]NodeProbeResult, error) {
	if nodeID <= 0 {
		return nil, fmt.Errorf("invalid node id")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, node_id, kind, target, success, latency_ms, status_code, error, checked_at, source_job_id
FROM node_probe_results
WHERE node_id = ?
ORDER BY checked_at DESC, id DESC
LIMIT ?;
`, nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NodeProbeResult, 0, limit)
	for rows.Next() {
		var r NodeProbeResult
		var success int
		var status sql.NullInt64
		var checkedAt string
		var sourceJobID sql.NullInt64
		if err := rows.Scan(&r.ID, &r.NodeID, &r.Kind, &r.Target, &success, &r.LatencyMS, &status, &r.Error, &checkedAt, &sourceJobID); err != nil {
			return nil, err
		}
		r.Success = success != 0
		if status.Valid {
			v := int(status.Int64)
			r.StatusCode = &v
		}
		if sourceJobID.Valid {
			v := sourceJobID.Int64
			r.SourceJobID = &v
		}
		r.CheckedAt, _ = time.Parse(time.RFC3339, checkedAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

func metricRangeDuration(name string) (time.Duration, error) {
	switch strings.TrimSpace(name) {
	case "", "1h":
		return time.Hour, nil
	case "6h":
		return 6 * time.Hour, nil
	case "24h":
		return 24 * time.Hour, nil
	case "7d":
		return 7 * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("unsupported metrics range")
	}
}

func metricStepSeconds(step string) (int, bool, error) {
	switch strings.TrimSpace(step) {
	case "", "raw":
		return 0, true, nil
	case "1m":
		return 60, false, nil
	case "5m":
		return 300, false, nil
	case "1h":
		return 3600, false, nil
	default:
		return 0, false, fmt.Errorf("unsupported metrics step")
	}
}

type metricSeriesScanner interface {
	Scan(dest ...any) error
}

func scanMetricSeriesRows(rows *sql.Rows) ([]NodeMetricSeriesPoint, error) {
	out := make([]NodeMetricSeriesPoint, 0)
	for rows.Next() {
		var p NodeMetricSeriesPoint
		var bucket string
		var memoryMax sql.NullInt64
		var tcpMax sql.NullInt64
		var udpMax sql.NullInt64
		var queueMax sql.NullInt64
		var sampleCount sql.NullInt64
		if err := rows.Scan(&bucket,
			&p.CPUAvg, &p.CPUMax,
			&p.MemoryUsedBytesAvg, &memoryMax,
			&p.MemoryUsedPercentAvg, &p.MemoryUsedPercentMax,
			&p.DiskUsedPercentAvg, &p.DiskUsedPercentMax,
			&p.InboundBPSAvg, &p.InboundBPSMax,
			&p.OutboundBPSAvg, &p.OutboundBPSMax,
			&p.TCPConnectionsAvg, &tcpMax,
			&p.UDPConnectionsAvg, &udpMax,
			&queueMax,
			&sampleCount,
		); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, bucket); err == nil {
			p.BucketStart = t
			p.SampledAt = t
		} else if t, err := time.Parse("2006-01-02 15:04:05", bucket); err == nil {
			p.BucketStart = t.UTC()
			p.SampledAt = p.BucketStart
		}
		if memoryMax.Valid {
			p.MemoryUsedBytesMax = memoryMax.Int64
		}
		if tcpMax.Valid {
			p.TCPConnectionsMax = int(tcpMax.Int64)
		}
		if udpMax.Valid {
			p.UDPConnectionsMax = int(udpMax.Int64)
		}
		if queueMax.Valid {
			p.QueueBatchesMax = int(queueMax.Int64)
		}
		if sampleCount.Valid {
			p.SampleCount = int(sampleCount.Int64)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type staticFactsScanner interface {
	Scan(dest ...any) error
}

func scanNodeStaticFacts(row staticFactsScanner) (NodeStaticFacts, error) {
	var item NodeStaticFacts
	var reportedAt string
	var factsJSON string
	if err := row.Scan(&item.ID, &item.NodeID, &reportedAt, &item.DataHash, &item.OSName, &item.OSVersion, &item.Kernel, &item.KernelVersion,
		&item.Arch, &item.Hostname, &item.Virtualization, &item.CPUModel, &item.CPUPhysicalCores, &item.CPULogicalCores,
		&item.AgentVersion, &item.XrayVersion, &factsJSON); err != nil {
		return NodeStaticFacts{}, err
	}
	item.ReportedAt, _ = time.Parse(time.RFC3339, reportedAt)
	item.FactsJSON = json.RawMessage(factsJSON)
	return item, nil
}

func normalizeStaticFactsInput(in NodeStaticFactsInput) NodeStaticFactsInput {
	in.OSName = trimLimit(in.OSName, 128)
	in.OSVersion = trimLimit(in.OSVersion, 256)
	in.Kernel = trimLimit(in.Kernel, 128)
	in.KernelVersion = trimLimit(in.KernelVersion, 256)
	in.Arch = trimLimit(in.Arch, 64)
	in.Hostname = trimLimit(in.Hostname, 255)
	in.Virtualization = trimLimit(in.Virtualization, 128)
	in.CPUModel = trimLimit(in.CPUModel, 255)
	in.AgentVersion = trimLimit(in.AgentVersion, 128)
	in.XrayVersion = trimLimit(in.XrayVersion, 128)
	if in.CPUPhysicalCores < 0 {
		in.CPUPhysicalCores = 0
	}
	if in.CPULogicalCores < 0 {
		in.CPULogicalCores = 0
	}
	return in
}

func staticFactsHash(in NodeStaticFactsInput) (string, string, error) {
	factsJSON, err := canonicalJSON(in.FactsJSON)
	if err != nil {
		return "", "", err
	}
	body := struct {
		OSName           string          `json:"os_name"`
		OSVersion        string          `json:"os_version"`
		Kernel           string          `json:"kernel"`
		KernelVersion    string          `json:"kernel_version"`
		Arch             string          `json:"arch"`
		Hostname         string          `json:"hostname"`
		Virtualization   string          `json:"virtualization"`
		CPUModel         string          `json:"cpu_model"`
		CPUPhysicalCores int             `json:"cpu_physical_cores"`
		CPULogicalCores  int             `json:"cpu_logical_cores"`
		AgentVersion     string          `json:"agent_version"`
		XrayVersion      string          `json:"xray_version"`
		FactsJSON        json.RawMessage `json:"facts_json"`
	}{
		OSName: in.OSName, OSVersion: in.OSVersion, Kernel: in.Kernel, KernelVersion: in.KernelVersion,
		Arch: in.Arch, Hostname: in.Hostname, Virtualization: in.Virtualization, CPUModel: in.CPUModel,
		CPUPhysicalCores: in.CPUPhysicalCores, CPULogicalCores: in.CPULogicalCores,
		AgentVersion: in.AgentVersion, XrayVersion: in.XrayVersion, FactsJSON: factsJSON,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), string(factsJSON), nil
}

func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	if !json.Valid(raw) {
		return json.RawMessage(`{}`), nil
	}
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}
