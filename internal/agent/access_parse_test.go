package agent

import (
	"testing"
	"time"
)

func loadLocForTest(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	if loc == nil {
		t.Fatalf("LoadLocation(%q) returned nil", name)
	}
	return loc
}

func TestExtractEventTime_ParsesUsingProvidedLocation_Shanghai(t *testing.T) {
	loc := loadLocForTest(t, "Asia/Shanghai")
	got := extractEventTime("2026/03/08 02:30:00 something", loc)
	want := time.Date(2026, 3, 7, 18, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("extractEventTime Asia/Shanghai: got %s want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestExtractEventTime_FallbackOnBadLineReturnsNonZero(t *testing.T) {
	loc := loadLocForTest(t, "UTC")
	before := time.Now().UTC().Add(-time.Second)
	got := extractEventTime("bad", loc)
	if got.IsZero() {
		t.Fatalf("expected non-zero fallback")
	}
	if !got.After(before) {
		t.Fatalf("expected fallback to be near now, got %s (before=%s)", got.Format(time.RFC3339), before.Format(time.RFC3339))
	}
}

func TestExtractEventTime_DSTSpringForward_NewYork(t *testing.T) {
	// On 2026-03-08 in America/New_York, DST jumps from 02:00 EST (UTC-5) to 03:00 EDT (UTC-4).
	// A log line at "02:30:00" is ambiguous but must not panic, and must produce a sensible UTC
	// moment somewhere in the 2026-03-08 window.
	loc := loadLocForTest(t, "America/New_York")
	got := extractEventTime("2026/03/08 02:30:00 evt", loc)
	if got.IsZero() {
		t.Fatalf("expected non-zero UTC time for DST-spring log line")
	}
	if got.Before(time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC)) ||
		got.After(time.Date(2026, 3, 8, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("DST-spring parse produced implausible UTC time: %s", got.Format(time.RFC3339))
	}
}

func TestExtractEventTime_NilLocationDefaultsToLocal(t *testing.T) {
	// Should not panic when loc is nil.
	got := extractEventTime("2026/04/17 12:00:00 evt", nil)
	if got.IsZero() {
		t.Fatalf("expected non-zero fallback for nil loc")
	}
}

func TestParseAccessLineExtractsBracketedIPv6ClientIP(t *testing.T) {
	line := "2026/04/25 12:00:00 from [2001:db8::1]:54321 accepted tcp:example.com:443 [vless >> proxy] email: user@example.com"
	evt, ok := parseAccessLine("/tmp/access.log", 123, 0, line, map[string]int64{"user@example.com": 7}, 9, time.UTC)
	if !ok {
		t.Fatalf("expected access line to parse")
	}
	if evt.ClientIP != "2001:db8::1" {
		t.Fatalf("client_ip=%q, want IPv6 address", evt.ClientIP)
	}
}

func TestCompactAccessOnlineEventPreservesDestinationMetadata(t *testing.T) {
	evt := UsageEvent{
		UserID:      7,
		TargetHost:  "example.com",
		TargetPort:  443,
		SNI:         "example.com",
		Destination: "tcp:example.com:443",
		ClientIP:    "203.0.113.10",
		InboundTag:  "vless",
		At:          "2026-04-25T12:34:56Z",
	}
	got, _, ok := compactAccessOnlineEvent(evt, 9)
	if !ok {
		t.Fatalf("expected compact online event")
	}
	if got.TargetHost != evt.TargetHost || got.TargetPort != evt.TargetPort || got.SNI != evt.SNI || got.Destination != evt.Destination {
		t.Fatalf("metadata not preserved: got %+v want target host=%q port=%d sni=%q destination=%q", got, evt.TargetHost, evt.TargetPort, evt.SNI, evt.Destination)
	}
}
