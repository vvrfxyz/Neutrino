package functional_test

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Module 1 finish: flag= synonym, mihomo alias, types=/filter= query filters.
func TestFunctional_SubscriptionQueryParamsAndAliases(t *testing.T) {
	env := setupTestEnv(t)
	token := setupSubTestUser(t, env)
	// Two renderable nodes (pbk/sid present) with distinct names for filtering.
	mkNode := func(name string) {
		t.Helper()
		resp := env.request(t, http.MethodPost, "/api/v1/nodes", map[string]any{
			"name": name, "core_type": "xray", "protocol": "vless_reality",
			"host": "example.com", "port": 443, "enabled": true,
			"public_key": "test-pbk", "short_id": "test-sid",
		}, true, "")
		mustStatus(t, resp, http.StatusCreated)
		_ = decodeBodyMap(t, resp)
	}
	mkNode(fmt.Sprintf("hk-filter-node-%d", time.Now().UnixNano()))
	mkNode(fmt.Sprintf("jp-other-node-%d", time.Now().UnixNano()))

	readBody := func(resp *http.Response) string {
		t.Helper()
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return string(b)
	}

	// flag= behaves as target=; mihomo aliases to clash yaml.
	resp := env.fetchSubscription(t, "/sub/"+token+"?flag=mihomo", "")
	mustStatus(t, resp, http.StatusOK)
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Fatalf("flag=mihomo content-type = %q, want yaml", ct)
	}
	if body := readBody(resp); !strings.Contains(body, "proxies:") {
		t.Fatalf("flag=mihomo did not render clash yaml")
	}

	// target wins over flag when both are present.
	resp = env.fetchSubscription(t, "/sub/"+token+"?target=v2rayn&flag=mihomo", "")
	mustStatus(t, resp, http.StatusOK)
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "yaml") {
		t.Fatalf("target= should win over flag=, got %q", ct)
	}
	_ = readBody(resp)

	// filter= narrows by node name keyword.
	resp = env.fetchSubscription(t, "/sub/"+token+"?target=v2rayn&filter=hk-filter", "")
	mustStatus(t, resp, http.StatusOK)
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(readBody(resp)))
	if err != nil {
		t.Fatalf("decode v2rayn payload: %v", err)
	}
	if !strings.Contains(string(decoded), "hk-filter") || strings.Contains(string(decoded), "jp-other") {
		t.Fatalf("filter= selection wrong:\n%s", decoded)
	}

	// types= keeps only matching protocols; the test nodes are vless_reality,
	// so asking for hysteria2 must 400 (filtered-empty), not fall back.
	resp = env.fetchSubscription(t, "/sub/"+token+"?target=v2rayn&types=hy2", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("types=hy2 with no hy2 nodes: status=%d, want 400", resp.StatusCode)
	}
	_ = readBody(resp)

	// Unknown types value is rejected.
	resp = env.fetchSubscription(t, "/sub/"+token+"?target=v2rayn&types=wireguard", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("types=wireguard: status=%d, want 400", resp.StatusCode)
	}
	_ = readBody(resp)
}
