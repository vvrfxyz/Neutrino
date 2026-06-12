package functional_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Module 4: structured custom outbounds/routes — preview endpoint, save-time
// validation, and the capability marker + customs landing in the job payload.
func TestFunctional_ManagedXrayCustomConfigPreviewAndSave(t *testing.T) {
	env := setupTestEnv(t)

	name := fmt.Sprintf("custom-cfg-%d", time.Now().UnixNano())
	resp := env.request(t, http.MethodPost, "/api/v1/nodes", map[string]any{
		"name": name, "core_type": "xray", "protocol": "vless_reality",
		"host": "example.com", "port": 443, "enabled": true, "managed": true,
	}, true, "")
	mustStatus(t, resp, http.StatusCreated)
	created := decodeBodyMap(t, resp)
	nodeID := int64(created["id"].(float64))
	if nodeID <= 0 {
		t.Fatalf("invalid node id: %v", created)
	}

	extra := `{"xray":{"rollback_on_fail":true,` +
		`"custom_outbounds":[{"tag":"up","protocol":"socks","address":"10.0.0.2","port":1080}],` +
		`"custom_routes":[{"outbound_tag":"up","domains":["geosite:openai"]},{"outbound_tag":"blocked","protocols":["bittorrent"]}]}}`

	// Preview against a draft that has not been saved.
	resp = env.request(t, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/managed/xray/preview", nodeID),
		map[string]any{"extra_json": extra}, true, "")
	mustStatus(t, resp, http.StatusOK)
	prev := decodeBodyMap(t, resp)
	previewText, _ := prev["preview"].(string)
	for _, want := range []string{`"geosite:openai"`, `"tag": "up"`, `"bittorrent"`, "${XRAY_REALITY_PRIVATE_KEY}"} {
		if !strings.Contains(previewText, want) {
			t.Fatalf("preview missing %q:\n%s", want, previewText)
		}
	}
	if strings.Contains(previewText, "neutrino:requires") {
		t.Fatalf("capability marker leaked into preview")
	}

	// Invalid custom config: preview rejects with 400.
	bad := `{"xray":{"custom_routes":[{"outbound_tag":"ghost","ports":"443"}]}}`
	resp = env.request(t, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/managed/xray/preview", nodeID),
		map[string]any{"extra_json": bad}, true, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid preview status=%d, want 400", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// Invalid custom config: saving via node update is rejected too.
	resp = env.request(t, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", nodeID), map[string]any{
		"name": name, "core_type": "xray", "protocol": "vless_reality",
		"host": "example.com", "port": 443, "enabled": true, "managed": true,
		"extra_json": bad,
	}, true, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid save status=%d, want 400", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// Valid save updates desired state; the xray_apply payload must carry the
	// customs and the old-agent capability marker.
	resp = env.request(t, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", nodeID), map[string]any{
		"name": name, "core_type": "xray", "protocol": "vless_reality",
		"host": "example.com", "port": 443, "enabled": true, "managed": true,
		"extra_json": extra,
	}, true, "")
	mustStatus(t, resp, http.StatusOK)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	var payloadJSON string
	if err := env.store.RawDB().QueryRowContext(context.Background(),
		`SELECT payload_json FROM node_jobs WHERE node_id=? AND kind='xray_apply' ORDER BY id DESC LIMIT 1`,
		nodeID).Scan(&payloadJSON); err != nil {
		t.Fatalf("read xray_apply payload: %v", err)
	}
	for _, want := range []string{`"custom_outbounds"`, `"custom_routes"`, "neutrino:requires=custom-config-v1"} {
		if !strings.Contains(payloadJSON, want) {
			t.Fatalf("xray_apply payload missing %q:\n%s", want, payloadJSON)
		}
	}
}
