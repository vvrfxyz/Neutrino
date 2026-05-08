package repo

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestApplyNodeReportWritesLatestSampleDetailsAndStaticFacts(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "metrics-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "metrics.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	reportedAt := time.Now().UTC().Truncate(time.Second)
	err = s.ApplyNodeReport(ctx, node.ID, NodeReportInput{
		ReportedAt: reportedAt.Format(time.RFC3339),
		Metrics: &NodeReportMetricsInput{
			CPUPercent:           42.5,
			Load1:                0.4,
			Load5:                0.5,
			Load15:               0.6,
			MemoryBytes:          1024,
			MemoryTotalBytes:     4096,
			MemoryAvailableBytes: 2048,
			SwapUsedBytes:        128,
			SwapTotalBytes:       512,
			InboundBPS:           1000,
			OutboundBPS:          2000,
			DiskTotalBytes:       10000,
			DiskUsedBytes:        6000,
			DiskFreeBytes:        4000,
			DiskUsedPercent:      60,
			DiskReadBPS:          300,
			DiskWriteBPS:         400,
			TCPConnections:       12,
			UDPConnections:       3,
			ProcessCount:         88,
			UptimeSec:            70,
			SystemUptimeSec:      8000,
			AgentUptimeSec:       70,
			BootTime:             reportedAt.Add(-8000 * time.Second).Format(time.RFC3339),
			QueueBytes:           900,
			QueueBatches:         2,
			Goroutines:           34,
			AgentVersion:         "agent-test",
			XrayVersion:          "26.2.6",
			XrayConfigVersion:    "cfg-a",
			Details:              json.RawMessage(`{"interfaces":[{"name":"eth0","rx_bps":1000}]}`),
		},
		StaticFacts: &NodeStaticFactsInput{
			OSName:           "Ubuntu",
			OSVersion:        "24.04",
			Kernel:           "Linux",
			KernelVersion:    "6.8",
			Arch:             "amd64",
			Hostname:         "metrics-host",
			Virtualization:   "kvm",
			CPUModel:         "EPYC",
			CPUPhysicalCores: 2,
			CPULogicalCores:  4,
			AgentVersion:     "agent-test",
			XrayVersion:      "26.2.6",
			FactsJSON:        json.RawMessage(`{"extra":"ok"}`),
		},
	})
	if err != nil {
		t.Fatalf("apply report: %v", err)
	}

	latest, err := s.ListNodeRuntimeMetrics(ctx)
	if err != nil {
		t.Fatalf("latest metrics: %v", err)
	}
	m := latest[node.ID]
	if m.Load1 != 0.4 || m.MemoryTotalBytes != 4096 || m.TCPConnections != 12 || m.AgentVersion != "agent-test" {
		t.Fatalf("expanded latest metrics not stored: %+v", m)
	}

	series, err := s.ListNodeMetricSeries(ctx, node.ID, "1h", "raw", reportedAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("metric series: %v", err)
	}
	if len(series) != 1 || series[0].SampleCount != 1 || series[0].CPUAvg != 42.5 {
		t.Fatalf("unexpected series: %+v", series)
	}
	if series[0].MemoryUsedPercentAvg != 25 || series[0].MemoryUsedPercentMax != 25 {
		t.Fatalf("unexpected memory percent series: %+v", series[0])
	}

	details, ok, err := s.GetLatestNodeMetricDetails(ctx, node.ID)
	if err != nil || !ok {
		t.Fatalf("details ok=%v err=%v", ok, err)
	}
	if !json.Valid(details.Data) {
		t.Fatalf("details are not valid json: %s", string(details.Data))
	}

	facts, ok, err := s.GetLatestNodeStaticFacts(ctx, node.ID)
	if err != nil || !ok {
		t.Fatalf("facts ok=%v err=%v", ok, err)
	}
	if facts.OSName != "Ubuntu" || facts.CPULogicalCores != 4 || facts.DataHash == "" {
		t.Fatalf("unexpected facts: %+v", facts)
	}

	if err := s.UpsertNodeStaticFacts(ctx, node.ID, reportedAt.Add(time.Minute), NodeStaticFactsInput{
		OSName:           "Ubuntu",
		OSVersion:        "24.04",
		Kernel:           "Linux",
		KernelVersion:    "6.8",
		Arch:             "amd64",
		Hostname:         "metrics-host",
		Virtualization:   "kvm",
		CPUModel:         "EPYC",
		CPUPhysicalCores: 2,
		CPULogicalCores:  4,
		AgentVersion:     "agent-test",
		XrayVersion:      "26.2.6",
		FactsJSON:        json.RawMessage(`{"extra":"ok"}`),
	}); err != nil {
		t.Fatalf("upsert duplicate facts: %v", err)
	}
	history, err := s.ListNodeStaticFacts(ctx, node.ID, 10)
	if err != nil {
		t.Fatalf("facts history: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("duplicate facts created %d rows", len(history))
	}
}

func TestListNodeMetricSeriesRejectsUnsupportedRangeAndStep(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.ListNodeMetricSeries(ctx, 1, "30d", "raw", time.Now()); err == nil {
		t.Fatalf("expected unsupported range error")
	}
	if _, err := s.ListNodeMetricSeries(ctx, 1, "1h", "30s", time.Now()); err == nil {
		t.Fatalf("expected unsupported step error")
	}
}

func TestStaticFactsCanonicalHashIgnoresJSONKeyOrder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "canonical-facts-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "facts.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	base := NodeStaticFactsInput{
		OSName:      "Ubuntu",
		OSVersion:   "24.04",
		Kernel:      "Linux",
		Arch:        "amd64",
		Hostname:    "facts-host",
		FactsJSON:   json.RawMessage(`{"b":2,"a":{"y":2,"x":1}}`),
		XrayVersion: "26.2.6",
	}
	if err := s.UpsertNodeStaticFacts(ctx, node.ID, time.Now().UTC(), base); err != nil {
		t.Fatalf("upsert facts: %v", err)
	}
	base.FactsJSON = json.RawMessage(`{ "a" : { "x" : 1, "y" : 2 }, "b" : 2 }`)
	if err := s.UpsertNodeStaticFacts(ctx, node.ID, time.Now().UTC().Add(time.Minute), base); err != nil {
		t.Fatalf("upsert reordered facts: %v", err)
	}
	history, err := s.ListNodeStaticFacts(ctx, node.ID, 10)
	if err != nil {
		t.Fatalf("list facts: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("reordered facts should not create duplicate rows, got %d", len(history))
	}
	if string(history[0].FactsJSON) != `{"a":{"x":1,"y":2},"b":2}` {
		t.Fatalf("facts_json not canonical: %s", string(history[0].FactsJSON))
	}

	base.XrayVersion = "26.3.0"
	if err := s.UpsertNodeStaticFacts(ctx, node.ID, time.Now().UTC().Add(2*time.Minute), base); err != nil {
		t.Fatalf("upsert changed facts: %v", err)
	}
	history, err = s.ListNodeStaticFacts(ctx, node.ID, 10)
	if err != nil {
		t.Fatalf("list changed facts: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("changed structured facts should create a new row, got %d", len(history))
	}
}

func TestListNodeMetricSeriesSupportsPlannedSteps(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "metric-step-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "steps.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		if err := s.InsertNodeMetricSample(ctx, node.ID, now.Add(time.Duration(i)*30*time.Second), NodeReportMetricsInput{CPUPercent: float64(10 + i), MemoryBytes: 100, MemoryTotalBytes: 200}); err != nil {
			t.Fatalf("insert sample: %v", err)
		}
	}
	for _, step := range []string{"raw", "1m", "5m", "1h"} {
		t.Run(step, func(t *testing.T) {
			items, err := s.ListNodeMetricSeries(ctx, node.ID, "1h", step, now.Add(time.Minute))
			if err != nil {
				t.Fatalf("step %s: %v", step, err)
			}
			if len(items) == 0 {
				t.Fatalf("step %s returned no points", step)
			}
		})
	}
}

func TestInsertNodeMetricHistoryBatchUsesTransactionAndKeepsSamplesOnDetailError(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "metric-batch-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "batch.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	err = s.InsertNodeMetricHistoryBatch(ctx, []NodeMetricHistoryInput{
		{
			NodeID:    node.ID,
			SampledAt: now,
			Metrics: NodeReportMetricsInput{
				CPUPercent:       44,
				MemoryBytes:      100,
				MemoryTotalBytes: 200,
				Details:          json.RawMessage(`{"bad"`),
			},
		},
		{
			NodeID:    node.ID,
			SampledAt: now.Add(time.Second),
			Metrics: NodeReportMetricsInput{
				CPUPercent:       45,
				MemoryBytes:      120,
				MemoryTotalBytes: 240,
				Details:          json.RawMessage(`{"ok":true}`),
			},
		},
	})
	if err == nil {
		t.Fatalf("expected invalid detail error")
	}
	series, err := s.ListNodeMetricSeries(ctx, node.ID, "1h", "raw", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("metric series: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("batch should keep sample rows even when one detail fails, got %+v", series)
	}
	details, ok, err := s.GetLatestNodeMetricDetails(ctx, node.ID)
	if err != nil || !ok {
		t.Fatalf("latest valid details ok=%v err=%v", ok, err)
	}
	if string(details.Data) != `{"ok":true}` {
		t.Fatalf("unexpected latest details: %s", string(details.Data))
	}
}
