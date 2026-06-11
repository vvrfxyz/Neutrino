package repo

import (
	"context"
	"testing"
	"time"
)

func TestOpsAlertDedupeAndResolve(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "alert-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "alert.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	nodeID := node.ID

	id1, err := s.UpsertOpsAlert(ctx, &nodeID, "node_stale", "warning", "first", "node:42:stale", now)
	if err != nil {
		t.Fatalf("upsert alert: %v", err)
	}
	id2, err := s.UpsertOpsAlert(ctx, &nodeID, "node_stale", "critical", "second", "node:42:stale", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("upsert duplicate alert: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("duplicate alert id=%d, want %d", id2, id1)
	}

	alerts, err := s.ListOpsAlerts(ctx, "active", 10)
	if err != nil {
		t.Fatalf("list alerts: %v", err)
	}
	if len(alerts) != 1 || alerts[0].Message != "second" || alerts[0].Severity != "critical" {
		t.Fatalf("alert was not updated: %+v", alerts)
	}
	pendingNotifications, err := s.ListPendingAlerts(ctx, 10)
	if err != nil {
		t.Fatalf("list pending notifications: %v", err)
	}
	if len(pendingNotifications) != 1 || pendingNotifications[0].Type != "system" || pendingNotifications[0].Threshold != "critical" {
		t.Fatalf("pending ops notification mismatch: %+v", pendingNotifications)
	}

	ok, err := s.ResolveOpsAlert(ctx, "node:42:stale", now.Add(2*time.Minute))
	if err != nil || !ok {
		t.Fatalf("resolve ok=%v err=%v", ok, err)
	}
	alerts, err = s.ListOpsAlerts(ctx, "active", 10)
	if err != nil {
		t.Fatalf("list active after resolve: %v", err)
	}
	if len(alerts) != 0 {
		t.Fatalf("active alerts after resolve: %+v", alerts)
	}
	alerts, err = s.ListOpsAlerts(ctx, "resolved", 10)
	if err != nil {
		t.Fatalf("list resolved: %v", err)
	}
	if len(alerts) != 1 || alerts[0].ResolvedAt == nil {
		t.Fatalf("resolved alert missing: %+v", alerts)
	}

	id3, err := s.UpsertOpsAlert(ctx, &nodeID, "node_stale", "warning", "third", "node:42:stale", now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("upsert recurring alert: %v", err)
	}
	if id3 == id1 {
		t.Fatalf("recurring alert reused resolved id=%d", id3)
	}
	ok, err = s.ResolveOpsAlert(ctx, "node:42:stale", now.Add(4*time.Minute))
	if err != nil || !ok {
		t.Fatalf("resolve recurring ok=%v err=%v", ok, err)
	}
	alerts, err = s.ListOpsAlerts(ctx, "resolved", 10)
	if err != nil {
		t.Fatalf("list recurring resolved: %v", err)
	}
	if len(alerts) != 2 {
		t.Fatalf("resolved alert history len=%d, want 2: %+v", len(alerts), alerts)
	}
	pendingNotifications, err = s.ListPendingAlerts(ctx, 10)
	if err != nil {
		t.Fatalf("list recurring pending notifications: %v", err)
	}
	if len(pendingNotifications) != 2 {
		t.Fatalf("pending notification len=%d, want 2: %+v", len(pendingNotifications), pendingNotifications)
	}
}

func TestNodeMetadataNormalizesTagsAndCost(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "metadata-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "metadata.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	renewAt := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	if err := s.UpsertNodeMetadata(ctx, node.ID, UpsertNodeMetadataInput{
		Provider:         "Provider",
		Region:           "region-1",
		Tags:             []string{"edge", "edge", "premium"},
		MonthlyCostCents: -10,
		Currency:         "usd",
		RenewAt:          &renewAt,
	}); err != nil {
		t.Fatalf("upsert metadata: %v", err)
	}
	item, ok, err := s.GetNodeMetadata(ctx, node.ID)
	if err != nil || !ok {
		t.Fatalf("get metadata ok=%v err=%v", ok, err)
	}
	if item.MonthlyCostCents != 0 || item.Currency != "USD" {
		t.Fatalf("cost/currency not normalized: %+v", item)
	}
	if len(item.Tags) != 2 || item.Tags[0] != "edge" || item.Tags[1] != "premium" {
		t.Fatalf("tags not normalized: %+v", item.Tags)
	}
	if item.RenewAt == nil || !item.RenewAt.Equal(renewAt) {
		t.Fatalf("renew at mismatch: %+v", item.RenewAt)
	}
}

func TestProbeResultsAndCleanupOpsData(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "cleanup-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "cleanup.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	old := now.AddDate(0, 0, -40)
	if _, err := s.InsertNodeProbeResult(ctx, node.ID, InsertNodeProbeResultInput{Kind: "probe_tcp", Target: "example.com:443", Success: true, CheckedAt: old}); err != nil {
		t.Fatalf("insert probe: %v", err)
	}
	if err := s.InsertNodeMetricSample(ctx, node.ID, old, NodeReportMetricsInput{CPUPercent: 1}); err != nil {
		t.Fatalf("insert sample: %v", err)
	}
	if err := s.InsertNodeMetricDetails(ctx, node.ID, old, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("insert details: %v", err)
	}
	if _, err := s.UpsertOpsAlert(ctx, nil, "probe_failed", "warning", "old", "old-alert", old); err != nil {
		t.Fatalf("upsert alert: %v", err)
	}
	if ok, err := s.ResolveOpsAlert(ctx, "old-alert", old.Add(time.Hour)); err != nil || !ok {
		t.Fatalf("resolve old alert ok=%v err=%v", ok, err)
	}
	counts, err := s.CleanupOpsData(ctx, now, 14, 72, 30, 30, 100)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if counts.MetricSamples != 1 || counts.MetricDetails != 1 || counts.ProbeResults != 1 || counts.OpsAlerts != 1 {
		t.Fatalf("unexpected cleanup counts: %+v", counts)
	}
}

func TestCleanupOpsDataDrainsBacklogBeyondOneBatch(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "cleanup-backlog-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "cleanup-backlog.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	old := now.AddDate(0, 0, -40)
	const backlog = 7
	for i := 0; i < backlog; i++ {
		if _, err := s.InsertNodeProbeResult(ctx, node.ID, InsertNodeProbeResultInput{Kind: "probe_tcp", Target: "example.com:443", Success: true, CheckedAt: old.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatalf("insert probe %d: %v", i, err)
		}
	}
	// batchSize=2 with a backlog of 7 needs 4 batches; a single call must
	// drain the whole backlog instead of deleting one batch per prune tick.
	counts, err := s.CleanupOpsData(ctx, now, 14, 72, 30, 30, 2)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if counts.ProbeResults != backlog {
		t.Fatalf("expected %d probe results pruned in one call, got %d", backlog, counts.ProbeResults)
	}
	var remaining int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_probe_results WHERE node_id = ?;`, node.ID).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expected backlog drained, %d rows remain", remaining)
	}
}
