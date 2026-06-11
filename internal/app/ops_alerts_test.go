package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"neutrino/internal/config"
	"neutrino/internal/db"
	"neutrino/internal/repo"
)

func TestSyncProbeOpsAlertCreatesAndResolves(t *testing.T) {
	ctx := context.Background()
	a, nodeID := newOpsAlertTestApp(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := a.syncProbeOpsAlert(ctx, nodeID, "probe_tcp", "example.com:443", false, "timeout", now); err != nil {
		t.Fatalf("sync failed probe alert: %v", err)
	}
	alerts, err := a.store.ListOpsAlerts(ctx, "active", 10)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(alerts) != 1 || alerts[0].Kind != "probe_failed" {
		t.Fatalf("active probe alert missing: %+v", alerts)
	}

	if err := a.syncProbeOpsAlert(ctx, nodeID, "probe_tcp", "example.com:443", true, "", now.Add(time.Minute)); err != nil {
		t.Fatalf("sync recovered probe alert: %v", err)
	}
	alerts, err = a.store.ListOpsAlerts(ctx, "active", 10)
	if err != nil {
		t.Fatalf("list active after recovery: %v", err)
	}
	if len(alerts) != 0 {
		t.Fatalf("active probe alert after recovery: %+v", alerts)
	}
}

func TestSyncNodeOpsAlertsCreatesAndResolvesHealthAlerts(t *testing.T) {
	ctx := context.Background()
	a, nodeID := newOpsAlertTestApp(t)
	now := time.Now().UTC().Truncate(time.Second)

	stale := map[string]any{
		"id":      nodeID,
		"name":    "edge-a",
		"enabled": true,
		"health":  "stale",
	}
	if err := a.syncNodeOpsAlerts(ctx, stale, now); err != nil {
		t.Fatalf("sync stale alerts: %v", err)
	}
	alerts, err := a.store.ListOpsAlerts(ctx, "active", 10)
	if err != nil {
		t.Fatalf("list active stale: %v", err)
	}
	if len(alerts) != 2 {
		t.Fatalf("active stale/missing alerts len=%d, want 2: %+v", len(alerts), alerts)
	}

	healthy := map[string]any{
		"id":      nodeID,
		"name":    "edge-a",
		"enabled": true,
		"health":  "online",
		"agent_metrics": map[string]any{
			"disk_used_percent":  10.0,
			"memory_bytes":       int64(100),
			"memory_total_bytes": int64(1000),
			"queue_batches":      int64(0),
			"queue_bytes":        int64(0),
		},
	}
	if err := a.syncNodeOpsAlerts(ctx, healthy, now.Add(time.Minute)); err != nil {
		t.Fatalf("sync healthy alerts: %v", err)
	}
	alerts, err = a.store.ListOpsAlerts(ctx, "active", 10)
	if err != nil {
		t.Fatalf("list active healthy: %v", err)
	}
	if len(alerts) != 0 {
		t.Fatalf("active alerts after healthy sync: %+v", alerts)
	}
}

func TestSyncNodeOpsAlertsClearsGeneratedAlertsWhenDisabled(t *testing.T) {
	ctx := context.Background()
	a, nodeID := newOpsAlertTestApp(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := a.syncNodeOpsAlerts(ctx, map[string]any{
		"id":      nodeID,
		"name":    "edge-a",
		"enabled": true,
		"health":  "online",
		"agent_metrics": map[string]any{
			"disk_used_percent":  96.0,
			"memory_bytes":       int64(950),
			"memory_total_bytes": int64(1000),
			"queue_batches":      int64(200),
			"queue_bytes":        int64(0),
		},
		"desired_xray_version": "v2",
		"applied_xray_version": "v1",
	}, now); err != nil {
		t.Fatalf("sync active alerts: %v", err)
	}
	alerts, err := a.store.ListOpsAlerts(ctx, "active", 20)
	if err != nil {
		t.Fatalf("list active before disable: %v", err)
	}
	if len(alerts) == 0 {
		t.Fatalf("expected active generated alerts before disable")
	}

	if err := a.syncNodeOpsAlerts(ctx, map[string]any{
		"id":      nodeID,
		"name":    "edge-a",
		"enabled": false,
		"health":  "disabled",
	}, now.Add(time.Minute)); err != nil {
		t.Fatalf("sync disabled alerts: %v", err)
	}
	alerts, err = a.store.ListOpsAlerts(ctx, "active", 20)
	if err != nil {
		t.Fatalf("list active after disable: %v", err)
	}
	if len(alerts) != 0 {
		t.Fatalf("active generated alerts after disable: %+v", alerts)
	}
}

func TestSyncNodeOpsAlertsRaisesAndResolvesUsageQuarantine(t *testing.T) {
	ctx := context.Background()
	a, nodeID := newOpsAlertTestApp(t)
	now := time.Now().UTC().Truncate(time.Second)

	quarantined := map[string]any{
		"id":      nodeID,
		"name":    "edge-a",
		"enabled": true,
		"health":  "online",
		"agent_metrics": map[string]any{
			"disk_used_percent":   10.0,
			"memory_bytes":        int64(100),
			"memory_total_bytes":  int64(1000),
			"queue_batches":       int64(0),
			"queue_bytes":         int64(0),
			"quarantined_batches": int64(2),
		},
	}
	if err := a.syncNodeOpsAlerts(ctx, quarantined, now); err != nil {
		t.Fatalf("sync quarantined alerts: %v", err)
	}
	alerts, err := a.store.ListOpsAlerts(ctx, "active", 10)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(alerts) != 1 || alerts[0].Kind != "usage_quarantined" || alerts[0].Severity != "critical" {
		t.Fatalf("usage_quarantined critical alert missing: %+v", alerts)
	}

	cleared := map[string]any{
		"id":      nodeID,
		"name":    "edge-a",
		"enabled": true,
		"health":  "online",
		"agent_metrics": map[string]any{
			"disk_used_percent":   10.0,
			"memory_bytes":        int64(100),
			"memory_total_bytes":  int64(1000),
			"queue_batches":       int64(0),
			"queue_bytes":         int64(0),
			"quarantined_batches": int64(0),
		},
	}
	if err := a.syncNodeOpsAlerts(ctx, cleared, now.Add(time.Minute)); err != nil {
		t.Fatalf("sync cleared alerts: %v", err)
	}
	alerts, err = a.store.ListOpsAlerts(ctx, "active", 10)
	if err != nil {
		t.Fatalf("list active after clear: %v", err)
	}
	if len(alerts) != 0 {
		t.Fatalf("active alerts after clear: %+v", alerts)
	}
}

func newOpsAlertTestApp(t *testing.T) (*App, int64) {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "ops-alerts.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	store := repo.New(conn, config.Config{})
	node, err := store.CreateNode(context.Background(), repo.CreateNodeInput{
		Name:     "ops-alert-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "ops-alert.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	return &App{store: store}, node.ID
}
