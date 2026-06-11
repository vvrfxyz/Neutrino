package repo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestInsertNodeMetricHistoryBatchTypedCommitError(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "metric-batch-node",
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

	// All-good batch: nil error.
	if err := s.InsertNodeMetricHistoryBatch(ctx, []NodeMetricHistoryInput{
		{NodeID: node.ID, SampledAt: now, Metrics: NodeReportMetricsInput{CPUPercent: 10}},
	}); err != nil {
		t.Fatalf("clean batch: %v", err)
	}

	// Batch with one bad item (invalid node id → per-item error) and one good
	// item: tx commits, error must carry the typed sentinel.
	err = s.InsertNodeMetricHistoryBatch(ctx, []NodeMetricHistoryInput{
		{NodeID: 0, SampledAt: now, Metrics: NodeReportMetricsInput{CPUPercent: 10}},
		{NodeID: node.ID, SampledAt: now.Add(time.Second), Metrics: NodeReportMetricsInput{CPUPercent: 20}},
	})
	if err == nil {
		t.Fatalf("expected per-item error")
	}
	if !errors.Is(err, ErrBatchCommittedWithItemErrors) {
		t.Fatalf("expected ErrBatchCommittedWithItemErrors, got %v", err)
	}

	// The good item must have been committed despite the sibling failure.
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_metric_samples WHERE node_id = ?`, node.ID).Scan(&count); err != nil {
		t.Fatalf("count samples: %v", err)
	}
	if count != 2 {
		t.Fatalf("samples = %d, want 2 (clean batch + good item of mixed batch)", count)
	}

	// Details failure also commits and is typed (sample ok, details invalid).
	err = s.InsertNodeMetricHistoryBatch(ctx, []NodeMetricHistoryInput{
		{NodeID: node.ID, SampledAt: now.Add(2 * time.Second), Metrics: NodeReportMetricsInput{CPUPercent: 30, Details: []byte("{not json")}},
	})
	if err == nil {
		t.Fatalf("expected details error")
	}
	if !errors.Is(err, ErrBatchCommittedWithItemErrors) {
		t.Fatalf("details failure: expected ErrBatchCommittedWithItemErrors, got %v", err)
	}
}

func TestGetNodeReturnsSentinelNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_, err := s.GetNode(ctx, 99999)
	if !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("expected ErrNodeNotFound, got %v", err)
	}
}

func TestEnrollCodeErrorsCarrySentinel(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "enroll-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	// No code issued at all → invalid.
	if err := s.ValidateNodeEnrollCode(ctx, node.ID, "bogus"); !errors.Is(err, ErrEnrollCodeInvalid) {
		t.Fatalf("expected ErrEnrollCodeInvalid, got %v", err)
	}
	// Empty code → invalid.
	if err := s.ValidateNodeEnrollCode(ctx, node.ID, ""); !errors.Is(err, ErrEnrollCodeInvalid) {
		t.Fatalf("empty code: expected ErrEnrollCodeInvalid, got %v", err)
	}

	// Issued code → the exact code validates, a same-length wrong code does not.
	issued, err := s.CreateNodeEnrollCode(ctx, node.ID, time.Minute)
	if err != nil {
		t.Fatalf("create enroll code: %v", err)
	}
	if err := s.ValidateNodeEnrollCode(ctx, node.ID, issued.Code); err != nil {
		t.Fatalf("correct code should validate, got %v", err)
	}
	wrong := strings.Repeat("x", len(issued.Code))
	if err := s.ValidateNodeEnrollCode(ctx, node.ID, wrong); !errors.Is(err, ErrEnrollCodeInvalid) {
		t.Fatalf("wrong code: expected ErrEnrollCodeInvalid, got %v", err)
	}
}
