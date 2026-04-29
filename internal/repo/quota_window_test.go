package repo

import (
	"testing"
	"time"
)

func TestQuotaWindowBounds_DSTDayLengths(t *testing.T) {
	// America/New_York DST start in 2026: 2026-03-08 (23-hour day)
	{
		at := time.Date(2026, 3, 8, 15, 0, 0, 0, time.UTC) // within the local day
		start, end, key := quotaWindowBounds("day", "America/New_York", at)
		if got := end.Sub(start); got != 23*time.Hour {
			t.Fatalf("dst start day expected 23h window, got %v (start=%s end=%s)", got, start.Format(time.RFC3339), end.Format(time.RFC3339))
		}
		if key != "2026-03-08" {
			t.Fatalf("unexpected cycle key: %q", key)
		}
	}

	// America/New_York DST end in 2026: 2026-11-01 (25-hour day)
	{
		at := time.Date(2026, 11, 1, 15, 0, 0, 0, time.UTC)
		start, end, key := quotaWindowBounds("day", "America/New_York", at)
		if got := end.Sub(start); got != 25*time.Hour {
			t.Fatalf("dst end day expected 25h window, got %v (start=%s end=%s)", got, start.Format(time.RFC3339), end.Format(time.RFC3339))
		}
		if key != "2026-11-01" {
			t.Fatalf("unexpected cycle key: %q", key)
		}
	}
}
