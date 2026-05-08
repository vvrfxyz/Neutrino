package service

import (
	"context"
	"strings"
	"time"

	"neutrino/internal/repo"
)

const opsNodeStaleAfter = 120 * time.Second

type OpsService struct {
	store *repo.Store
}

func NewOpsService(store *repo.Store) *OpsService {
	return &OpsService{store: store}
}

func (s *OpsService) ListEnforcementLogs(ctx context.Context, limit int) ([]repo.EnforcementLog, error) {
	return s.store.ListEnforcementLogs(ctx, limit)
}

func (s *OpsService) ListOnlineUsers(ctx context.Context, windowSec int) ([]repo.OnlineUser, error) {
	return s.store.ListOnlineUsers(ctx, windowSec)
}

func (s *OpsService) GetHostNetMonthlyUsage(ctx context.Context, at time.Time) (repo.HostNetMonthlyUsage, bool, error) {
	return s.store.GetHostNetMonthlyUsage(ctx, at)
}

func (s *OpsService) BuildNodeItems(ctx context.Context) ([]map[string]any, error) {
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	metricsByNode, _ := s.store.ListNodeRuntimeMetrics(ctx)
	monthlyUsageByNode, _ := s.store.ListLatestNodeMonthlyUsage(ctx)
	jobSummariesByNode, _ := s.store.ListNodeJobSummaries(ctx)
	metadataByNode := make(map[int64]repo.NodeMetadata, len(nodes))
	for _, n := range nodes {
		if meta, ok, err := s.store.GetNodeMetadata(ctx, n.ID); err == nil && ok {
			metadataByNode[n.ID] = meta
		}
	}
	now := time.Now().UTC()
	items := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		items = append(items, s.buildNodeItem(n, metricsByNode[n.ID], monthlyUsageByNode[n.ID], jobSummariesByNode[n.ID], metadataByNode[n.ID], now))
	}
	return items, nil
}

func (s *OpsService) buildNodeItem(n repo.Node, metrics repo.NodeRuntimeMetrics, monthly repo.NodeMonthlyUsage, jobSummary repo.NodeJobSummary, metadata repo.NodeMetadata, now time.Time) map[string]any {
	health := "unknown"
	switch {
	case !n.Enabled:
		health = "disabled"
	case n.LastSeenAt != nil && now.Sub(*n.LastSeenAt) <= opsNodeStaleAfter:
		health = "online"
	case n.LastSeenAt != nil:
		health = "stale"
	}

	item := map[string]any{
		"id":                    n.ID,
		"name":                  n.Name,
		"host":                  n.Host,
		"observed_ip":           n.ObservedIP,
		"enabled":               n.Enabled,
		"managed":               n.Managed,
		"health":                health,
		"last_seen_at":          fmtMaybeTime(n.LastSeenAt),
		"last_error":            strings.TrimSpace(n.LastError),
		"pending_jobs":          jobSummary.Pending,
		"running_kind":          jobSummary.RunningKind,
		"running_desired":       jobSummary.RunningDesired,
		"running_started_at":    jobSummary.RunningStartedAt,
		"running_correlation":   jobSummary.RunningCorrelation,
		"desired_users_version": n.DesiredUsersVersion,
		"applied_users_version": n.AppliedUsersVersion,
		"desired_xray_version":  n.DesiredXrayVersion,
		"applied_xray_version":  n.AppliedXrayVersion,
	}
	if metrics.UpdatedAt != (time.Time{}) {
		item["agent_metrics"] = map[string]any{
			"cpu_percent":            metrics.CPUPercent,
			"load1":                  metrics.Load1,
			"load5":                  metrics.Load5,
			"load15":                 metrics.Load15,
			"memory_bytes":           metrics.MemoryBytes,
			"memory_total_bytes":     metrics.MemoryTotalBytes,
			"memory_available_bytes": metrics.MemoryAvailableBytes,
			"swap_used_bytes":        metrics.SwapUsedBytes,
			"swap_total_bytes":       metrics.SwapTotalBytes,
			"inbound_bps":            metrics.InboundBPS,
			"outbound_bps":           metrics.OutboundBPS,
			"disk_total_bytes":       metrics.DiskTotalBytes,
			"disk_used_bytes":        metrics.DiskUsedBytes,
			"disk_free_bytes":        metrics.DiskFreeBytes,
			"disk_used_percent":      metrics.DiskUsedPercent,
			"disk_read_bps":          metrics.DiskReadBPS,
			"disk_write_bps":         metrics.DiskWriteBPS,
			"tcp_connections":        metrics.TCPConnections,
			"udp_connections":        metrics.UDPConnections,
			"process_count":          metrics.ProcessCount,
			"uptime_sec":             metrics.UptimeSec,
			"system_uptime_sec":      metrics.SystemUptimeSec,
			"agent_uptime_sec":       metrics.AgentUptimeSec,
			"boot_time":              fmtMaybeTime(metrics.BootTime),
			"queue_bytes":            metrics.QueueBytes,
			"queue_batches":          metrics.QueueBatches,
			"goroutines":             metrics.Goroutines,
			"agent_version":          metrics.AgentVersion,
			"xray_version":           metrics.XrayVersion,
			"xray_config_version":    metrics.XrayConfigVersion,
		}
		item["agent_metrics_at"] = fmtMaybeTime(&metrics.UpdatedAt)
	}
	if monthly.MonthKey != "" {
		item["month_usage"] = map[string]any{
			"month_key":        monthly.MonthKey,
			"timezone_name":    monthly.TimezoneName,
			"rx_bytes":         monthly.RXBytes,
			"tx_bytes":         monthly.TXBytes,
			"total_bytes":      monthly.RXBytes + monthly.TXBytes,
			"counter_source":   monthly.CounterSource,
			"last_reported_at": fmtMaybeTime(monthly.LastReportedAt),
		}
	}
	if metadata.NodeID > 0 {
		item["metadata"] = metadata
	}
	return item
}

func fmtMaybeTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
