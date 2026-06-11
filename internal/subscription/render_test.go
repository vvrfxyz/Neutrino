package subscription

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"neutrino/internal/repo"
)

func TestRenderTargets(t *testing.T) {
	u := repo.User{ID: 1, Username: "alice", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour)}
	link := &repo.ProxyLink{
		UUID: "11111111-1111-1111-1111-111111111111",
		Link: "vless://11111111-1111-1111-1111-111111111111@example.com:443?encryption=none&type=tcp&security=reality&flow=xtls-rprx-vision&sni=example.com&fp=chrome&pbk=abc&sid=12#alice",
	}
	nodes := []repo.Node{{
		ID:       1,
		Name:     "n1",
		Protocol: "vless_reality",
		CoreType: "xray",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	}}

	cases := []string{"v2rayn", "shadowrocket", "clash", "singbox"}
	for _, target := range cases {
		out, _, err := Render(target, u, link, nodes)
		if err != nil {
			t.Fatalf("target=%s err=%v", target, err)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatalf("target=%s empty output", target)
		}
	}
}

func TestRenderRequiresActiveLink(t *testing.T) {
	u := repo.User{ID: 1, Username: "alice", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour)}
	nodes := []repo.Node{{
		ID:       1,
		Name:     "n1",
		Protocol: "vless_reality",
		CoreType: "xray",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	}}

	_, _, err := Render("shadowrocket", u, nil, nodes)
	if err == nil {
		t.Fatalf("expected error when active link missing")
	}
}

func TestRenderWithOptionsDisablesActiveLinkFallback(t *testing.T) {
	u := repo.User{ID: 1, Username: "alice", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour)}
	link := &repo.ProxyLink{
		UUID: "11111111-1111-1111-1111-111111111111",
		Link: "vless://11111111-1111-1111-1111-111111111111@fallback.example.com:443?encryption=none&type=tcp&security=reality&flow=xtls-rprx-vision&sni=example.com&fp=chrome&pbk=abc&sid=12#alice",
	}
	nodes := []repo.Node{{
		ID:       1,
		Name:     "n1",
		Protocol: "vless_reality",
		CoreType: "xray",
		Host:     "permitted.example.com",
		Port:     443,
		Enabled:  true,
		// Missing REALITY public key / short id, so the permitted node is not renderable yet.
	}}

	_, _, err := RenderWithOptions("shadowrocket", u, link, nodes, RenderOptions{AllowActiveLinkFallback: false})
	if err == nil {
		t.Fatalf("expected no available nodes when fallback is disabled")
	}
}

func TestRenderDedupIdenticalURIs(t *testing.T) {
	u := repo.User{ID: 1, Username: "alice", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour)}
	link := &repo.ProxyLink{
		UUID: "11111111-1111-1111-1111-111111111111",
		// Match renderNodeURI exactly: name is "n1-alice".
		Link: "vless://11111111-1111-1111-1111-111111111111@example.com:443?encryption=none&type=tcp&security=reality&flow=xtls-rprx-vision&sni=example.com&fp=chrome&pbk=abc&sid=12#n1-alice",
	}
	nodes := []repo.Node{{
		ID:        1,
		Name:      "n1",
		Protocol:  "vless_reality",
		CoreType:  "xray",
		Host:      "example.com",
		Port:      443,
		Enabled:   true,
		Transport: "tcp",
		Security:  "reality",
		Flow:      "xtls-rprx-vision",
		SNI:       "example.com",
		FP:        "chrome",
		PublicKey: "abc",
		ShortID:   "12",
	}}

	out, _, err := Render("shadowrocket", u, link, nodes)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 uri after dedup, got %d: %q", len(lines), out)
	}
	got := strings.TrimSpace(lines[0])
	if canonicalProxyURIKey(got) != canonicalProxyURIKey(link.Link) {
		t.Fatalf("unexpected uri: got=%q want=%q", got, link.Link)
	}
}

func TestRenderOmitsActiveLinkWhenNodeURIsExist(t *testing.T) {
	u := repo.User{ID: 1, Username: "alice", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour)}
	link := &repo.ProxyLink{
		UUID: "11111111-1111-1111-1111-111111111111",
		// Different label fragment than renderNodeURI ("n1-alice"), should be omitted when node URIs exist.
		Link: "vless://11111111-1111-1111-1111-111111111111@example.com:443?encryption=none&type=tcp&security=reality&flow=xtls-rprx-vision&sni=example.com&fp=chrome&pbk=abc&sid=12#alice",
	}
	nodes := []repo.Node{{
		ID:        1,
		Name:      "n1",
		Protocol:  "vless_reality",
		CoreType:  "xray",
		Host:      "example.com",
		Port:      443,
		Enabled:   true,
		Transport: "tcp",
		Security:  "reality",
		Flow:      "xtls-rprx-vision",
		SNI:       "example.com",
		FP:        "chrome",
		PublicKey: "abc",
		ShortID:   "12",
	}}

	out, _, err := Render("shadowrocket", u, link, nodes)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 uri when node uris exist, got %d: %q", len(lines), out)
	}
	if strings.Contains(lines[0], "#alice") {
		t.Fatalf("unexpected active link fragment in output: %q", lines[0])
	}
	if !strings.Contains(lines[0], "#n1-alice") {
		t.Fatalf("expected node fragment in output: %q", lines[0])
	}
}

func TestRenderClashUsesActualProxyNamesInGroup(t *testing.T) {
	out := renderClashYAML([]string{
		"vless://11111111-1111-1111-1111-111111111111@example.com:443?encryption=none&type=tcp&security=reality&flow=xtls-rprx-vision&sni=example.com&fp=chrome&pbk=abc&sid=12#edge-a",
		"vless://22222222-2222-2222-2222-222222222222@example.org:443?encryption=none&type=tcp&security=reality&flow=xtls-rprx-vision&sni=example.org&fp=chrome&pbk=def&sid=34#edge-b",
	})
	if !strings.Contains(out, "      - 'edge-a'") || !strings.Contains(out, "      - 'edge-b'") {
		t.Fatalf("expected clash proxy group to reference actual proxy names, got:\n%s", out)
	}
	if strings.Contains(out, "      - 'node-1'") || strings.Contains(out, "      - 'node-2'") {
		t.Fatalf("unexpected synthetic proxy names in clash proxy group:\n%s", out)
	}
}

func TestRenderClashIncludesMihomoRules(t *testing.T) {
	out := renderClashYAML([]string{
		"vless://11111111-1111-1111-1111-111111111111@example.com:443?encryption=none&type=tcp&security=reality&flow=xtls-rprx-vision&sni=example.com&fp=chrome&pbk=abc&sid=12#edge-a",
	})
	for _, want := range []string{
		"# Generated for Mihomo / Clash.Meta. Legacy Clash does not support VLESS REALITY.",
		"mixed-port: 7890",
		"global-client-fingerprint: chrome",
		"  enhanced-mode: fake-ip",
		"  - name: 'PROXY'",
		"  - name: 'AUTO'",
		"    type: url-test",
		"  openai:",
		"    url: 'https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geosite/openai.yaml'",
		"  geoip-cn:",
		"    behavior: ipcidr",
		"  - RULE-SET,openai,AI",
		"  - RULE-SET,geolocation-!cn,PROXY",
		"  - MATCH,Final",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected clash output to contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "  - MATCH,AUTO") {
		t.Fatalf("unexpected legacy catch-all in clash output:\n%s", out)
	}
}

func TestRenderNeverLeaksExtraJSON(t *testing.T) {
	u := repo.User{ID: 1, Username: "alice", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour)}
	link := &repo.ProxyLink{UUID: "11111111-1111-1111-1111-111111111111"}
	secret := `{"xray":{"vars":{"REALITY_PRIVATE_KEY":"supersecret"}}}`
	nodes := []repo.Node{
		{ID: 1, Name: "h1", Protocol: "hysteria2", CoreType: "xray", Host: "h.example.com", Port: 443, Enabled: true, SNI: "h.example.com", ExtraJSON: secret},
		{ID: 2, Name: "t1", Protocol: "tuic", CoreType: "xray", Host: "t.example.com", Port: 443, Enabled: true, SNI: "t.example.com", ExtraJSON: secret},
	}

	for _, target := range []string{"shadowrocket", "clash", "singbox"} {
		out, _, err := Render(target, u, link, nodes)
		if err != nil {
			t.Fatalf("target=%s err=%v", target, err)
		}
		if strings.Contains(out, "supersecret") || strings.Contains(out, "extra=") {
			t.Fatalf("target=%s leaked node ExtraJSON into subscriber payload:\n%s", target, out)
		}
	}
	// v2rayn is base64; decode before checking.
	out, _, err := Render("v2rayn", u, link, nodes)
	if err != nil {
		t.Fatalf("v2rayn err=%v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		t.Fatalf("decode v2rayn: %v", err)
	}
	if strings.Contains(string(decoded), "supersecret") || strings.Contains(string(decoded), "extra=") {
		t.Fatalf("v2rayn leaked node ExtraJSON:\n%s", decoded)
	}
}

func TestRenderNodeNameWithSpaces(t *testing.T) {
	u := repo.User{ID: 1, Username: "alice", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour)}
	link := &repo.ProxyLink{UUID: "11111111-1111-1111-1111-111111111111"}
	nodes := []repo.Node{{
		ID: 1, Name: "HK 01", Protocol: "vless_reality", CoreType: "xray",
		Host: "example.com", Port: 443, Enabled: true,
		Transport: "tcp", Security: "reality", SNI: "example.com",
		FP: "chrome", PublicKey: "abc", ShortID: "12",
	}}

	// Fragment must use %20, not "+": "+" survives fragment decoding and
	// corrupts the display name in clash/sing-box.
	raw, _, err := Render("shadowrocket", u, link, nodes)
	if err != nil {
		t.Fatalf("shadowrocket err=%v", err)
	}
	if strings.Contains(raw, "#HK+01-alice") {
		t.Fatalf("fragment uses + for space: %q", raw)
	}
	if !strings.Contains(raw, "#HK%2001-alice") {
		t.Fatalf("fragment should percent-encode the space: %q", raw)
	}

	// Downstream renderers decode it back to a literal space.
	yaml, _, err := Render("clash", u, link, nodes)
	if err != nil {
		t.Fatalf("clash err=%v", err)
	}
	if !strings.Contains(yaml, "'HK 01-alice'") {
		t.Fatalf("clash proxy name should contain the space:\n%s", yaml)
	}
	if strings.Contains(yaml, "HK+01") {
		t.Fatalf("clash proxy name contains +:\n%s", yaml)
	}
	jsonOut, _, err := Render("singbox", u, link, nodes)
	if err != nil {
		t.Fatalf("singbox err=%v", err)
	}
	if !strings.Contains(jsonOut, `"HK 01-alice"`) {
		t.Fatalf("singbox tag should contain the space:\n%s", jsonOut)
	}
}

func TestRendererRegistryAliasesAndUnknown(t *testing.T) {
	u := repo.User{ID: 1, Username: "alice", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour)}
	link := &repo.ProxyLink{
		UUID: "11111111-1111-1111-1111-111111111111",
		Link: "vless://11111111-1111-1111-1111-111111111111@example.com:443?encryption=none&type=tcp&security=reality&sni=example.com&pbk=abc&sid=12#n1-alice",
	}
	nodes := []repo.Node{{
		ID: 1, Name: "n1", Protocol: "vless_reality", CoreType: "xray",
		Host: "example.com", Port: 443, Enabled: true,
		Transport: "tcp", Security: "reality", SNI: "example.com",
		PublicKey: "abc", ShortID: "12",
	}}

	// Alias resolves to the same renderer.
	a, ctA, err := Render("sing-box", u, link, nodes)
	if err != nil {
		t.Fatalf("sing-box err=%v", err)
	}
	b, ctB, err := Render("singbox", u, link, nodes)
	if err != nil {
		t.Fatalf("singbox err=%v", err)
	}
	if a != b || ctA != ctB {
		t.Fatalf("alias output diverged")
	}

	// Unknown target errors before doing any URI work.
	if _, _, err := Render("quantumult", u, link, nodes); err == nil || !strings.Contains(err.Error(), "unsupported target") {
		t.Fatalf("unknown target should error, got %v", err)
	}

	// Every UA-detection result must exist in the registry.
	for _, target := range []string{"v2rayn", "clash", "singbox", "shadowrocket"} {
		if _, ok := resolveRenderer(target); !ok {
			t.Fatalf("UA-detectable target %q missing from registry", target)
		}
	}
	if got := Targets(); len(got) != 4 {
		t.Fatalf("Targets() = %v", got)
	}
}
