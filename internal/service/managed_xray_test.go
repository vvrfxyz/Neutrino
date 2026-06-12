package service

import (
	"encoding/json"
	"strings"
	"testing"

	"neutrino/internal/repo"
	"neutrino/internal/xraycfg"
)

func TestBuildManagedXrayApplyPayloadWithCustoms(t *testing.T) {
	node := repo.Node{
		ID: 1, Managed: true, CoreType: "xray",
		ExtraJSON: `{"xray":{"rollback_on_fail":true,"custom_outbounds":[{"tag":"up","protocol":"socks","address":"10.0.0.2","port":1080}],"custom_routes":[{"outbound_tag":"up","domains":["geosite:openai"]}]}}`,
	}
	payload, err := BuildManagedXrayApplyPayload(node)
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}
	var p XrayApplyPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatalf("payload not json: %v", err)
	}
	if len(p.CustomOutbounds) != 1 || p.CustomOutbounds[0].Tag != "up" {
		t.Fatalf("custom outbounds missing from payload: %+v", p.CustomOutbounds)
	}
	if len(p.CustomRoutes) != 1 || p.CustomRoutes[0].OutboundTag != "up" {
		t.Fatalf("custom routes missing from payload: %+v", p.CustomRoutes)
	}
	if !strings.Contains(p.Template, xraycfg.Marker) {
		t.Fatalf("template missing capability marker")
	}
}

func TestBuildManagedXrayApplyPayloadNoCustomsHasNoMarker(t *testing.T) {
	node := repo.Node{ID: 1, Managed: true, CoreType: "xray", ExtraJSON: `{"xray":{"rollback_on_fail":true}}`}
	payload, err := BuildManagedXrayApplyPayload(node)
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}
	if strings.Contains(payload, xraycfg.Marker) {
		t.Fatalf("marker added without customs — would break old agents needlessly")
	}
}

func TestParseNodeExtraRejectsInvalidCustoms(t *testing.T) {
	_, err := ParseNodeExtra(`{"xray":{"custom_routes":[{"outbound_tag":"ghost","ports":"443"}]}}`)
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected unknown-target rejection, got %v", err)
	}
	_, err = ParseNodeExtra(`{"xray":{"custom_outbounds":[{"tag":"direct","protocol":"freedom"}]}}`)
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected reserved-tag rejection, got %v", err)
	}
}

func TestPreviewManagedXrayConfig(t *testing.T) {
	node := repo.Node{
		ID: 1, Managed: true, CoreType: "xray",
		ExtraJSON: `{"xray":{"custom_outbounds":[{"tag":"up","protocol":"http","address":"proxy.internal","port":8080}],"custom_routes":[{"outbound_tag":"up","ports":"8443"}]}}`,
	}
	preview, err := PreviewManagedXrayConfig(node)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(preview, &cfg); err != nil {
		t.Fatalf("preview not valid json: %v", err)
	}
	if strings.Contains(string(preview), xraycfg.Marker) {
		t.Fatalf("marker leaked into preview")
	}
	// Placeholder vars render as ${VAR} markers, never resolved values —
	// the panel must not guess at node-local secrets.
	if !strings.Contains(string(preview), "${XRAY_REALITY_PRIVATE_KEY}") {
		t.Fatalf("preview should keep unresolved placeholders visible")
	}
	found := false
	for _, o := range cfg["outbounds"].([]any) {
		if m, ok := o.(map[string]any); ok && m["tag"] == "up" {
			found = true
		}
	}
	if !found {
		t.Fatalf("custom outbound missing from preview")
	}
}

func TestPreviewManagedXrayConfigVarsApplied(t *testing.T) {
	node := repo.Node{
		ID: 1, Managed: true, CoreType: "xray",
		ExtraJSON: `{"xray":{"vars":{"XRAY_VLESS_PORT":"8443"}}}`,
	}
	preview, err := PreviewManagedXrayConfig(node)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if !strings.Contains(string(preview), `"port": 8443`) {
		t.Fatalf("panel-set var not rendered into preview:\n%s", preview)
	}
}
