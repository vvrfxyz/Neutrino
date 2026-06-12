package service

import (
	"context"
	"fmt"
	"log"
	"time"

	"neutrino/internal/repo"
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

// AlertSender abstracts the notification transport used to deliver pending
// alerts. Implemented by internal/notify.Notifier.
type AlertSender interface {
	SendMail(subject, body string, to []string) error
}

// AlertStore is the narrow repo surface AlertService depends on. *repo.Store
// satisfies it.
type AlertStore interface {
	ListOpsAlerts(ctx context.Context, status string, limit int) ([]repo.OpsAlert, error)
	UpsertOpsAlert(ctx context.Context, nodeID *int64, kind, severity, message, dedupeKey string, at time.Time) (int64, error)
	ResolveOpsAlert(ctx context.Context, dedupeKey string, at time.Time) (bool, error)
	ListPendingAlerts(ctx context.Context, limit int) ([]repo.Alert, error)
	MarkAlertSent(ctx context.Context, alertID int64) error
}

var _ AlertStore = (*repo.Store)(nil)

// AlertService owns ops-alert lifecycle: deriving node health alerts from the
// ops snapshot, probe/job alert sync, manual upsert/resolve, and dispatching
// pending alerts to the configured channels.
type AlertService struct {
	store AlertStore
}

func NewAlertService(store AlertStore) *AlertService {
	return &AlertService{store: store}
}

func (s *AlertService) List(ctx context.Context, status string, limit int) ([]repo.OpsAlert, error) {
	return s.store.ListOpsAlerts(ctx, status, limit)
}

func (s *AlertService) Upsert(ctx context.Context, nodeID *int64, kind, severity, message, dedupeKey string, at time.Time) (int64, error) {
	return s.store.UpsertOpsAlert(ctx, nodeID, kind, severity, message, dedupeKey, at)
}

func (s *AlertService) Resolve(ctx context.Context, dedupeKey string, at time.Time) (bool, error) {
	return s.store.ResolveOpsAlert(ctx, dedupeKey, at)
}

// SyncFromNodeItems derives generated per-node alerts from the ops snapshot
// rows produced by OpsService.BuildNodeItems.
func (s *AlertService) SyncFromNodeItems(ctx context.Context, items []map[string]any, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	for _, item := range items {
		if err := s.SyncNodeAlerts(ctx, item, now.UTC()); err != nil {
			return err
		}
	}
	return nil
}

func (s *AlertService) SyncNodeAlerts(ctx context.Context, item map[string]any, now time.Time) error {
	nodeID := anyInt64(item["id"])
	if nodeID <= 0 {
		return nil
	}
	name := anyString(item["name"])
	if name == "" {
		name = fmt.Sprintf("node %d", nodeID)
	}
	enabled := anyBool(item["enabled"])
	health := anyString(item["health"])

	if !enabled {
		return s.resolveGeneratedNodeAlerts(ctx, nodeID, now)
	}

	if enabled && health == "stale" {
		if err := s.upsertNodeAlert(ctx, nodeID, "node_stale", "warning", fmt.Sprintf("%s has not reported recently", name), now); err != nil {
			return err
		}
	} else if health == "online" {
		if err := s.resolveNodeAlert(ctx, nodeID, "node_stale", now); err != nil {
			return err
		}
	}

	metrics, hasMetrics := anyMap(item["agent_metrics"])
	if !hasMetrics {
		if err := s.upsertNodeAlert(ctx, nodeID, "agent_metrics_missing", "warning", fmt.Sprintf("%s has no runtime metrics", name), now); err != nil {
			return err
		}
	} else if hasMetrics {
		if err := s.resolveNodeAlert(ctx, nodeID, "agent_metrics_missing", now); err != nil {
			return err
		}
	}
	if !hasMetrics {
		return nil
	}

	diskUsed := anyFloat64(metrics["disk_used_percent"])
	switch {
	case diskUsed >= 95:
		if err := s.upsertNodeAlert(ctx, nodeID, "disk_high", "critical", fmt.Sprintf("%s disk usage is %.1f%%", name, diskUsed), now); err != nil {
			return err
		}
	case diskUsed >= opsAlertDiskWarnPercent:
		if err := s.upsertNodeAlert(ctx, nodeID, "disk_high", "warning", fmt.Sprintf("%s disk usage is %.1f%%", name, diskUsed), now); err != nil {
			return err
		}
	case diskUsed < opsAlertDiskClearPercent:
		if err := s.resolveNodeAlert(ctx, nodeID, "disk_high", now); err != nil {
			return err
		}
	}

	memoryUsed := anyFloat64(metrics["memory_bytes"])
	memoryTotal := anyFloat64(metrics["memory_total_bytes"])
	if memoryTotal > 0 {
		memoryPct := memoryUsed / memoryTotal * 100
		switch {
		case memoryPct >= 95:
			if err := s.upsertNodeAlert(ctx, nodeID, "memory_high", "critical", fmt.Sprintf("%s memory usage is %.1f%%", name, memoryPct), now); err != nil {
				return err
			}
		case memoryPct >= opsAlertMemoryWarnPercent:
			if err := s.upsertNodeAlert(ctx, nodeID, "memory_high", "warning", fmt.Sprintf("%s memory usage is %.1f%%", name, memoryPct), now); err != nil {
				return err
			}
		case memoryPct < opsAlertMemoryClearPct:
			if err := s.resolveNodeAlert(ctx, nodeID, "memory_high", now); err != nil {
				return err
			}
		}
	}

	queueBatches := anyInt64(metrics["queue_batches"])
	queueBytes := anyInt64(metrics["queue_bytes"])
	if queueBatches >= opsAlertQueueBatches || queueBytes >= opsAlertQueueBytes {
		if err := s.upsertNodeAlert(ctx, nodeID, "queue_backlog", "warning", fmt.Sprintf("%s has queued usage batches", name), now); err != nil {
			return err
		}
	} else if queueBatches == 0 && queueBytes == 0 {
		if err := s.resolveNodeAlert(ctx, nodeID, "queue_backlog", now); err != nil {
			return err
		}
	}

	if quarantined := anyInt64(metrics["quarantined_batches"]); quarantined > 0 {
		if err := s.upsertNodeAlert(ctx, nodeID, "usage_quarantined", "critical", fmt.Sprintf("%s has %d quarantined usage batches (usage data is being dropped)", name, quarantined), now); err != nil {
			return err
		}
	} else if err := s.resolveNodeAlert(ctx, nodeID, "usage_quarantined", now); err != nil {
		return err
	}

	if hasVersionDrift(item, "desired_users_version", "applied_users_version") || hasVersionDrift(item, "desired_xray_version", "applied_xray_version") {
		if err := s.upsertNodeAlert(ctx, nodeID, "version_drift", "warning", fmt.Sprintf("%s applied version differs from desired", name), now); err != nil {
			return err
		}
	} else if err := s.resolveNodeAlert(ctx, nodeID, "version_drift", now); err != nil {
		return err
	}

	if startedAt, ok := anyTimeRFC3339(item["running_started_at"]); ok && now.Sub(startedAt) >= opsAlertJobStuckAfter {
		if err := s.upsertNodeAlert(ctx, nodeID, "job_stuck", "warning", fmt.Sprintf("%s has a long-running node job", name), now); err != nil {
			return err
		}
	} else if err := s.resolveNodeAlert(ctx, nodeID, "job_stuck", now); err != nil {
		return err
	}

	return nil
}

func (s *AlertService) resolveGeneratedNodeAlerts(ctx context.Context, nodeID int64, at time.Time) error {
	for _, kind := range []string{
		"node_stale",
		"agent_metrics_missing",
		"disk_high",
		"memory_high",
		"queue_backlog",
		"usage_quarantined",
		"version_drift",
		"job_stuck",
		"xray_apply_failed",
	} {
		if err := s.resolveNodeAlert(ctx, nodeID, kind, at); err != nil {
			return err
		}
	}
	return nil
}

func (s *AlertService) upsertNodeAlert(ctx context.Context, nodeID int64, kind, severity, message string, at time.Time) error {
	_, err := s.store.UpsertOpsAlert(ctx, &nodeID, kind, severity, message, nodeOpsAlertKey(nodeID, kind), at)
	return err
}

func (s *AlertService) resolveNodeAlert(ctx context.Context, nodeID int64, kind string, at time.Time) error {
	_, err := s.store.ResolveOpsAlert(ctx, nodeOpsAlertKey(nodeID, kind), at)
	return err
}

func (s *AlertService) SyncProbeAlert(ctx context.Context, nodeID int64, probeKind, target string, success bool, errMsg string, at time.Time) error {
	key := fmt.Sprintf("node:%d:probe:%s:%s", nodeID, probeKind, target)
	if success {
		_, err := s.store.ResolveOpsAlert(ctx, key, at)
		return err
	}
	message := fmt.Sprintf("probe %s failed", probeKind)
	if target != "" {
		message = fmt.Sprintf("probe %s failed for %s", probeKind, target)
	}
	if errMsg != "" {
		message += ": " + errMsg
	}
	_, err := s.store.UpsertOpsAlert(ctx, &nodeID, "probe_failed", "warning", message, key, at)
	return err
}

func (s *AlertService) SyncNodeJobAlert(ctx context.Context, nodeID int64, jobKind, finalStatus, errMsg string, at time.Time) error {
	if jobKind != "xray_apply" {
		return nil
	}
	if finalStatus == "succeeded" {
		return s.resolveNodeAlert(ctx, nodeID, "xray_apply_failed", at)
	}
	if finalStatus != "failed" {
		return nil
	}
	message := "xray apply failed"
	if errMsg != "" {
		message += ": " + errMsg
	}
	return s.upsertNodeAlert(ctx, nodeID, "xray_apply_failed", "critical", message, at)
}

// DispatchPending delivers queued alerts via email, marking each as sent on
// success. Returns the first error encountered, but keeps going so one bad
// alert does not wedge the queue.
func (s *AlertService) DispatchPending(ctx context.Context, sender AlertSender, emailTo []string) error {
	alerts, err := s.store.ListPendingAlerts(ctx, 200)
	if err != nil {
		return err
	}
	var firstErr error
	for _, al := range alerts {
		message := fmt.Sprintf("[%s] %s", upperASCII(al.Type), al.Message)
		if sendErr := sender.SendMail("Neutrino Alert", message, emailTo); sendErr != nil {
			log.Printf("alert send failed id=%d channel=%s err=%v", al.ID, al.Channel, sendErr)
			if firstErr == nil {
				firstErr = sendErr
			}
			continue
		}
		if err := s.store.MarkAlertSent(ctx, al.ID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			log.Printf("mark alert sent failed id=%d err=%v", al.ID, err)
		}
	}
	return firstErr
}

func nodeOpsAlertKey(nodeID int64, kind string) string {
	return fmt.Sprintf("node:%d:%s", nodeID, kind)
}

func hasVersionDrift(item map[string]any, desiredKey, appliedKey string) bool {
	desired := anyString(item[desiredKey])
	applied := anyString(item[appliedKey])
	return desired != "" && applied != "" && desired != applied
}

func upperASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

func anyMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok && m != nil
}

func anyString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func anyBool(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

func anyInt64(v any) int64 {
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

func anyFloat64(v any) float64 {
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

func anyTimeRFC3339(v any) (time.Time, bool) {
	s := anyString(v)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}
