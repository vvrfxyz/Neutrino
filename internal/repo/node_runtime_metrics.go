package repo

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type NodeRuntimeMetrics struct {
	NodeID          int64
	CPUPercent      float64
	MemoryBytes     int64
	InboundBPS      float64
	OutboundBPS     float64
	DiskTotalBytes  int64
	DiskUsedBytes   int64
	DiskFreeBytes   int64
	DiskUsedPercent float64
	UptimeSec       int64
	QueueBytes      int64
	QueueBatches    int64
	Goroutines      int
	UpdatedAt       time.Time
}

func (s *Store) UpsertNodeRuntimeMetrics(ctx context.Context, nodeID int64, reportedAt time.Time, in NodeReportMetricsInput) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	if in.CPUPercent < 0 {
		in.CPUPercent = 0
	}
	if in.CPUPercent > 1000 {
		in.CPUPercent = 1000
	}
	if in.MemoryBytes < 0 {
		in.MemoryBytes = 0
	}
	if in.InboundBPS < 0 {
		in.InboundBPS = 0
	}
	if in.OutboundBPS < 0 {
		in.OutboundBPS = 0
	}
	if in.DiskTotalBytes < 0 {
		in.DiskTotalBytes = 0
	}
	if in.DiskUsedBytes < 0 {
		in.DiskUsedBytes = 0
	}
	if in.DiskFreeBytes < 0 {
		in.DiskFreeBytes = 0
	}
	if in.DiskUsedPercent < 0 {
		in.DiskUsedPercent = 0
	}
	if in.DiskUsedPercent > 100 {
		in.DiskUsedPercent = 100
	}
	if in.UptimeSec < 0 {
		in.UptimeSec = 0
	}
	if in.QueueBytes < 0 {
		in.QueueBytes = 0
	}
	if in.QueueBatches < 0 {
		in.QueueBatches = 0
	}
	if in.Goroutines < 0 {
		in.Goroutines = 0
	}
	if in.Goroutines > 100000 {
		in.Goroutines = 100000
	}

	if reportedAt.IsZero() {
		reportedAt = time.Now().UTC()
	}
	now := reportedAt.UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO node_runtime_metrics(
	node_id,
	cpu_percent,
	memory_bytes,
	inbound_bps,
	outbound_bps,
	disk_total_bytes,
	disk_used_bytes,
	disk_free_bytes,
	disk_used_percent,
	uptime_sec,
	queue_bytes,
	queue_batches,
	goroutines,
	updated_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(node_id) DO UPDATE SET
	cpu_percent = excluded.cpu_percent,
	memory_bytes = excluded.memory_bytes,
	inbound_bps = excluded.inbound_bps,
	outbound_bps = excluded.outbound_bps,
	disk_total_bytes = excluded.disk_total_bytes,
	disk_used_bytes = excluded.disk_used_bytes,
	disk_free_bytes = excluded.disk_free_bytes,
	disk_used_percent = excluded.disk_used_percent,
	uptime_sec = excluded.uptime_sec,
	queue_bytes = excluded.queue_bytes,
	queue_batches = excluded.queue_batches,
	goroutines = excluded.goroutines,
	updated_at = excluded.updated_at;
`, nodeID,
		in.CPUPercent,
		in.MemoryBytes,
		in.InboundBPS,
		in.OutboundBPS,
		in.DiskTotalBytes,
		in.DiskUsedBytes,
		in.DiskFreeBytes,
		in.DiskUsedPercent,
		in.UptimeSec,
		in.QueueBytes,
		in.QueueBatches,
		in.Goroutines,
		now,
	)
	return err
}

func (s *Store) ListNodeRuntimeMetrics(ctx context.Context) (map[int64]NodeRuntimeMetrics, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT node_id,
	cpu_percent,
	memory_bytes,
	inbound_bps,
	outbound_bps,
	disk_total_bytes,
	disk_used_bytes,
	disk_free_bytes,
	disk_used_percent,
	uptime_sec,
	queue_bytes,
	queue_batches,
	goroutines,
	updated_at
FROM node_runtime_metrics;
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]NodeRuntimeMetrics)
	for rows.Next() {
		var m NodeRuntimeMetrics
		var cpuPercent sql.NullFloat64
		var memoryBytes sql.NullInt64
		var inBPS sql.NullFloat64
		var outBPS sql.NullFloat64
		var diskTotal sql.NullInt64
		var diskUsed sql.NullInt64
		var diskFree sql.NullInt64
		var diskUsedPerc sql.NullFloat64
		var uptime sql.NullInt64
		var queueBytes sql.NullInt64
		var queueBatches sql.NullInt64
		var goroutines sql.NullInt64
		var updatedAt string
		if err := rows.Scan(
			&m.NodeID,
			&cpuPercent,
			&memoryBytes,
			&inBPS,
			&outBPS,
			&diskTotal,
			&diskUsed,
			&diskFree,
			&diskUsedPerc,
			&uptime,
			&queueBytes,
			&queueBatches,
			&goroutines,
			&updatedAt,
		); err != nil {
			return nil, err
		}
		if cpuPercent.Valid {
			m.CPUPercent = cpuPercent.Float64
		}
		if memoryBytes.Valid {
			m.MemoryBytes = memoryBytes.Int64
		}
		if inBPS.Valid {
			m.InboundBPS = inBPS.Float64
		}
		if outBPS.Valid {
			m.OutboundBPS = outBPS.Float64
		}
		if diskTotal.Valid {
			m.DiskTotalBytes = diskTotal.Int64
		}
		if diskUsed.Valid {
			m.DiskUsedBytes = diskUsed.Int64
		}
		if diskFree.Valid {
			m.DiskFreeBytes = diskFree.Int64
		}
		if diskUsedPerc.Valid {
			m.DiskUsedPercent = diskUsedPerc.Float64
		}
		if uptime.Valid {
			m.UptimeSec = uptime.Int64
		}
		if queueBytes.Valid {
			m.QueueBytes = queueBytes.Int64
		}
		if queueBatches.Valid {
			m.QueueBatches = queueBatches.Int64
		}
		if goroutines.Valid {
			m.Goroutines = int(goroutines.Int64)
		}
		m.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
		out[m.NodeID] = m
	}
	return out, rows.Err()
}
