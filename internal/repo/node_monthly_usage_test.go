package repo

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func createMonthlyUsageTestNode(t *testing.T, s *Store, suffix string) Node {
	t.Helper()
	node, err := s.CreateNode(context.Background(), CreateNodeInput{
		Name:      fmt.Sprintf("node-month-%s-%d", suffix, time.Now().UnixNano()),
		CoreType:  "xray",
		Protocol:  "vless_reality",
		Host:      "node.example.com",
		Port:      443,
		Transport: "tcp",
		Security:  "reality",
		Flow:      "xtls-rprx-vision",
		SNI:       "example.com",
		Enabled:   true,
		Managed:   true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	return node
}

func TestUpsertNodeMonthlyUsageFromReport_AccumulatesFromRawTotals(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	node := createMonthlyUsageTestNode(t, s, "raw")

	at := time.Date(2026, 3, 30, 10, 0, 0, 0, time.UTC)
	if err := s.UpsertNodeMonthlyUsageFromReport(ctx, node.ID, NodeReportMetricsInput{
		MonthKey:      "2026-03",
		MonthTimezone: "Asia/Shanghai",
		NetSource:     "proc:/host/proc/1/net",
		NetRXTotal:    1000,
		NetTXTotal:    2000,
	}, at); err != nil {
		t.Fatalf("upsert first: %v", err)
	}
	items, err := s.ListLatestNodeMonthlyUsage(ctx)
	if err != nil {
		t.Fatalf("list latest: %v", err)
	}
	item := items[node.ID]
	if item.RXBytes != 0 || item.TXBytes != 0 {
		t.Fatalf("first raw sample should only establish baseline, got rx=%d tx=%d", item.RXBytes, item.TXBytes)
	}
	if item.BaselineRX != 1000 || item.BaselineTX != 2000 || item.LastRXTotal != 1000 || item.LastTXTotal != 2000 {
		t.Fatalf("unexpected baseline/last totals: %+v", item)
	}

	if err := s.UpsertNodeMonthlyUsageFromReport(ctx, node.ID, NodeReportMetricsInput{
		MonthKey:      "2026-03",
		MonthTimezone: "Asia/Shanghai",
		NetSource:     "proc:/host/proc/1/net",
		NetRXTotal:    1600,
		NetTXTotal:    2800,
	}, at.Add(time.Minute)); err != nil {
		t.Fatalf("upsert second: %v", err)
	}
	items, err = s.ListLatestNodeMonthlyUsage(ctx)
	if err != nil {
		t.Fatalf("list latest after second: %v", err)
	}
	item = items[node.ID]
	if item.RXBytes != 600 || item.TXBytes != 800 {
		t.Fatalf("unexpected month bytes rx=%d tx=%d", item.RXBytes, item.TXBytes)
	}
	if item.LastReportedAt == nil || !item.LastReportedAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("unexpected last reported at: %+v", item.LastReportedAt)
	}
}

func TestUpsertNodeMonthlyUsageFromReport_UsesReportedAtMonthWhenPayloadIsStale(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	node := createMonthlyUsageTestNode(t, s, "stale-payload")

	at := time.Date(2026, 4, 1, 0, 30, 0, 0, time.UTC)
	if err := s.UpsertNodeMonthlyUsageFromReport(ctx, node.ID, NodeReportMetricsInput{
		MonthKey:      "2026-03",
		MonthTimezone: "UTC",
		NetSource:     "proc:/host/proc/1/net",
		NetRXTotal:    1000,
		NetTXTotal:    2000,
	}, at); err != nil {
		t.Fatalf("upsert stale payload month: %v", err)
	}

	item := mustLatestNodeMonthUsage(t, s, node.ID)
	if item.MonthKey != "2026-04" {
		t.Fatalf("expected reported_at month to win over stale payload month, got %q", item.MonthKey)
	}
}

func TestUpsertNodeMonthlyUsageFromReport_PreservesTotalsAcrossCounterReset(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	node := createMonthlyUsageTestNode(t, s, "reset")
	at := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	_ = s.UpsertNodeMonthlyUsageFromReport(ctx, node.ID, NodeReportMetricsInput{
		MonthKey: "2026-04", MonthTimezone: "UTC", NetSource: "proc:/host/proc/1/net", NetRXTotal: 1000, NetTXTotal: 2000,
	}, at)
	_ = s.UpsertNodeMonthlyUsageFromReport(ctx, node.ID, NodeReportMetricsInput{
		MonthKey: "2026-04", MonthTimezone: "UTC", NetSource: "proc:/host/proc/1/net", NetRXTotal: 1400, NetTXTotal: 2600,
	}, at.Add(time.Minute))
	_ = s.UpsertNodeMonthlyUsageFromReport(ctx, node.ID, NodeReportMetricsInput{
		MonthKey: "2026-04", MonthTimezone: "UTC", NetSource: "proc:/host/proc/1/net", NetRXTotal: 100, NetTXTotal: 200,
	}, at.Add(2*time.Minute))
	_ = s.UpsertNodeMonthlyUsageFromReport(ctx, node.ID, NodeReportMetricsInput{
		MonthKey: "2026-04", MonthTimezone: "UTC", NetSource: "proc:/host/proc/1/net", NetRXTotal: 180, NetTXTotal: 260,
	}, at.Add(3*time.Minute))

	item := mustLatestNodeMonthUsage(t, s, node.ID)
	if item.RXBytes != 480 || item.TXBytes != 660 {
		t.Fatalf("counter reset should preserve and continue totals, got rx=%d tx=%d", item.RXBytes, item.TXBytes)
	}
	if item.BaselineRX != 100 || item.BaselineTX != 200 {
		t.Fatalf("expected reset baseline to move to raw reset totals, got %+v", item)
	}
}

func TestUpsertNodeMonthlyUsageFromReport_RebasesOnCounterSourceChange(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	node := createMonthlyUsageTestNode(t, s, "source")
	at := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	_ = s.UpsertNodeMonthlyUsageFromReport(ctx, node.ID, NodeReportMetricsInput{
		MonthKey: "2026-04", MonthTimezone: "UTC", NetSource: "gopsutil", NetRXTotal: 1000, NetTXTotal: 2000,
	}, at)
	_ = s.UpsertNodeMonthlyUsageFromReport(ctx, node.ID, NodeReportMetricsInput{
		MonthKey: "2026-04", MonthTimezone: "UTC", NetSource: "gopsutil", NetRXTotal: 1600, NetTXTotal: 2600,
	}, at.Add(time.Minute))
	_ = s.UpsertNodeMonthlyUsageFromReport(ctx, node.ID, NodeReportMetricsInput{
		MonthKey: "2026-04", MonthTimezone: "UTC", NetSource: "proc:/host/proc/1/net", NetRXTotal: 500000, NetTXTotal: 800000,
	}, at.Add(2*time.Minute))
	_ = s.UpsertNodeMonthlyUsageFromReport(ctx, node.ID, NodeReportMetricsInput{
		MonthKey: "2026-04", MonthTimezone: "UTC", NetSource: "proc:/host/proc/1/net", NetRXTotal: 500100, NetTXTotal: 800250,
	}, at.Add(3*time.Minute))

	item := mustLatestNodeMonthUsage(t, s, node.ID)
	if item.RXBytes != 700 || item.TXBytes != 850 {
		t.Fatalf("source change should preserve totals and continue from new source, got rx=%d tx=%d", item.RXBytes, item.TXBytes)
	}
	if item.CounterSource != "proc:/host/proc/1/net" {
		t.Fatalf("unexpected source %q", item.CounterSource)
	}
}

func TestUpsertNodeMonthlyUsageFromReport_IgnoresOutOfOrderReports(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	node := createMonthlyUsageTestNode(t, s, "order")
	at := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	_ = s.UpsertNodeMonthlyUsageFromReport(ctx, node.ID, NodeReportMetricsInput{
		MonthKey: "2026-04", MonthTimezone: "UTC", NetSource: "proc:/host/proc/1/net", NetRXTotal: 1000, NetTXTotal: 1000,
	}, at)
	_ = s.UpsertNodeMonthlyUsageFromReport(ctx, node.ID, NodeReportMetricsInput{
		MonthKey: "2026-04", MonthTimezone: "UTC", NetSource: "proc:/host/proc/1/net", NetRXTotal: 2000, NetTXTotal: 3000,
	}, at.Add(time.Minute))
	_ = s.UpsertNodeMonthlyUsageFromReport(ctx, node.ID, NodeReportMetricsInput{
		MonthKey: "2026-04", MonthTimezone: "UTC", NetSource: "proc:/host/proc/1/net", NetRXTotal: 2500, NetTXTotal: 3500,
	}, at.Add(30*time.Second))

	item := mustLatestNodeMonthUsage(t, s, node.ID)
	if item.RXBytes != 1000 || item.TXBytes != 2000 {
		t.Fatalf("out-of-order report changed totals: %+v", item)
	}
}

func TestUpsertNodeMonthlyUsageFromReport_PicksNewestMonthPerNode(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	node := createMonthlyUsageTestNode(t, s, "next")

	marchAt := time.Date(2026, 3, 31, 23, 59, 0, 0, time.UTC)
	aprilAt := marchAt.Add(2 * time.Minute)

	_ = s.UpsertNodeMonthlyUsageFromReport(ctx, node.ID, NodeReportMetricsInput{
		MonthKey: "2026-03", MonthTimezone: "UTC", NetSource: "gopsutil", NetRXTotal: 100, NetTXTotal: 200,
	}, marchAt)
	_ = s.UpsertNodeMonthlyUsageFromReport(ctx, node.ID, NodeReportMetricsInput{
		MonthKey: "2026-04", MonthTimezone: "UTC", NetSource: "gopsutil", NetRXTotal: 50, NetTXTotal: 60,
	}, aprilAt)

	item := mustLatestNodeMonthUsage(t, s, node.ID)
	if item.MonthKey != "2026-04" {
		t.Fatalf("expected newest month row, got %q", item.MonthKey)
	}
	if item.RXBytes != 0 || item.TXBytes != 0 {
		t.Fatalf("unexpected april bytes after baseline: rx=%d tx=%d", item.RXBytes, item.TXBytes)
	}
}

func TestListLatestNodeMonthlyUsage_PicksNewestMonthBeforeNewestReport(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	node := createMonthlyUsageTestNode(t, s, "latest-month")

	if _, err := s.RawDB().ExecContext(ctx, `
	INSERT INTO node_monthly_usage(
		node_id, month_key, timezone_name, rx_bytes, tx_bytes,
		baseline_rx_total, baseline_tx_total, last_rx_total, last_tx_total,
		counter_source, last_reported_at, updated_at
	)
	VALUES
		(?, '2026-03', 'UTC', 900, 90, 0, 0, 0, 0, 'test', '2026-04-20T00:00:00Z', '2026-04-20T00:00:00Z'),
		(?, '2026-04', 'UTC', 100, 10, 0, 0, 0, 0, 'test', '2026-04-01T00:00:00Z', '2026-04-01T00:00:00Z');
	`, node.ID, node.ID); err != nil {
		t.Fatalf("seed monthly usage rows: %v", err)
	}

	item := mustLatestNodeMonthUsage(t, s, node.ID)
	if item.MonthKey != "2026-04" {
		t.Fatalf("expected newest month row, got %q with rx=%d", item.MonthKey, item.RXBytes)
	}
	if item.RXBytes != 100 || item.TXBytes != 10 {
		t.Fatalf("unexpected newest-month bytes: rx=%d tx=%d", item.RXBytes, item.TXBytes)
	}
}

func TestListLatestNodeMonthlyUsage_SkipsMalformedMonthBeforeValidMonth(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	node := createMonthlyUsageTestNode(t, s, "bad-month")

	if _, err := s.RawDB().ExecContext(ctx, `
		INSERT INTO node_monthly_usage(
			node_id, month_key, timezone_name, rx_bytes, tx_bytes,
			baseline_rx_total, baseline_tx_total, last_rx_total, last_tx_total,
			counter_source, last_reported_at, updated_at
		)
		VALUES
			(?, '9999-99', 'UTC', 900, 90, 0, 0, 0, 0, 'test', '2026-04-20T00:00:00Z', '2026-04-20T00:00:00Z'),
			(?, '2026-04', 'UTC', 100, 10, 0, 0, 0, 0, 'test', '2026-04-01T00:00:00Z', '2026-04-01T00:00:00Z');
		`, node.ID, node.ID); err != nil {
		t.Fatalf("seed monthly usage rows: %v", err)
	}

	item := mustLatestNodeMonthUsage(t, s, node.ID)
	if item.MonthKey != "2026-04" {
		t.Fatalf("expected valid month row, got %q with rx=%d", item.MonthKey, item.RXBytes)
	}
	if item.RXBytes != 100 || item.TXBytes != 10 {
		t.Fatalf("unexpected valid-month bytes: rx=%d tx=%d", item.RXBytes, item.TXBytes)
	}
}

func mustLatestNodeMonthUsage(t *testing.T, s *Store, nodeID int64) NodeMonthlyUsage {
	t.Helper()
	items, err := s.ListLatestNodeMonthlyUsage(context.Background())
	if err != nil {
		t.Fatalf("list latest: %v", err)
	}
	item, ok := items[nodeID]
	if !ok {
		t.Fatalf("missing node usage for node_id=%d", nodeID)
	}
	return item
}
