package config

import "testing"

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
