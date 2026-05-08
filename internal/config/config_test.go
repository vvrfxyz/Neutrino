package config

import (
	"testing"
	"time"
)

func TestLoadOnlineDisplayWindowDefaultsToAtLeastFiveMinutes(t *testing.T) {
	t.Setenv("ONLINE_WINDOW_SEC", "120")
	t.Setenv("ONLINE_DISPLAY_WINDOW_SEC", "")

	cfg := Load()
	if cfg.OnlineWindowSec != 120 {
		t.Fatalf("OnlineWindowSec=%d, want 120", cfg.OnlineWindowSec)
	}
	if cfg.OnlineDisplayWindowSec != 300 {
		t.Fatalf("OnlineDisplayWindowSec=%d, want 300", cfg.OnlineDisplayWindowSec)
	}
}

func TestLoadOnlineDisplayWindowHonorsExplicitOverride(t *testing.T) {
	t.Setenv("ONLINE_WINDOW_SEC", "120")
	t.Setenv("ONLINE_DISPLAY_WINDOW_SEC", "180")

	cfg := Load()
	if cfg.OnlineDisplayWindowSec != 180 {
		t.Fatalf("OnlineDisplayWindowSec=%d, want 180", cfg.OnlineDisplayWindowSec)
	}
}

func TestOpsAndHostMetricIntervalsDefaultToTwoSeconds(t *testing.T) {
	t.Setenv("OPS_SNAPSHOT_INTERVAL_SEC", "")
	t.Setenv("HOST_METRICS_INTERVAL_SEC", "")

	cfg := Load()
	if got := cfg.OpsSnapshotInterval(); got != 2*time.Second {
		t.Fatalf("OpsSnapshotInterval=%v, want 2s", got)
	}
	if got := cfg.HostMetricsInterval(); got != 2*time.Second {
		t.Fatalf("HostMetricsInterval=%v, want 2s", got)
	}
}

func TestOpsAndHostMetricIntervalsAllowOneSecondMinimum(t *testing.T) {
	t.Setenv("OPS_SNAPSHOT_INTERVAL_SEC", "0")
	t.Setenv("HOST_METRICS_INTERVAL_SEC", "-10")

	cfg := Load()
	if got := cfg.OpsSnapshotInterval(); got != 2*time.Second {
		t.Fatalf("invalid OpsSnapshotInterval=%v, want fallback 2s", got)
	}
	if got := cfg.HostMetricsInterval(); got != 2*time.Second {
		t.Fatalf("invalid HostMetricsInterval=%v, want fallback 2s", got)
	}

	t.Setenv("OPS_SNAPSHOT_INTERVAL_SEC", "1")
	t.Setenv("HOST_METRICS_INTERVAL_SEC", "1")
	cfg = Load()
	if got := cfg.OpsSnapshotInterval(); got != time.Second {
		t.Fatalf("OpsSnapshotInterval=%v, want 1s", got)
	}
	if got := cfg.HostMetricsInterval(); got != time.Second {
		t.Fatalf("HostMetricsInterval=%v, want 1s", got)
	}
}
