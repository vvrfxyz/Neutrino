package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"neutrino/internal/hostnet"
)

// TestSampleTransientNetReadFailureDoesNotSpikeBPS covers the
// success → transient failure → recovery sequence: the failed sample must
// carry over the previous totals and must not advance the BPS baseline, so
// the recovery sample computes BPS from the real counter delta instead of
// (realTotal-0)/dt.
func TestSampleTransientNetReadFailureDoesNotSpikeBPS(t *testing.T) {
	orig := readNetTotals
	t.Cleanup(func() { readNetTotals = orig })

	ctx := context.Background()
	m := NewHostMonitor(10, "")
	t0 := time.Now().UTC()

	// First sample: successful read establishes the baseline.
	readNetTotals = func(context.Context, string) (hostnet.NetTotals, string, error) {
		return hostnet.NetTotals{RX: 1_000_000, TX: 2_000_000}, "test", nil
	}
	snap, ok := m.sample(ctx, t0, nil, 0, 0, time.Time{}, false)
	if !ok {
		t.Fatalf("expected successful net read")
	}
	if snap.InboundTotal != 1_000_000 || snap.OutboundTotal != 2_000_000 {
		t.Fatalf("unexpected totals: %+v", snap)
	}
	if snap.InboundBPS != 0 || snap.OutboundBPS != 0 {
		t.Fatalf("first sample must not report BPS: %+v", snap)
	}

	// Second sample: transient read failure. Totals carry over, BPS stays 0,
	// and the caller must not advance the baseline (ok=false).
	readNetTotals = func(context.Context, string) (hostnet.NetTotals, string, error) {
		return hostnet.NetTotals{}, "", errors.New("transient read failure")
	}
	t1 := t0.Add(2 * time.Second)
	snap, ok = m.sample(ctx, t1, nil, 1_000_000, 2_000_000, t0, true)
	if ok {
		t.Fatalf("failed read must report ok=false")
	}
	if snap.InboundTotal != 1_000_000 || snap.OutboundTotal != 2_000_000 {
		t.Fatalf("failed read must carry over previous totals, got %+v", snap)
	}
	if snap.InboundBPS != 0 || snap.OutboundBPS != 0 {
		t.Fatalf("failed read must not report BPS, got %+v", snap)
	}

	// Third sample: recovery. Because the baseline was not reset to zero, the
	// BPS reflects the real counter delta over the full 4s window.
	readNetTotals = func(context.Context, string) (hostnet.NetTotals, string, error) {
		return hostnet.NetTotals{RX: 1_000_000 + 4_000, TX: 2_000_000 + 8_000}, "test", nil
	}
	t2 := t0.Add(4 * time.Second)
	snap, ok = m.sample(ctx, t2, nil, 1_000_000, 2_000_000, t0, true)
	if !ok {
		t.Fatalf("expected successful recovery read")
	}
	if snap.InboundBPS != 1_000 || snap.OutboundBPS != 2_000 {
		t.Fatalf("recovery BPS must reflect real delta, got in=%v out=%v", snap.InboundBPS, snap.OutboundBPS)
	}
}
