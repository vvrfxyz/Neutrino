package app

import (
	"context"
	"net/http"
	"strings"
	"time"

	"neutrino/internal/repo"
)

const opsNodeStaleAfter = 120 * time.Second

func (a *App) handleAPIOpsNodesV1(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	items, err := a.buildOpsNodesItems(r.Context())
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"count": len(items), "items": items})
}

// buildOpsNodesItems is the single source of truth for the per-node row that
// the /ops dashboard renders. Both the polling endpoint and the WebSocket
// publisher call it so the two paths cannot drift apart in shape.
func (a *App) buildOpsNodesItems(ctx context.Context) ([]map[string]any, error) {
	nodes, err := a.store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	metricsByNode, _ := a.store.ListNodeRuntimeMetrics(ctx)
	monthlyUsageByNode, _ := a.store.ListLatestNodeMonthlyUsage(ctx)
	now := time.Now().UTC()
	items := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		items = append(items, a.buildOpsNodeItem(ctx, n, metricsByNode[n.ID], monthlyUsageByNode[n.ID], now))
	}
	return items, nil
}

func (a *App) buildOpsNodeItem(ctx context.Context, n repo.Node, metrics repo.NodeRuntimeMetrics, monthly repo.NodeMonthlyUsage, now time.Time) map[string]any {
	health := "unknown"
	switch {
	case !n.Enabled:
		health = "disabled"
	case n.LastSeenAt != nil && now.Sub(*n.LastSeenAt) <= opsNodeStaleAfter:
		health = "online"
	case n.LastSeenAt != nil:
		health = "stale"
	}

	pending, runningKind, runningDesired, runningStarted, runningCorr, _ := a.store.GetNodeJobSummary(ctx, n.ID)

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
