package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type NodeRuntimeMetrics struct {
	NodeID               int64
	CPUPercent           float64
	Load1                float64
	Load5                float64
	Load15               float64
	MemoryBytes          int64
	MemoryTotalBytes     int64
	MemoryAvailableBytes int64
	SwapUsedBytes        int64
	SwapTotalBytes       int64
	InboundBPS           float64
	OutboundBPS          float64
	DiskTotalBytes       int64
	DiskUsedBytes        int64
	DiskFreeBytes        int64
	DiskUsedPercent      float64
	DiskReadBPS          float64
	DiskWriteBPS         float64
	TCPConnections       int
	UDPConnections       int
	ProcessCount         int
	UptimeSec            int64
	SystemUptimeSec      int64
	AgentUptimeSec       int64
	BootTime             *time.Time
	QueueBytes           int64
	QueueBatches         int64
	QuarantinedBatches   int64
	Goroutines           int
	AgentVersion         string
	XrayVersion          string
	XrayConfigVersion    string
	UpdatedAt            time.Time
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
	in.Load1 = clampFloatMin(in.Load1, 0)
	in.Load5 = clampFloatMin(in.Load5, 0)
	in.Load15 = clampFloatMin(in.Load15, 0)
	if in.MemoryBytes < 0 {
		in.MemoryBytes = 0
	}
	if in.MemoryTotalBytes < 0 {
		in.MemoryTotalBytes = 0
	}
	if in.MemoryAvailableBytes < 0 {
		in.MemoryAvailableBytes = 0
	}
	if in.SwapUsedBytes < 0 {
		in.SwapUsedBytes = 0
	}
	if in.SwapTotalBytes < 0 {
		in.SwapTotalBytes = 0
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
	in.DiskReadBPS = clampFloatMin(in.DiskReadBPS, 0)
	in.DiskWriteBPS = clampFloatMin(in.DiskWriteBPS, 0)
	if in.TCPConnections < 0 {
		in.TCPConnections = 0
	}
	if in.UDPConnections < 0 {
		in.UDPConnections = 0
	}
	if in.ProcessCount < 0 {
		in.ProcessCount = 0
	}
	if in.UptimeSec < 0 {
		in.UptimeSec = 0
	}
	if in.AgentUptimeSec < 0 {
		in.AgentUptimeSec = 0
	}
	if in.SystemUptimeSec < 0 {
		in.SystemUptimeSec = 0
	}
	if in.AgentUptimeSec == 0 && in.UptimeSec > 0 {
		in.AgentUptimeSec = in.UptimeSec
	}
	if in.UptimeSec == 0 && in.AgentUptimeSec > 0 {
		in.UptimeSec = in.AgentUptimeSec
	}
	if in.QueueBytes < 0 {
		in.QueueBytes = 0
	}
	if in.QueueBatches < 0 {
		in.QueueBatches = 0
	}
	if in.QuarantinedBatches < 0 {
		in.QuarantinedBatches = 0
	}
	if in.Goroutines < 0 {
		in.Goroutines = 0
	}
	if in.Goroutines > 100000 {
		in.Goroutines = 100000
	}
	in.AgentVersion = trimLimit(in.AgentVersion, 128)
	in.XrayVersion = trimLimit(in.XrayVersion, 128)
	in.XrayConfigVersion = trimLimit(in.XrayConfigVersion, 128)

	if reportedAt.IsZero() {
		reportedAt = time.Now().UTC()
	}
	now := reportedAt.UTC().Format(time.RFC3339)
	bootTime := sql.NullString{}
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(in.BootTime)); err == nil && !t.IsZero() {
		bootTime = sql.NullString{String: t.UTC().Format(time.RFC3339), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO node_runtime_metrics(
	node_id,
	cpu_percent,
	load1,
	load5,
	load15,
	memory_bytes,
	memory_total_bytes,
	memory_available_bytes,
	swap_used_bytes,
	swap_total_bytes,
	inbound_bps,
	outbound_bps,
	disk_total_bytes,
	disk_used_bytes,
	disk_free_bytes,
	disk_used_percent,
	disk_read_bps,
	disk_write_bps,
	tcp_connections,
	udp_connections,
	process_count,
	uptime_sec,
	system_uptime_sec,
	agent_uptime_sec,
	boot_time,
	queue_bytes,
	queue_batches,
	quarantined_batches,
	goroutines,
	agent_version,
	xray_version,
	xray_config_version,
	updated_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(node_id) DO UPDATE SET
	cpu_percent = excluded.cpu_percent,
	load1 = excluded.load1,
	load5 = excluded.load5,
	load15 = excluded.load15,
	memory_bytes = excluded.memory_bytes,
	memory_total_bytes = excluded.memory_total_bytes,
	memory_available_bytes = excluded.memory_available_bytes,
	swap_used_bytes = excluded.swap_used_bytes,
	swap_total_bytes = excluded.swap_total_bytes,
	inbound_bps = excluded.inbound_bps,
	outbound_bps = excluded.outbound_bps,
	disk_total_bytes = excluded.disk_total_bytes,
	disk_used_bytes = excluded.disk_used_bytes,
	disk_free_bytes = excluded.disk_free_bytes,
	disk_used_percent = excluded.disk_used_percent,
	disk_read_bps = excluded.disk_read_bps,
	disk_write_bps = excluded.disk_write_bps,
	tcp_connections = excluded.tcp_connections,
	udp_connections = excluded.udp_connections,
	process_count = excluded.process_count,
	uptime_sec = excluded.uptime_sec,
	system_uptime_sec = excluded.system_uptime_sec,
	agent_uptime_sec = excluded.agent_uptime_sec,
	boot_time = excluded.boot_time,
	queue_bytes = excluded.queue_bytes,
	queue_batches = excluded.queue_batches,
	quarantined_batches = excluded.quarantined_batches,
	goroutines = excluded.goroutines,
	agent_version = excluded.agent_version,
	xray_version = excluded.xray_version,
	xray_config_version = excluded.xray_config_version,
	updated_at = excluded.updated_at;
`, nodeID,
		in.CPUPercent,
		in.Load1,
		in.Load5,
		in.Load15,
		in.MemoryBytes,
		in.MemoryTotalBytes,
		in.MemoryAvailableBytes,
		in.SwapUsedBytes,
		in.SwapTotalBytes,
		in.InboundBPS,
		in.OutboundBPS,
		in.DiskTotalBytes,
		in.DiskUsedBytes,
		in.DiskFreeBytes,
		in.DiskUsedPercent,
		in.DiskReadBPS,
		in.DiskWriteBPS,
		in.TCPConnections,
		in.UDPConnections,
		in.ProcessCount,
		in.UptimeSec,
		in.SystemUptimeSec,
		in.AgentUptimeSec,
		bootTime,
		in.QueueBytes,
		in.QueueBatches,
		in.QuarantinedBatches,
		in.Goroutines,
		in.AgentVersion,
		in.XrayVersion,
		in.XrayConfigVersion,
		now,
	)
	return err
}

func (s *Store) ListNodeRuntimeMetrics(ctx context.Context) (map[int64]NodeRuntimeMetrics, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT node_id,
	cpu_percent,
	load1,
	load5,
	load15,
	memory_bytes,
	memory_total_bytes,
	memory_available_bytes,
	swap_used_bytes,
	swap_total_bytes,
	inbound_bps,
	outbound_bps,
	disk_total_bytes,
	disk_used_bytes,
	disk_free_bytes,
	disk_used_percent,
	disk_read_bps,
	disk_write_bps,
	tcp_connections,
	udp_connections,
	process_count,
	uptime_sec,
	system_uptime_sec,
	agent_uptime_sec,
	boot_time,
	queue_bytes,
	queue_batches,
	quarantined_batches,
	goroutines,
	agent_version,
	xray_version,
	xray_config_version,
	updated_at
FROM node_runtime_metrics;
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]NodeRuntimeMetrics)
	for rows.Next() {
		m, err := scanNodeRuntimeMetrics(rows)
		if err != nil {
			return nil, err
		}
		out[m.NodeID] = m
	}
	return out, rows.Err()
}

func (s *Store) GetNodeRuntimeMetrics(ctx context.Context, nodeID int64) (NodeRuntimeMetrics, bool, error) {
	if nodeID <= 0 {
		return NodeRuntimeMetrics{}, false, fmt.Errorf("invalid node id")
	}
	row := s.db.QueryRowContext(ctx, `
SELECT node_id,
	cpu_percent,
	load1,
	load5,
	load15,
	memory_bytes,
	memory_total_bytes,
	memory_available_bytes,
	swap_used_bytes,
	swap_total_bytes,
	inbound_bps,
	outbound_bps,
	disk_total_bytes,
	disk_used_bytes,
	disk_free_bytes,
	disk_used_percent,
	disk_read_bps,
	disk_write_bps,
	tcp_connections,
	udp_connections,
	process_count,
	uptime_sec,
	system_uptime_sec,
	agent_uptime_sec,
	boot_time,
	queue_bytes,
	queue_batches,
	quarantined_batches,
	goroutines,
	agent_version,
	xray_version,
	xray_config_version,
	updated_at
FROM node_runtime_metrics
WHERE node_id = ?;
`, nodeID)
	item, err := scanNodeRuntimeMetrics(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NodeRuntimeMetrics{}, false, nil
		}
		return NodeRuntimeMetrics{}, false, err
	}
	return item, true, nil
}

type nodeRuntimeMetricsScanner interface {
	Scan(dest ...any) error
}

func scanNodeRuntimeMetrics(scanner nodeRuntimeMetricsScanner) (NodeRuntimeMetrics, error) {
	var m NodeRuntimeMetrics
	var cpuPercent sql.NullFloat64
	var load1 sql.NullFloat64
	var load5 sql.NullFloat64
	var load15 sql.NullFloat64
	var memoryBytes sql.NullInt64
	var memoryTotalBytes sql.NullInt64
	var memoryAvailableBytes sql.NullInt64
	var swapUsedBytes sql.NullInt64
	var swapTotalBytes sql.NullInt64
	var inBPS sql.NullFloat64
	var outBPS sql.NullFloat64
	var diskTotal sql.NullInt64
	var diskUsed sql.NullInt64
	var diskFree sql.NullInt64
	var diskUsedPerc sql.NullFloat64
	var diskReadBPS sql.NullFloat64
	var diskWriteBPS sql.NullFloat64
	var tcpConnections sql.NullInt64
	var udpConnections sql.NullInt64
	var processCount sql.NullInt64
	var uptime sql.NullInt64
	var systemUptime sql.NullInt64
	var agentUptime sql.NullInt64
	var bootTime sql.NullString
	var queueBytes sql.NullInt64
	var queueBatches sql.NullInt64
	var quarantinedBatches sql.NullInt64
	var goroutines sql.NullInt64
	var agentVersion sql.NullString
	var xrayVersion sql.NullString
	var xrayConfigVersion sql.NullString
	var updatedAt string
	if err := scanner.Scan(
		&m.NodeID,
		&cpuPercent,
		&load1,
		&load5,
		&load15,
		&memoryBytes,
		&memoryTotalBytes,
		&memoryAvailableBytes,
		&swapUsedBytes,
		&swapTotalBytes,
		&inBPS,
		&outBPS,
		&diskTotal,
		&diskUsed,
		&diskFree,
		&diskUsedPerc,
		&diskReadBPS,
		&diskWriteBPS,
		&tcpConnections,
		&udpConnections,
		&processCount,
		&uptime,
		&systemUptime,
		&agentUptime,
		&bootTime,
		&queueBytes,
		&queueBatches,
		&quarantinedBatches,
		&goroutines,
		&agentVersion,
		&xrayVersion,
		&xrayConfigVersion,
		&updatedAt,
	); err != nil {
		return NodeRuntimeMetrics{}, err
	}
	if cpuPercent.Valid {
		m.CPUPercent = cpuPercent.Float64
	}
	if load1.Valid {
		m.Load1 = load1.Float64
	}
	if load5.Valid {
		m.Load5 = load5.Float64
	}
	if load15.Valid {
		m.Load15 = load15.Float64
	}
	if memoryBytes.Valid {
		m.MemoryBytes = memoryBytes.Int64
	}
	if memoryTotalBytes.Valid {
		m.MemoryTotalBytes = memoryTotalBytes.Int64
	}
	if memoryAvailableBytes.Valid {
		m.MemoryAvailableBytes = memoryAvailableBytes.Int64
	}
	if swapUsedBytes.Valid {
		m.SwapUsedBytes = swapUsedBytes.Int64
	}
	if swapTotalBytes.Valid {
		m.SwapTotalBytes = swapTotalBytes.Int64
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
	if diskReadBPS.Valid {
		m.DiskReadBPS = diskReadBPS.Float64
	}
	if diskWriteBPS.Valid {
		m.DiskWriteBPS = diskWriteBPS.Float64
	}
	if tcpConnections.Valid {
		m.TCPConnections = int(tcpConnections.Int64)
	}
	if udpConnections.Valid {
		m.UDPConnections = int(udpConnections.Int64)
	}
	if processCount.Valid {
		m.ProcessCount = int(processCount.Int64)
	}
	if uptime.Valid {
		m.UptimeSec = uptime.Int64
	}
	if systemUptime.Valid {
		m.SystemUptimeSec = systemUptime.Int64
	}
	if agentUptime.Valid {
		m.AgentUptimeSec = agentUptime.Int64
	}
	if bootTime.Valid && bootTime.String != "" {
		if t, err := time.Parse(time.RFC3339, bootTime.String); err == nil {
			m.BootTime = &t
		}
	}
	if queueBytes.Valid {
		m.QueueBytes = queueBytes.Int64
	}
	if queueBatches.Valid {
		m.QueueBatches = queueBatches.Int64
	}
	if quarantinedBatches.Valid {
		m.QuarantinedBatches = quarantinedBatches.Int64
	}
	if goroutines.Valid {
		m.Goroutines = int(goroutines.Int64)
	}
	if agentVersion.Valid {
		m.AgentVersion = agentVersion.String
	}
	if xrayVersion.Valid {
		m.XrayVersion = xrayVersion.String
	}
	if xrayConfigVersion.Valid {
		m.XrayConfigVersion = xrayConfigVersion.String
	}
	m.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return m, nil
}

func clampFloatMin(v, min float64) float64 {
	if v < min {
		return min
	}
	return v
}

func trimLimit(v string, max int) string {
	v = strings.TrimSpace(v)
	if max > 0 && len(v) > max {
		return v[:max]
	}
	return v
}
