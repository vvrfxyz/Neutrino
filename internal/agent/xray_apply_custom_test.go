package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"neutrino/internal/xraycfg"
)

const customBaseTemplate = `{
  "outbounds": [
    {"protocol": "freedom", "tag": "direct"},
    {"protocol": "blackhole", "tag": "blocked"}
  ],
  "routing": {
    "rules": [
      {"type": "field", "inboundTag": ["api-in"], "outboundTag": "api"}
    ]
  }
}`

func TestExecXrayApplyMergesCustomConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	args, count := reloadCounter(t, dir)
	a := &Agent{xrayConfigPath: configPath, xrayReloadArgs: args}

	req := XrayApplyRequest{
		Template: customBaseTemplate + "\n" + xraycfg.Marker + "\n",
		CustomOutbounds: []xraycfg.Outbound{
			{Tag: "up", Protocol: "socks", Address: "10.0.0.2", Port: 1080},
		},
		CustomRoutes: []xraycfg.Route{
			{OutboundTag: "up", Domains: []string{"geosite:openai"}},
		},
	}
	res, err := a.execXrayApply(context.Background(), req)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("apply not ok: %+v", res)
	}
	if count() != 1 {
		t.Fatalf("reload count = %d, want 1", count())
	}

	b, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read installed config: %v", err)
	}
	if strings.Contains(string(b), xraycfg.Marker) {
		t.Fatalf("marker leaked into installed config")
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("installed config invalid: %v", err)
	}
	outs := cfg["outbounds"].([]any)
	if len(outs) != 3 {
		t.Fatalf("expected 3 outbounds, got %d", len(outs))
	}
	rules := cfg["routing"].(map[string]any)["rules"].([]any)
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}
	if tag := rules[1].(map[string]any)["outboundTag"]; tag != "up" {
		t.Fatalf("custom rule not merged after api rule: %v", tag)
	}

	// Idempotency: identical request must hit the unchanged-config skip — the
	// merged form, not the raw template, is what's compared against disk.
	res, err = a.execXrayApply(context.Background(), req)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if skipped, _ := res["skipped"].(bool); !skipped {
		t.Fatalf("identical custom apply should skip; res=%+v", res)
	}
	if count() != 1 {
		t.Fatalf("skip must not reload; count=%d", count())
	}
}

// The agent never trusts the panel: an invalid custom config in the payload
// is rejected before any backup/write/reload.
func TestExecXrayApplyRejectsInvalidCustomConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	args, count := reloadCounter(t, dir)
	a := &Agent{xrayConfigPath: configPath, xrayReloadArgs: args}

	_, err := a.execXrayApply(context.Background(), XrayApplyRequest{
		Template: customBaseTemplate + "\n" + xraycfg.Marker + "\n",
		CustomRoutes: []xraycfg.Route{
			{OutboundTag: "direct", Domains: []string{"ext:geosite.dat:cn"}},
		},
	})
	if err == nil {
		t.Fatalf("expected rejection")
	}
	je, ok := err.(JobError)
	if !ok || je.Retryable {
		t.Fatalf("want non-retryable JobError, got %v", err)
	}
	if !strings.Contains(je.Msg, "custom config rejected") {
		t.Fatalf("unexpected message: %s", je.Msg)
	}
	if count() != 0 {
		t.Fatalf("reload ran despite rejection")
	}
	if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
		t.Fatalf("config written despite rejection")
	}
}

// Old-agent contract: a marker-bearing template without custom-config support
// must fail JSON validation, never silently apply with the customs dropped.
// This test pins the marker's invalid-JSON property at the payload level.
func TestMarkerTemplateIsInvalidJSONWithoutStripping(t *testing.T) {
	tpl := customBaseTemplate + "\n" + xraycfg.Marker + "\n"
	if json.Valid([]byte(tpl)) {
		t.Fatalf("marker template is valid json; old agents would apply it silently")
	}
}
