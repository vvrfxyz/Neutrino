package repo

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"neutrino/internal/config"
	"neutrino/internal/db"
)

func newHostNetStore(t *testing.T, tz string) *Store {
	t.Helper()
	tmp := t.TempDir()
	conn, err := db.Open(filepath.Join(tmp, "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg := config.Load()
	cfg.PanelTZ = tz
	return New(conn, cfg)
}

func TestRecordHostNetTotals_MonthlyAccumulateAndReset(t *testing.T) {
	s := newHostNetStore(t, "Asia/Shanghai")
	loc := s.cfg.PanelLocation()

	ctx := context.Background()
	at := time.Date(2026, 2, 7, 10, 0, 0, 0, loc)

	// First sample sets baseline, does not accumulate.
	if err := s.RecordHostNetTotals(ctx, 1000, 2000, "gopsutil", at); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	u, ok, err := s.GetHostNetMonthlyUsage(ctx, at)
	if err != nil || !ok {
		t.Fatalf("get month: ok=%v err=%v", ok, err)
	}
	if u.RXBytes != 0 || u.TXBytes != 0 {
		t.Fatalf("expected 0/0 baseline got rx=%d tx=%d", u.RXBytes, u.TXBytes)
	}

	// Second sample accumulates delta.
	if err := s.RecordHostNetTotals(ctx, 1600, 2600, "gopsutil", at.Add(10*time.Second)); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	u, _, _ = s.GetHostNetMonthlyUsage(ctx, at)
	if u.RXBytes != 600 || u.TXBytes != 600 {
		t.Fatalf("expected 600/600 got rx=%d tx=%d", u.RXBytes, u.TXBytes)
	}

	// Counter reset (reboot): negative deltas should not decrease accumulated values.
	if err := s.RecordHostNetTotals(ctx, 100, 100, "gopsutil", at.Add(20*time.Second)); err != nil {
		t.Fatalf("record reset: %v", err)
	}
	u, _, _ = s.GetHostNetMonthlyUsage(ctx, at)
	if u.RXBytes != 600 || u.TXBytes != 600 {
		t.Fatalf("expected unchanged after reset got rx=%d tx=%d", u.RXBytes, u.TXBytes)
	}
	if err := s.RecordHostNetTotals(ctx, 200, 350, "gopsutil", at.Add(30*time.Second)); err != nil {
		t.Fatalf("record after reset: %v", err)
	}
	u, _, _ = s.GetHostNetMonthlyUsage(ctx, at)
	if u.RXBytes != 700 || u.TXBytes != 850 {
		t.Fatalf("expected +100/+250 got rx=%d tx=%d", u.RXBytes, u.TXBytes)
	}
}

func TestRecordHostNetTotals_MonthRolloverBaselinesNewMonth(t *testing.T) {
	s := newHostNetStore(t, "UTC")
	loc := s.cfg.PanelLocation()

	ctx := context.Background()
	endOfMonth := time.Date(2026, 1, 31, 23, 59, 50, 0, loc)
	startNext := time.Date(2026, 2, 1, 0, 0, 10, 0, loc)

	_ = s.RecordHostNetTotals(ctx, 1000, 1000, "gopsutil", endOfMonth)
	_ = s.RecordHostNetTotals(ctx, 1500, 1600, "gopsutil", endOfMonth.Add(5*time.Second))
	jan, ok, _ := s.GetHostNetMonthlyUsage(ctx, endOfMonth)
	if !ok {
		t.Fatalf("missing jan usage")
	}
	if jan.RXBytes != 500 || jan.TXBytes != 600 {
		t.Fatalf("unexpected jan rx/tx: %d/%d", jan.RXBytes, jan.TXBytes)
	}

	// Month rollover should baseline without carrying last delta.
	_ = s.RecordHostNetTotals(ctx, 2000, 2200, "gopsutil", startNext)
	feb, ok, _ := s.GetHostNetMonthlyUsage(ctx, startNext)
	if !ok {
		t.Fatalf("missing feb usage")
	}
	if feb.RXBytes != 0 || feb.TXBytes != 0 {
		t.Fatalf("expected feb baseline 0/0 got %d/%d", feb.RXBytes, feb.TXBytes)
	}
}

func TestRecordHostNetTotals_RebasesOnSourceChange(t *testing.T) {
	s := newHostNetStore(t, "UTC")
	loc := s.cfg.PanelLocation()

	ctx := context.Background()
	at := time.Date(2026, 4, 9, 10, 0, 0, 0, loc)

	if err := s.RecordHostNetTotals(ctx, 1000, 2000, "gopsutil", at); err != nil {
		t.Fatalf("record baseline: %v", err)
	}
	if err := s.RecordHostNetTotals(ctx, 1600, 2600, "gopsutil", at.Add(10*time.Second)); err != nil {
		t.Fatalf("record delta: %v", err)
	}

	u, ok, err := s.GetHostNetMonthlyUsage(ctx, at)
	if err != nil || !ok {
		t.Fatalf("get usage before switch ok=%v err=%v", ok, err)
	}
	if u.RXBytes != 600 || u.TXBytes != 600 {
		t.Fatalf("expected pre-switch 600/600 got %d/%d", u.RXBytes, u.TXBytes)
	}

	if err := s.RecordHostNetTotals(ctx, 500000, 800000, "proc:/host/proc", at.Add(20*time.Second)); err != nil {
		t.Fatalf("record source switch: %v", err)
	}
	u, ok, err = s.GetHostNetMonthlyUsage(ctx, at)
	if err != nil || !ok {
		t.Fatalf("get usage after switch ok=%v err=%v", ok, err)
	}
	if u.RXBytes != 600 || u.TXBytes != 600 {
		t.Fatalf("source switch should preserve month totals got %d/%d", u.RXBytes, u.TXBytes)
	}

	if err := s.RecordHostNetTotals(ctx, 500100, 800250, "proc:/host/proc", at.Add(30*time.Second)); err != nil {
		t.Fatalf("record after source switch: %v", err)
	}
	u, ok, err = s.GetHostNetMonthlyUsage(ctx, at)
	if err != nil || !ok {
		t.Fatalf("get usage after delta ok=%v err=%v", ok, err)
	}
	if u.RXBytes != 700 || u.TXBytes != 850 {
		t.Fatalf("expected post-switch 700/850 got %d/%d", u.RXBytes, u.TXBytes)
	}
}

func TestRecordHostNetTotals_DefaultsToUTCWhenUnset(t *testing.T) {
	// Explicitly leave PanelTZ empty so PanelLocation falls back to UTC.
	tmp := t.TempDir()
	conn, err := db.Open(filepath.Join(tmp, "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg := config.Load()
	cfg.PanelTZ = ""
	s := New(conn, cfg)

	if got := s.cfg.PanelLocation(); got != time.UTC {
		t.Fatalf("expected UTC with empty PanelTZ, got %v", got)
	}

	ctx := context.Background()
	// 2026-03-31T23:30:00 UTC. In Asia/Shanghai this would already be April 2026,
	// but with empty PanelTZ the month key must be the UTC one.
	at := time.Date(2026, 3, 31, 23, 30, 0, 0, time.UTC)
	if err := s.RecordHostNetTotals(ctx, 100, 100, "gopsutil", at); err != nil {
		t.Fatalf("record baseline: %v", err)
	}
	u, ok, err := s.GetHostNetMonthlyUsage(ctx, at)
	if err != nil || !ok {
		t.Fatalf("get usage ok=%v err=%v", ok, err)
	}
	if u.MonthKey != "2026-03" {
		t.Fatalf("expected UTC month key 2026-03 got %q", u.MonthKey)
	}
}
