package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"neutrino/internal/config"
	"neutrino/internal/db"
	"neutrino/internal/repo"
)

func TestOpsServiceWarmupAndRefreshNodeUpdatesCachedItem(t *testing.T) {
	ctx := context.Background()
	store := newOpsServiceTestStore(t)
	node, err := store.CreateNode(ctx, repo.CreateNodeInput{
		Name:     "ops-cache-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC()
	if err := store.UpsertNodeRuntimeMetrics(ctx, node.ID, now, repo.NodeReportMetricsInput{CPUPercent: 10}); err != nil {
		t.Fatalf("upsert metrics: %v", err)
	}
	svc := NewOpsService(store)
	if err := svc.WarmUp(ctx); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	if err := store.UpsertNodeRuntimeMetrics(ctx, node.ID, now.Add(time.Second), repo.NodeReportMetricsInput{CPUPercent: 20}); err != nil {
		t.Fatalf("upsert changed metrics: %v", err)
	}
	items, err := svc.BuildNodeItems(ctx)
	if err != nil {
		t.Fatalf("build cached items: %v", err)
	}
	if got := cachedCPU(t, items); got != 10 {
		t.Fatalf("cache should retain warm value before invalidation, got %v", got)
	}
	if err := svc.RefreshNode(ctx, node.ID); err != nil {
		t.Fatalf("refresh node: %v", err)
	}
	items, err = svc.BuildNodeItems(ctx)
	if err != nil {
		t.Fatalf("build refreshed items: %v", err)
	}
	if got := cachedCPU(t, items); got != 20 {
		t.Fatalf("cache should refresh after invalidation, got %v", got)
	}
}

func TestOpsServiceRemoveNodeDeletesCachedItem(t *testing.T) {
	ctx := context.Background()
	store := newOpsServiceTestStore(t)
	node, err := store.CreateNode(ctx, repo.CreateNodeInput{
		Name:     "ops-cache-delete-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	svc := NewOpsService(store)
	if err := svc.WarmUp(ctx); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	svc.RemoveNode(node.ID)
	items, err := svc.BuildNodeItems(ctx)
	if err != nil {
		t.Fatalf("build cached items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("removed node should be gone from cache, got %+v", items)
	}
}

func TestOpsServiceBuildNodeItemsIncludesBatchMetadata(t *testing.T) {
	ctx := context.Background()
	store := newOpsServiceTestStore(t)
	node, err := store.CreateNode(ctx, repo.CreateNodeInput{
		Name:     "ops-metadata-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := store.UpsertNodeMetadata(ctx, node.ID, repo.UpsertNodeMetadataInput{Provider: "hetzner", Region: "fsn1"}); err != nil {
		t.Fatalf("upsert metadata: %v", err)
	}
	items, err := NewOpsService(store).BuildNodeItems(ctx)
	if err != nil {
		t.Fatalf("build items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	meta, ok := items[0]["metadata"].(repo.NodeMetadata)
	if !ok || meta.Provider != "hetzner" || meta.Region != "fsn1" {
		t.Fatalf("metadata missing from ops item: %+v", items[0]["metadata"])
	}
}

func TestOpsServiceCachedItemHealthGoesStaleWithoutNewReports(t *testing.T) {
	ctx := context.Background()
	store := newOpsServiceTestStore(t)
	node, err := store.CreateNode(ctx, repo.CreateNodeInput{
		Name:     "ops-stale-health-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	svc := NewOpsService(store)
	// Simulate a cache entry kept alive by some other chatty node: the item
	// was built while the node was "online", but its last report is now well
	// past the stale threshold.
	lastSeen := time.Now().UTC().Add(-opsNodeStaleAfter - time.Minute)
	svc.cache.Set([]map[string]any{{
		"id":           node.ID,
		"enabled":      true,
		"health":       "online",
		"last_seen_at": lastSeen.Format(time.RFC3339),
	}})
	items, err := svc.BuildNodeItems(ctx)
	if err != nil {
		t.Fatalf("build cached items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if got := items[0]["health"]; got != "stale" {
		t.Fatalf("cached item health should be recomputed to stale, got %v", got)
	}
}

func TestNodeHealth(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-time.Second)
	old := now.Add(-opsNodeStaleAfter - time.Second)
	cases := []struct {
		name     string
		enabled  bool
		lastSeen *time.Time
		want     string
	}{
		{"disabled", false, &recent, "disabled"},
		{"online", true, &recent, "online"},
		{"stale", true, &old, "stale"},
		{"unknown", true, nil, "unknown"},
	}
	for _, c := range cases {
		if got := nodeHealth(c.enabled, c.lastSeen, now); got != c.want {
			t.Fatalf("%s: nodeHealth=%q want %q", c.name, got, c.want)
		}
	}
}

func cachedCPU(t *testing.T, items []map[string]any) float64 {
	t.Helper()
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	metrics, ok := items[0]["agent_metrics"].(map[string]any)
	if !ok {
		t.Fatalf("agent metrics missing: %+v", items[0])
	}
	got, ok := metrics["cpu_percent"].(float64)
	if !ok {
		t.Fatalf("cpu percent missing: %+v", metrics)
	}
	return got
}

func newOpsServiceTestStore(t *testing.T) *repo.Store {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "ops-service-test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return repo.New(conn, config.Config{})
}
