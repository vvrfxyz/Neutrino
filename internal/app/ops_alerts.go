package app

import (
	"context"
	"fmt"
	"time"
)

const (
	opsAlertDiskWarnPercent   = 90
	opsAlertDiskClearPercent  = 85
	opsAlertMemoryWarnPercent = 90
	opsAlertMemoryClearPct    = 85
	opsAlertQueueBatches      = 100
	opsAlertQueueBytes        = 64 * 1024 * 1024
	opsAlertJobStuckAfter     = 10 * time.Minute
)

func (a *App) syncOpsAlerts(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	items, err := a.buildOpsNodesItems(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if err := a.syncNodeOpsAlerts(ctx, item, now.UTC()); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) syncNodeOpsAlerts(ctx context.Context, item map[string]any, now time.Time) error {
	nodeID := int64FromAny(item["id"])
	if nodeID <= 0 {
		return nil
	}
	name := stringFromAny(item["name"])
	if name == "" {
		name = fmt.Sprintf("node %d", nodeID)
	}
	enabled := boolFromAny(item["enabled"])
	health := stringFromAny(item["health"])

	if !enabled {
		return a.resolveGeneratedNodeOpsAlerts(ctx, nodeID, now)
	}

	if enabled && health == "stale" {
		if err := a.upsertNodeOpsAlert(ctx, nodeID, "node_stale", "warning", fmt.Sprintf("%s has not reported recently", name), now); err != nil {
			return err
		}
	} else if health == "online" {
		if err := a.resolveNodeOpsAlert(ctx, nodeID, "node_stale", now); err != nil {
			return err
		}
	}

	metrics, hasMetrics := mapFromAny(item["agent_metrics"])
	if !hasMetrics {
		if err := a.upsertNodeOpsAlert(ctx, nodeID, "agent_metrics_missing", "warning", fmt.Sprintf("%s has no runtime metrics", name), now); err != nil {
			return err
		}
	} else if hasMetrics {
		if err := a.resolveNodeOpsAlert(ctx, nodeID, "agent_metrics_missing", now); err != nil {
			return err
		}
	}
	if !hasMetrics {
		return nil
	}

	diskUsed := floatFromAny(metrics["disk_used_percent"])
	switch {
	case diskUsed >= 95:
		if err := a.upsertNodeOpsAlert(ctx, nodeID, "disk_high", "critical", fmt.Sprintf("%s disk usage is %.1f%%", name, diskUsed), now); err != nil {
			return err
		}
	case diskUsed >= opsAlertDiskWarnPercent:
		if err := a.upsertNodeOpsAlert(ctx, nodeID, "disk_high", "warning", fmt.Sprintf("%s disk usage is %.1f%%", name, diskUsed), now); err != nil {
			return err
		}
	case diskUsed < opsAlertDiskClearPercent:
		if err := a.resolveNodeOpsAlert(ctx, nodeID, "disk_high", now); err != nil {
			return err
		}
	}

	memoryUsed := floatFromAny(metrics["memory_bytes"])
	memoryTotal := floatFromAny(metrics["memory_total_bytes"])
	if memoryTotal > 0 {
		memoryPct := memoryUsed / memoryTotal * 100
		switch {
		case memoryPct >= 95:
			if err := a.upsertNodeOpsAlert(ctx, nodeID, "memory_high", "critical", fmt.Sprintf("%s memory usage is %.1f%%", name, memoryPct), now); err != nil {
				return err
			}
		case memoryPct >= opsAlertMemoryWarnPercent:
			if err := a.upsertNodeOpsAlert(ctx, nodeID, "memory_high", "warning", fmt.Sprintf("%s memory usage is %.1f%%", name, memoryPct), now); err != nil {
				return err
			}
		case memoryPct < opsAlertMemoryClearPct:
			if err := a.resolveNodeOpsAlert(ctx, nodeID, "memory_high", now); err != nil {
				return err
			}
		}
	}

	queueBatches := int64FromAny(metrics["queue_batches"])
	queueBytes := int64FromAny(metrics["queue_bytes"])
	if queueBatches >= opsAlertQueueBatches || queueBytes >= opsAlertQueueBytes {
		if err := a.upsertNodeOpsAlert(ctx, nodeID, "queue_backlog", "warning", fmt.Sprintf("%s has queued usage batches", name), now); err != nil {
			return err
		}
	} else if queueBatches == 0 && queueBytes == 0 {
		if err := a.resolveNodeOpsAlert(ctx, nodeID, "queue_backlog", now); err != nil {
			return err
		}
	}

	if hasVersionDrift(item, "desired_users_version", "applied_users_version") || hasVersionDrift(item, "desired_xray_version", "applied_xray_version") {
		if err := a.upsertNodeOpsAlert(ctx, nodeID, "version_drift", "warning", fmt.Sprintf("%s applied version differs from desired", name), now); err != nil {
			return err
		}
	} else if err := a.resolveNodeOpsAlert(ctx, nodeID, "version_drift", now); err != nil {
		return err
	}

	if startedAt, ok := parseTimeString(item["running_started_at"]); ok && now.Sub(startedAt) >= opsAlertJobStuckAfter {
		if err := a.upsertNodeOpsAlert(ctx, nodeID, "job_stuck", "warning", fmt.Sprintf("%s has a long-running node job", name), now); err != nil {
			return err
		}
	} else if err := a.resolveNodeOpsAlert(ctx, nodeID, "job_stuck", now); err != nil {
		return err
	}

	return nil
}

func (a *App) resolveGeneratedNodeOpsAlerts(ctx context.Context, nodeID int64, at time.Time) error {
	for _, kind := range []string{
		"node_stale",
		"agent_metrics_missing",
		"disk_high",
		"memory_high",
		"queue_backlog",
		"version_drift",
		"job_stuck",
		"xray_apply_failed",
	} {
		if err := a.resolveNodeOpsAlert(ctx, nodeID, kind, at); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) upsertNodeOpsAlert(ctx context.Context, nodeID int64, kind, severity, message string, at time.Time) error {
	_, err := a.store.UpsertOpsAlert(ctx, &nodeID, kind, severity, message, nodeOpsAlertKey(nodeID, kind), at)
	return err
}

func (a *App) resolveNodeOpsAlert(ctx context.Context, nodeID int64, kind string, at time.Time) error {
	_, err := a.store.ResolveOpsAlert(ctx, nodeOpsAlertKey(nodeID, kind), at)
	return err
}

func (a *App) syncProbeOpsAlert(ctx context.Context, nodeID int64, probeKind, target string, success bool, errMsg string, at time.Time) error {
	key := fmt.Sprintf("node:%d:probe:%s:%s", nodeID, probeKind, target)
	if success {
		_, err := a.store.ResolveOpsAlert(ctx, key, at)
		return err
	}
	message := fmt.Sprintf("probe %s failed", probeKind)
	if target != "" {
		message = fmt.Sprintf("probe %s failed for %s", probeKind, target)
	}
	if errMsg != "" {
		message += ": " + errMsg
	}
	_, err := a.store.UpsertOpsAlert(ctx, &nodeID, "probe_failed", "warning", message, key, at)
	return err
}

func (a *App) syncNodeJobOpsAlert(ctx context.Context, nodeID int64, jobKind, finalStatus, errMsg string, at time.Time) error {
	if jobKind != "xray_apply" {
		return nil
	}
	if finalStatus == "succeeded" {
		return a.resolveNodeOpsAlert(ctx, nodeID, "xray_apply_failed", at)
	}
	if finalStatus != "failed" {
		return nil
	}
	message := "xray apply failed"
	if errMsg != "" {
		message += ": " + errMsg
	}
	return a.upsertNodeOpsAlert(ctx, nodeID, "xray_apply_failed", "critical", message, at)
}

func nodeOpsAlertKey(nodeID int64, kind string) string {
	return fmt.Sprintf("node:%d:%s", nodeID, kind)
}

func hasVersionDrift(item map[string]any, desiredKey, appliedKey string) bool {
	desired := stringFromAny(item[desiredKey])
	applied := stringFromAny(item[appliedKey])
	return desired != "" && applied != "" && desired != applied
}

func mapFromAny(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok && m != nil
}

func stringFromAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func boolFromAny(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

func int64FromAny(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	case float32:
		return int64(n)
	default:
		return 0
	}
}

func floatFromAny(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	default:
		return 0
	}
}

func parseTimeString(v any) (time.Time, bool) {
	s := stringFromAny(v)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}
