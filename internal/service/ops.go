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
	now := time.Now().UTC()
	items := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		items = append(items, s.buildNodeItem(ctx, n, metricsByNode[n.ID], monthlyUsageByNode[n.ID], now))
	}
	return items, nil
}

func (s *OpsService) buildNodeItem(ctx context.Context, n repo.Node, metrics repo.NodeRuntimeMetrics, monthly repo.NodeMonthlyUsage, now time.Time) map[string]any {
	health := "unknown"
	switch {
	case !n.Enabled:
		health = "disabled"
	case n.LastSeenAt != nil && now.Sub(*n.LastSeenAt) <= opsNodeStaleAfter:
		health = "online"
	case n.LastSeenAt != nil:
		health = "stale"
	}

	pending, runningKind, runningDesired, runningStarted, runningCorr, _ := s.store.GetNodeJobSummary(ctx, n.ID)

	item := map[string]any{
		"id":                    n.ID,
		"name":                  n.Name,
		"enabled":               n.Enabled,
		"managed":               n.Managed,
		"health":                health,
		"last_seen_at":          fmtMaybeTime(n.LastSeenAt),
		"last_error":            strings.TrimSpace(n.LastError),
		"pending_jobs":          pending,
		"running_kind":          runningKind,
		"running_desired":       runningDesired,
		"running_started_at":    runningStarted,
		"running_correlation":   runningCorr,
		"desired_users_version": n.DesiredUsersVersion,
		"applied_users_version": n.AppliedUsersVersion,
		"desired_xray_version":  n.DesiredXrayVersion,
		"applied_xray_version":  n.AppliedXrayVersion,
	}
	if metrics.UpdatedAt != (time.Time{}) {
		item["agent_metrics"] = map[string]any{
			"cpu_percent":       metrics.CPUPercent,
			"memory_bytes":      metrics.MemoryBytes,
			"inbound_bps":       metrics.InboundBPS,
			"outbound_bps":      metrics.OutboundBPS,
			"disk_total_bytes":  metrics.DiskTotalBytes,
			"disk_used_bytes":   metrics.DiskUsedBytes,
			"disk_free_bytes":   metrics.DiskFreeBytes,
			"disk_used_percent": metrics.DiskUsedPercent,
			"uptime_sec":        metrics.UptimeSec,
			"queue_bytes":       metrics.QueueBytes,
			"queue_batches":     metrics.QueueBatches,
			"goroutines":        metrics.Goroutines,
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
	return item
}

func fmtMaybeTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
