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

func TestConfigFromEnvDefaultsHealthEndpointToLoopback(t *testing.T) {
	t.Setenv("AGENT_HTTP_ADDR", "")

	cfg := ConfigFromEnv()
	if cfg.AgentHTTPAddr != "127.0.0.1:9090" {
		t.Fatalf("AgentHTTPAddr=%q, want loopback default", cfg.AgentHTTPAddr)
	}
}
