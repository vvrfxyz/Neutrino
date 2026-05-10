package subscription

import (
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
