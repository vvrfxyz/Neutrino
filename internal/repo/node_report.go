package repo

import (
	"context"
	"database/sql"
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
	ReportedAt string                  `json:"reported_at,omitempty"`
	Port       *int                    `json:"port,omitempty"`
	SNI        string                  `json:"sni,omitempty"`
	PublicKey  string                  `json:"public_key,omitempty"`
	ShortID    string                  `json:"short_id,omitempty"`
	Metrics    *NodeReportMetricsInput `json:"metrics,omitempty"`
}

type NodeReportMetricsInput struct {
	CPUPercent    float64 `json:"cpu_percent,omitempty"`
	MemoryBytes   int64   `json:"memory_bytes,omitempty"`
	InboundBPS    float64 `json:"inbound_bps,omitempty"`
	OutboundBPS   float64 `json:"outbound_bps,omitempty"`
	MonthKey      string  `json:"month_key,omitempty"`
	MonthTimezone string  `json:"month_timezone,omitempty"`
	NetRXTotal    int64   `json:"net_rx_total_bytes,omitempty"`
	NetTXTotal    int64   `json:"net_tx_total_bytes,omitempty"`
	NetSource     string  `json:"net_counter_source,omitempty"`

	DiskTotalBytes  int64   `json:"disk_total_bytes,omitempty"`
	DiskUsedBytes   int64   `json:"disk_used_bytes,omitempty"`
	DiskFreeBytes   int64   `json:"disk_free_bytes,omitempty"`
	DiskUsedPercent float64 `json:"disk_used_percent,omitempty"`

	UptimeSec    int64 `json:"uptime_sec,omitempty"`
	QueueBytes   int64 `json:"queue_bytes,omitempty"`
	QueueBatches int64 `json:"queue_batches,omitempty"`
	Goroutines   int   `json:"goroutines,omitempty"`
}

// ApplyNodeReport updates node fields that are needed for subscription rendering.
// It never accepts private keys.
func (s *Store) ApplyNodeReport(ctx context.Context, nodeID int64, in NodeReportInput) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	reportedAt := normalizeNodeReportTime(in.ReportedAt, time.Now().UTC())
	if in.Metrics != nil {
		if err := s.UpsertNodeRuntimeMetrics(ctx, nodeID, reportedAt, *in.Metrics); err != nil {
			return err
		}
		if err := s.UpsertNodeMonthlyUsageFromReport(ctx, nodeID, *in.Metrics, reportedAt); err != nil {
			return err
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
		return nil
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
	return err
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
