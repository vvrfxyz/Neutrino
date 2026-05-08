package agent

import "testing"

func TestConfigFromEnvLoadsRuntimeReportInterval(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_REPORT_SEC", "1")

	cfg := ConfigFromEnv()
	if cfg.RuntimeReportSec != 1 {
		t.Fatalf("RuntimeReportSec=%d, want 1", cfg.RuntimeReportSec)
	}
}

func TestConfigFromEnvDefaultsRuntimeReportIntervalToTwoSeconds(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_REPORT_SEC", "")

	cfg := ConfigFromEnv()
	if cfg.RuntimeReportSec != 2 {
		t.Fatalf("RuntimeReportSec=%d, want 2", cfg.RuntimeReportSec)
	}
}
