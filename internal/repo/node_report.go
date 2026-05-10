package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// UpdateNodeObservedIP records the best-effort IP address observed from the node's mTLS connection.
func (s *Store) UpdateNodeObservedIP(ctx context.Context, nodeID int64, ip string) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
UPDATE nodes
SET observed_ip = ?,
	updated_at = ?
WHERE id = ?;
`, ip, now, nodeID)
	return err
}

type NodeReportInput struct {
	ReportedAt     string                  `json:"reported_at,omitempty"`
	Port           *int                    `json:"port,omitempty"`
	SNI            string                  `json:"sni,omitempty"`
	PublicKey      string                  `json:"public_key,omitempty"`
	ShortID        string                  `json:"short_id,omitempty"`
	Metrics        *NodeReportMetricsInput `json:"metrics,omitempty"`
	StaticFacts    *NodeStaticFactsInput   `json:"static_facts,omitempty"`
	OnlineSnapshot *OnlineSnapshotInput    `json:"online_snapshot,omitempty"`
}

type NodeReportMetricsInput struct {
	CPUPercent           float64 `json:"cpu_percent,omitempty"`
	Load1                float64 `json:"load1,omitempty"`
	Load5                float64 `json:"load5,omitempty"`
	Load15               float64 `json:"load15,omitempty"`
	MemoryBytes          int64   `json:"memory_bytes,omitempty"`
	MemoryTotalBytes     int64   `json:"memory_total_bytes,omitempty"`
	MemoryAvailableBytes int64   `json:"memory_available_bytes,omitempty"`
	SwapUsedBytes        int64   `json:"swap_used_bytes,omitempty"`
	SwapTotalBytes       int64   `json:"swap_total_bytes,omitempty"`
	InboundBPS           float64 `json:"inbound_bps,omitempty"`
	OutboundBPS          float64 `json:"outbound_bps,omitempty"`
	MonthKey             string  `json:"month_key,omitempty"`
	MonthTimezone        string  `json:"month_timezone,omitempty"`
	NetRXTotal           int64   `json:"net_rx_total_bytes,omitempty"`
	NetTXTotal           int64   `json:"net_tx_total_bytes,omitempty"`
	NetSource            string  `json:"net_counter_source,omitempty"`

	DiskTotalBytes  int64   `json:"disk_total_bytes,omitempty"`
	DiskUsedBytes   int64   `json:"disk_used_bytes,omitempty"`
	DiskFreeBytes   int64   `json:"disk_free_bytes,omitempty"`
	DiskUsedPercent float64 `json:"disk_used_percent,omitempty"`
	DiskReadBPS     float64 `json:"disk_read_bps,omitempty"`
	DiskWriteBPS    float64 `json:"disk_write_bps,omitempty"`
	TCPConnections  int     `json:"tcp_connections,omitempty"`
	UDPConnections  int     `json:"udp_connections,omitempty"`
	ProcessCount    int     `json:"process_count,omitempty"`

	UptimeSec         int64           `json:"uptime_sec,omitempty"`
	SystemUptimeSec   int64           `json:"system_uptime_sec,omitempty"`
	AgentUptimeSec    int64           `json:"agent_uptime_sec,omitempty"`
	BootTime          string          `json:"boot_time,omitempty"`
	QueueBytes        int64           `json:"queue_bytes,omitempty"`
	QueueBatches      int64           `json:"queue_batches,omitempty"`
	Goroutines        int             `json:"goroutines,omitempty"`
	AgentVersion      string          `json:"agent_version,omitempty"`
	XrayVersion       string          `json:"xray_version,omitempty"`
	XrayConfigVersion string          `json:"xray_config_version,omitempty"`
	Details           json.RawMessage `json:"details,omitempty"`
}

type NodeStaticFactsInput struct {
	OSName           string          `json:"os_name,omitempty"`
	OSVersion        string          `json:"os_version,omitempty"`
	Kernel           string          `json:"kernel,omitempty"`
	KernelVersion    string          `json:"kernel_version,omitempty"`
	Arch             string          `json:"arch,omitempty"`
	Hostname         string          `json:"hostname,omitempty"`
	Virtualization   string          `json:"virtualization,omitempty"`
	CPUModel         string          `json:"cpu_model,omitempty"`
	CPUPhysicalCores int             `json:"cpu_physical_cores,omitempty"`
	CPULogicalCores  int             `json:"cpu_logical_cores,omitempty"`
	AgentVersion     string          `json:"agent_version,omitempty"`
	XrayVersion      string          `json:"xray_version,omitempty"`
	FactsJSON        json.RawMessage `json:"facts_json,omitempty"`
}

// ApplyNodeReportLatest updates node fields and latest runtime/monthly state
// needed for subscription rendering and ops liveness. It never accepts private
// keys. Historical metric sample/detail writes are left to the caller so API
// report paths can buffer them without delaying heartbeat success.
func (s *Store) ApplyNodeReportLatest(ctx context.Context, nodeID int64, in NodeReportInput) (time.Time, error) {
	if nodeID <= 0 {
		return time.Time{}, fmt.Errorf("invalid node id")
	}
	reportedAt := normalizeNodeReportTime(in.ReportedAt, time.Now().UTC())
	if in.Metrics != nil {
		if err := s.UpsertNodeRuntimeMetrics(ctx, nodeID, reportedAt, *in.Metrics); err != nil {
			return time.Time{}, err
		}
		if err := s.UpsertNodeMonthlyUsageFromReport(ctx, nodeID, *in.Metrics, reportedAt); err != nil {
			return time.Time{}, err
		}
	}
	if in.StaticFacts != nil {
		if err := s.UpsertNodeStaticFacts(ctx, nodeID, reportedAt, *in.StaticFacts); err != nil {
			return time.Time{}, err
		}
	}
	if in.OnlineSnapshot != nil {
		if err := s.ApplyOnlineSnapshot(ctx, nodeID, *in.OnlineSnapshot); err != nil {
			return time.Time{}, err
		}
	}
	sni := strings.TrimSpace(in.SNI)
	pbk := strings.TrimSpace(in.PublicKey)
	sid := strings.TrimSpace(in.ShortID)
	port := sql.NullInt64{}
	if in.Port != nil && *in.Port > 0 {
		port = sql.NullInt64{Int64: int64(*in.Port), Valid: true}
	}

	// Nothing to update.
	if sni == "" && pbk == "" && sid == "" && !port.Valid {
		return reportedAt, nil
	}

	// Clamp obvious junk (avoid accidentally writing huge strings).
	if len(sni) > 255 {
		sni = sni[:255]
	}
	if len(pbk) > 512 {
		pbk = pbk[:512]
	}
	if len(sid) > 64 {
		sid = sid[:64]
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
UPDATE nodes
SET sni = COALESCE(NULLIF(?, ''), sni),
	public_key = COALESCE(NULLIF(?, ''), public_key),
	short_id = COALESCE(NULLIF(?, ''), short_id),
	port = COALESCE(?, port),
	updated_at = ?
WHERE id = ?;
`, sni, pbk, sid, port, now, nodeID)
	if err != nil {
		return time.Time{}, err
	}
	return reportedAt, nil
}

// ApplyNodeReport updates node latest state and writes metric history
// synchronously. It is kept for repo-level callers; the HTTP report path uses
// ApplyNodeReportLatest plus an async history queue.
func (s *Store) ApplyNodeReport(ctx context.Context, nodeID int64, in NodeReportInput) error {
	reportedAt, err := s.ApplyNodeReportLatest(ctx, nodeID, in)
	if err != nil {
		return err
	}
	if in.Metrics != nil {
		if err := s.InsertNodeMetricSample(ctx, nodeID, reportedAt, *in.Metrics); err != nil {
			// Historical samples must not break latest-state reporting.
			_ = err
		}
		if len(in.Metrics.Details) > 0 {
			if err := s.InsertNodeMetricDetails(ctx, nodeID, reportedAt, in.Metrics.Details); err != nil {
				_ = err
			}
		}
	}
	return nil
}

func normalizeNodeReportTime(raw string, fallback time.Time) time.Time {
	fallback = fallback.UTC()
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return fallback
	}
	parsed = parsed.UTC()
	if parsed.Before(fallback.Add(-5*time.Minute)) || parsed.After(fallback.Add(5*time.Minute)) {
		return fallback
	}
	return parsed
}
