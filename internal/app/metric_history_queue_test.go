package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"neutrino/internal/config"
	"neutrino/internal/db"
	"neutrino/internal/repo"
)

func TestHandleAPINodeReportHistoryQueueFullStillUpdatesLatest(t *testing.T) {
	ctx := context.Background()
	store := newMetricHistoryTestStore(t)
	node, err := store.CreateNode(ctx, repo.CreateNodeInput{
		Name:     "queue-full-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	a := &App{store: store, cfg: config.Config{}, metricHistoryQueue: newNodeMetricHistoryQueue(store, 0)}

	body, _ := json.Marshal(repo.NodeReportInput{
		ReportedAt: time.Now().UTC().Format(time.RFC3339),
		Metrics: &repo.NodeReportMetricsInput{
			CPUPercent:       12.5,
			MemoryBytes:      1024,
			MemoryTotalBytes: 4096,
			Details:          json.RawMessage(`{"interfaces":[{"name":"eth0"}]}`),
		},
	})
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/report", node.ID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Subject: pkix.Name{CommonName: fmt.Sprintf("node-%d", node.ID)}}}}
	rr := httptest.NewRecorder()

	a.handleAPINodeReport(rr, req, node.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("report got %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	latest, err := store.ListNodeRuntimeMetrics(ctx)
	if err != nil {
		t.Fatalf("latest metrics: %v", err)
	}
	if latest[node.ID].CPUPercent != 12.5 {
		t.Fatalf("latest metrics not updated: %+v", latest[node.ID])
	}
	series, err := store.ListNodeMetricSeries(ctx, node.ID, "1h", "raw", time.Now().UTC())
	if err != nil {
		t.Fatalf("metric series: %v", err)
	}
	if len(series) != 0 {
		t.Fatalf("full queue should drop historical sample, got %+v", series)
	}
	if a.metricHistoryQueue.Dropped() != 1 {
		t.Fatalf("expected 1 dropped history item, got %d", a.metricHistoryQueue.Dropped())
	}
	alerts, err := store.ListOpsAlerts(ctx, "active", 10)
	if err != nil {
		t.Fatalf("list alerts: %v", err)
	}
	if len(alerts) != 0 {
		t.Fatalf("report path should not synchronously write drop alert, got %+v", alerts)
	}
	a.metricHistoryQueue.syncDroppedAlert(ctx)
	alerts, err = store.ListOpsAlerts(ctx, "active", 10)
	if err != nil {
		t.Fatalf("list alerts after sync: %v", err)
	}
	if len(alerts) != 1 || alerts[0].Kind != "metric_history_dropped" {
		t.Fatalf("worker drop alert not written: %+v", alerts)
	}
}

func TestMetricHistoryQueueFlushesSamplesAndDetails(t *testing.T) {
	ctx := context.Background()
	store := newMetricHistoryTestStore(t)
	node, err := store.CreateNode(ctx, repo.CreateNodeInput{
		Name:     "queue-flush-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	q := newNodeMetricHistoryQueue(store, 4)
	sampledAt := time.Now().UTC().Truncate(time.Second)
	if !q.Enqueue(node.ID, sampledAt, repo.NodeReportMetricsInput{
		CPUPercent:       33,
		MemoryBytes:      100,
		MemoryTotalBytes: 200,
		Details:          json.RawMessage(`{"interfaces":[{"name":"eth0"}]}`),
	}) {
		t.Fatalf("enqueue failed")
	}
	q.flushRemaining(ctx)

	series, err := store.ListNodeMetricSeries(ctx, node.ID, "1h", "raw", sampledAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("metric series: %v", err)
	}
	if len(series) != 1 || series[0].CPUAvg != 33 {
		t.Fatalf("unexpected series after flush: %+v", series)
	}
	details, ok, err := store.GetLatestNodeMetricDetails(ctx, node.ID)
	if err != nil || !ok {
		t.Fatalf("details ok=%v err=%v", ok, err)
	}
	if !json.Valid(details.Data) {
		t.Fatalf("invalid details json: %s", string(details.Data))
	}
}

func TestMetricHistoryQueueFallsBackToDiskWhenMemoryFull(t *testing.T) {
	ctx := context.Background()
	store := newMetricHistoryTestStore(t)
	node, err := store.CreateNode(ctx, repo.CreateNodeInput{
		Name:     "queue-disk-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	q := newNodeMetricHistoryQueueWithDisk(store, 0, t.TempDir(), 1024*1024)
	sampledAt := time.Now().UTC().Truncate(time.Second)
	if !q.Enqueue(node.ID, sampledAt, repo.NodeReportMetricsInput{CPUPercent: 44, MemoryBytes: 100, MemoryTotalBytes: 200}) {
		t.Fatalf("enqueue with disk fallback failed")
	}
	if q.Dropped() != 0 {
		t.Fatalf("disk fallback should not count as dropped, got %d", q.Dropped())
	}
	if q.disk == nil {
		t.Fatalf("expected disk queue to be enabled")
	}
	if q.disk.ApproxFiles() != 1 {
		t.Fatalf("expected one disk backlog file, got %d", q.disk.ApproxFiles())
	}

	series, err := store.ListNodeMetricSeries(ctx, node.ID, "1h", "raw", sampledAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("metric series before disk flush: %v", err)
	}
	if len(series) != 0 {
		t.Fatalf("disk fallback should defer history write, got %+v", series)
	}

	q.flushDiskBacklog(ctx)
	series, err = store.ListNodeMetricSeries(ctx, node.ID, "1h", "raw", sampledAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("metric series after disk flush: %v", err)
	}
	if len(series) != 1 || series[0].CPUAvg != 44 {
		t.Fatalf("unexpected series after disk flush: %+v", series)
	}
	if q.disk.ApproxFiles() != 0 {
		t.Fatalf("disk backlog should be drained, got %d files", q.disk.ApproxFiles())
	}
}

func TestMetricHistoryQueueDiskBacklogDoesNotDuplicateAfterDetailError(t *testing.T) {
	ctx := context.Background()
	conn, err := db.Open(filepath.Join(t.TempDir(), "metric-history-detail-error-test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `
CREATE TRIGGER fail_metric_details
BEFORE INSERT ON node_metric_details
WHEN NEW.data_json LIKE '%"fail":true%'
BEGIN
	SELECT RAISE(ABORT, 'forced detail failure');
END;
`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	store := repo.New(conn, config.Config{})
	node, err := store.CreateNode(ctx, repo.CreateNodeInput{
		Name:     "queue-disk-detail-error-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	q := newNodeMetricHistoryQueueWithDisk(store, 0, t.TempDir(), 1024*1024)
	first := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	second := first.Add(time.Second)
	if !q.Enqueue(node.ID, first, repo.NodeReportMetricsInput{
		CPUPercent:       61,
		MemoryBytes:      100,
		MemoryTotalBytes: 200,
		Details:          json.RawMessage(`{"fail":true}`),
	}) {
		t.Fatalf("enqueue first disk item failed")
	}
	if !q.Enqueue(node.ID, second, repo.NodeReportMetricsInput{
		CPUPercent:       62,
		MemoryBytes:      100,
		MemoryTotalBytes: 200,
		Details:          json.RawMessage(`{"ok":true}`),
	}) {
		t.Fatalf("enqueue second disk item failed")
	}

	q.flushDiskBacklog(ctx)
	if q.disk.ApproxFiles() != 0 {
		t.Fatalf("disk backlog should drain detail-error item after sample commit, got %d files", q.disk.ApproxFiles())
	}
	q.flushDiskBacklog(ctx)
	series, err := store.ListNodeMetricSeries(ctx, node.ID, "1h", "raw", second.Add(time.Minute))
	if err != nil {
		t.Fatalf("metric series: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected exactly two samples after repeated disk flush, got %d: %+v", len(series), series)
	}
	if series[0].CPUAvg != 61 || series[1].CPUAvg != 62 {
		t.Fatalf("unexpected samples after disk flush: %+v", series)
	}
}

func TestMetricHistoryQueueDropsWhenMemoryAndDiskFull(t *testing.T) {
	ctx := context.Background()
	store := newMetricHistoryTestStore(t)
	node, err := store.CreateNode(ctx, repo.CreateNodeInput{
		Name:     "queue-disk-full-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	q := newNodeMetricHistoryQueueWithDisk(store, 0, t.TempDir(), 1)
	if q.Enqueue(node.ID, time.Now().UTC(), repo.NodeReportMetricsInput{CPUPercent: 55}) {
		t.Fatalf("enqueue should fail when memory and disk are full")
	}
	if q.Dropped() != 1 {
		t.Fatalf("expected one dropped item, got %d", q.Dropped())
	}
	q.syncDroppedAlert(ctx)
	alerts, err := store.ListOpsAlerts(ctx, "active", 10)
	if err != nil {
		t.Fatalf("list alerts: %v", err)
	}
	if len(alerts) != 1 || alerts[0].Kind != "metric_history_dropped" {
		t.Fatalf("drop alert not written: %+v", alerts)
	}
}

func newMetricHistoryTestStore(t *testing.T) *repo.Store {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "metric-history-test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return repo.New(conn, config.Config{})
}
