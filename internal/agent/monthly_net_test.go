package agent

import (
	"testing"
	"time"
)

func TestMonthLocationFallback(t *testing.T) {
	cfg := Config{MonthTZ: "Asia/Shanghai"}
	if loc := cfg.MonthLocation(); loc == nil || loc.String() != "Asia/Shanghai" {
		t.Fatalf("expected Asia/Shanghai, got %v", loc)
	}

	cfg = Config{MonthTZ: "Not/A/Real/Zone"}
	if loc := cfg.MonthLocation(); loc == nil {
		t.Fatalf("expected non-nil fallback location")
	}

	cfg = Config{}
	if loc := cfg.MonthLocation(); loc == nil {
		t.Fatalf("expected non-nil default location")
	}
}

func TestMonthMetadata(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	reportedAt := time.Date(2026, 3, 31, 18, 0, 0, 0, time.UTC)
	if got := localMonthKey(reportedAt, loc); got != "2026-04" {
		t.Fatalf("unexpected month key %q", got)
	}
	if got := localTimezoneName(reportedAt, loc); got != "Asia/Shanghai" {
		t.Fatalf("unexpected timezone name %q", got)
	}
}
