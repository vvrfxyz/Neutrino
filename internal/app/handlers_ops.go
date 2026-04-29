package app

import (
	"net/http"
	"strings"
	"time"
)

func (a *App) handleAPIOpsNodesV1(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	nodes, err := a.store.ListNodes(r.Context())
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	metricsByNode, _ := a.store.ListNodeRuntimeMetrics(r.Context())
	monthlyUsageByNode, _ := a.store.ListLatestNodeMonthlyUsage(r.Context())

	now := time.Now().UTC()
	staleAfter := 120 * time.Second

	items := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		health := "unknown"
		if !n.Enabled {
			health = "disabled"
		} else if n.LastSeenAt != nil && now.Sub(*n.LastSeenAt) <= staleAfter {
			health = "online"
		} else if n.LastSeenAt != nil {
			health = "stale"
		}

		pending, runningKind, runningDesired, runningStarted, runningCorr, _ := a.store.GetNodeJobSummary(r.Context(), n.ID)

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
		if m, ok := metricsByNode[n.ID]; ok {
			item["agent_metrics"] = map[string]any{
				"cpu_percent":       m.CPUPercent,
				"memory_bytes":      m.MemoryBytes,
				"inbound_bps":       m.InboundBPS,
				"outbound_bps":      m.OutboundBPS,
				"disk_total_bytes":  m.DiskTotalBytes,
				"disk_used_bytes":   m.DiskUsedBytes,
				"disk_free_bytes":   m.DiskFreeBytes,
				"disk_used_percent": m.DiskUsedPercent,
				"uptime_sec":        m.UptimeSec,
				"queue_bytes":       m.QueueBytes,
				"queue_batches":     m.QueueBatches,
				"goroutines":        m.Goroutines,
			}
			item["agent_metrics_at"] = fmtMaybeTime(&m.UpdatedAt)
		}
		if mu, ok := monthlyUsageByNode[n.ID]; ok {
			item["month_usage"] = map[string]any{
				"month_key":        mu.MonthKey,
				"timezone_name":    mu.TimezoneName,
				"rx_bytes":         mu.RXBytes,
				"tx_bytes":         mu.TXBytes,
				"total_bytes":      mu.RXBytes + mu.TXBytes,
				"counter_source":   mu.CounterSource,
				"last_reported_at": fmtMaybeTime(mu.LastReportedAt),
			}
		}
		items = append(items, item)
	}

	a.writeJSON(w, http.StatusOK, map[string]any{"count": len(items), "items": items})
}

func fmtMaybeTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
