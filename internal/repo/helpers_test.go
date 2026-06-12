package repo

import (
	"testing"
	"time"
)

func TestTrafficBucketKey(t *testing.T) {
	at := time.Date(2026, time.June, 11, 13, 45, 9, 0, time.UTC)

	cases := []struct {
		period string
		want   string
	}{
		{"hourly", "2026-06-11T13:00:00Z"},
		{"daily", "2026-06-11T00:00:00Z"},
		// Regression: the old "2006-01-01" layout rendered the month twice
		// ("2026-06-06"), so mid-month timestamps never keyed to month start.
		{"monthly", "2026-06-01T00:00:00Z"},
	}
	for _, c := range cases {
		if got := trafficBucketKey(at, c.period); got != c.want {
			t.Errorf("trafficBucketKey(%s) = %q, want %q", c.period, got, c.want)
		}
	}

	// Series iteration starts at month boundaries; both sides must agree.
	monthStart := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	if got, want := trafficBucketKey(monthStart, "monthly"), trafficBucketKey(at, "monthly"); got != want {
		t.Errorf("month-start and mid-month keys diverge: %q vs %q", got, want)
	}
}
