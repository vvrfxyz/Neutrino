package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"neutrino/internal/repo"
)

const opsNodeStaleAfter = 120 * time.Second

type OpsService struct {
	store *repo.Store
	cache *OpsLatestCache
}

func NewOpsService(store *repo.Store) *OpsService {
	return &OpsService{store: store, cache: NewOpsLatestCache(5 * time.Second)}
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
	if s.cache != nil {
		if items, ok := s.cache.Get(); ok {
			// Health is time-derived: a cached "online" item must flip to
			// "stale" once the node stops reporting, even while other nodes
			// keep the cache entry alive. Recompute it at read time from the
			// item's last_seen_at instead of trusting the cached value.
			refreshNodeItemsHealth(items, time.Now().UTC())
			return items, nil
		}
	}
	items, err := s.buildNodeItemsFresh(ctx)
	if err != nil {
		return nil, err
	}
	if s.cache != nil {
		s.cache.Set(items)
	}
	return items, nil
}

func (s *OpsService) WarmUp(ctx context.Context) error {
	items, err := s.buildNodeItemsFresh(ctx)
	if err != nil {
		return err
	}
	if s.cache != nil {
		s.cache.Set(items)
	}
	return nil
}

func (s *OpsService) InvalidateNode(nodeID int64) {
	if s.cache != nil {
		s.cache.Invalidate()
	}
}

func (s *OpsService) RefreshNode(ctx context.Context, nodeID int64) error {
	if s.cache == nil {
		return nil
	}
	item, ok, err := s.buildSingleNodeItem(ctx, nodeID)
	if err != nil {
		if errors.Is(err, repo.ErrNodeNotFound) {
			s.cache.RemoveNode(nodeID)
			return nil
		}
		return err
	}
	if !ok {
		s.cache.RemoveNode(nodeID)
		return nil
	}
	s.cache.UpsertNode(item)
	return nil
}

func (s *OpsService) RemoveNode(nodeID int64) {
	if s.cache != nil {
		s.cache.RemoveNode(nodeID)
	}
}

func (s *OpsService) InvalidateAll() {
	if s.cache != nil {
		s.cache.Invalidate()
	}
}

func (s *OpsService) buildNodeItemsFresh(ctx context.Context) ([]map[string]any, error) {
	nodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	metricsByNode, _ := s.store.ListNodeRuntimeMetrics(ctx)
	monthlyUsageByNode, _ := s.store.ListLatestNodeMonthlyUsage(ctx)
	jobSummariesByNode, _ := s.store.ListNodeJobSummaries(ctx)
	metadataByNode, err := s.store.ListNodeMetadata(ctx)
	if err != nil {
		metadataByNode = map[int64]repo.NodeMetadata{}
	}
	now := time.Now().UTC()
	items := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		items = append(items, s.buildNodeItem(n, metricsByNode[n.ID], monthlyUsageByNode[n.ID], jobSummariesByNode[n.ID], metadataByNode[n.ID], now))
	}
	return items, nil
}

func (s *OpsService) buildSingleNodeItem(ctx context.Context, nodeID int64) (map[string]any, bool, error) {
	node, err := s.store.GetNode(ctx, nodeID)
	if err != nil {
		return nil, false, err
	}
	metrics, _, _ := s.store.GetNodeRuntimeMetrics(ctx, nodeID)
	monthly, _, _ := s.store.GetLatestNodeMonthlyUsage(ctx, nodeID)
	pending, runningKind, runningDesired, runningStartedAt, runningCorrelation, _ := s.store.GetNodeJobSummary(ctx, nodeID)
	jobSummary := repo.NodeJobSummary{
		NodeID:             nodeID,
		Pending:            pending,
		RunningKind:        runningKind,
		RunningDesired:     runningDesired,
		RunningStartedAt:   runningStartedAt,
		RunningCorrelation: runningCorrelation,
	}
	metadata, ok, _ := s.store.GetNodeMetadata(ctx, nodeID)
	if !ok {
		metadata = repo.NodeMetadata{}
	}
	return s.buildNodeItem(node, metrics, monthly, jobSummary, metadata, time.Now().UTC()), true, nil
}

func (s *OpsService) buildNodeItem(n repo.Node, metrics repo.NodeRuntimeMetrics, monthly repo.NodeMonthlyUsage, jobSummary repo.NodeJobSummary, metadata repo.NodeMetadata, now time.Time) map[string]any {
	health := nodeHealth(n.Enabled, n.LastSeenAt, now)

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
			"quarantined_batches":    metrics.QuarantinedBatches,
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

func nodeHealth(enabled bool, lastSeenAt *time.Time, now time.Time) string {
	switch {
	case !enabled:
		return "disabled"
	case lastSeenAt != nil && now.Sub(*lastSeenAt) <= opsNodeStaleAfter:
		return "online"
	case lastSeenAt != nil:
		return "stale"
	}
	return "unknown"
}

func refreshNodeItemsHealth(items []map[string]any, now time.Time) {
	for _, item := range items {
		enabled, _ := item["enabled"].(bool)
		var lastSeen *time.Time
		if raw, _ := item["last_seen_at"].(string); raw != "" {
			if t, err := time.Parse(time.RFC3339, raw); err == nil {
				lastSeen = &t
			}
		}
		item["health"] = nodeHealth(enabled, lastSeen, now)
	}
}

type OpsLatestCache struct {
	mu        sync.RWMutex
	ttl       time.Duration
	items     []map[string]any
	expiresAt time.Time
}

func NewOpsLatestCache(ttl time.Duration) *OpsLatestCache {
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	return &OpsLatestCache{ttl: ttl}
}

func (c *OpsLatestCache) Get() ([]map[string]any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.items == nil || time.Now().UTC().After(c.expiresAt) {
		return nil, false
	}
	return cloneOpsItems(c.items), true
}

func (c *OpsLatestCache) Set(items []map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = cloneOpsItems(items)
	c.expiresAt = time.Now().UTC().Add(c.ttl)
}

func (c *OpsLatestCache) UpsertNode(item map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if item == nil || c.items == nil {
		return
	}
	nodeID, ok := int64FromAny(item["id"])
	if !ok || nodeID <= 0 {
		return
	}
	cp := cloneOpsItem(item)
	for i, existing := range c.items {
		if existingID, ok := int64FromAny(existing["id"]); ok && existingID == nodeID {
			c.items[i] = cp
			c.expiresAt = time.Now().UTC().Add(c.ttl)
			return
		}
	}
	c.items = append([]map[string]any{cp}, c.items...)
	c.expiresAt = time.Now().UTC().Add(c.ttl)
}

func (c *OpsLatestCache) RemoveNode(nodeID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil || nodeID <= 0 {
		return
	}
	out := c.items[:0]
	for _, item := range c.items {
		if existingID, ok := int64FromAny(item["id"]); ok && existingID == nodeID {
			continue
		}
		out = append(out, item)
	}
	c.items = out
	c.expiresAt = time.Now().UTC().Add(c.ttl)
}

func (c *OpsLatestCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = nil
	c.expiresAt = time.Time{}
}

func cloneOpsItems(items []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, cloneOpsItem(item))
	}
	return out
}

func cloneOpsItem(item map[string]any) map[string]any {
	cp := make(map[string]any, len(item))
	for k, v := range item {
		cp[k] = v
	}
	return cp
}

func int64FromAny(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), n == float64(int64(n))
	default:
		return 0, false
	}
}
