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
	}, "default")
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
	}, "default")
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

// An IPv6 host (reachable via the ObservedIP fallback) must be bracketed or
// every emitted URI is unparseable.
func TestRenderNodeURIBracketsIPv6Host(t *testing.T) {
	user := repo.User{Username: "alice"}
	link := &repo.ProxyLink{UUID: "11111111-1111-1111-1111-111111111111"}
	node := repo.Node{
		ID: 1, Name: "v6", Protocol: "vless_reality", Port: 443,
		ObservedIP: "2001:db8::1", PublicKey: "pbk", ShortID: "sid",
	}
	uri := renderNodeURI(node, user, link)
	if !strings.Contains(uri, "@[2001:db8::1]:443?") {
		t.Fatalf("IPv6 host must be bracketed: %s", uri)
	}
	p := parseProxyURI(uri)
	if p.Host != "2001:db8::1" || p.Port != 443 {
		t.Fatalf("parsed host/port: %q/%d", p.Host, p.Port)
	}
}

func TestRenderQueryFilters(t *testing.T) {
	u := repo.User{ID: 1, Username: "alice", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour)}
	link := &repo.ProxyLink{UUID: "11111111-1111-1111-1111-111111111111", Link: "vless://x@h:443?security=none#alice"}
	nodes := []repo.Node{
		{ID: 1, Name: "HK 01", Protocol: "vless_reality", CoreType: "xray", Host: "hk.example.com", Port: 443, PublicKey: "pbk", ShortID: "sid", Enabled: true},
		{ID: 2, Name: "JP 01", Protocol: "hysteria2", CoreType: "xray", Host: "jp.example.com", Port: 443, Enabled: true},
		{ID: 3, Name: "US 01", Protocol: "tuic", CoreType: "xray", Host: "us.example.com", Port: 443, Enabled: true},
	}

	// types= keeps only the requested protocols (with alias spellings).
	out, _, err := RenderWithOptions("shadowrocket", u, link, nodes, RenderOptions{Protocols: []string{"hy2"}})
	if err != nil {
		t.Fatalf("types filter: %v", err)
	}
	if !strings.Contains(out, "hysteria2://") || strings.Contains(out, "vless://") || strings.Contains(out, "tuic://") {
		t.Fatalf("types filter leaked other protocols:\n%s", out)
	}

	// filter= keyword on the node name, case-insensitive, | for OR.
	out, _, err = RenderWithOptions("shadowrocket", u, link, nodes, RenderOptions{NameFilter: "hk|us"})
	if err != nil {
		t.Fatalf("name filter: %v", err)
	}
	if !strings.Contains(out, "hk.example.com") || !strings.Contains(out, "us.example.com") || strings.Contains(out, "jp.example.com") {
		t.Fatalf("name filter wrong selection:\n%s", out)
	}

	// A filter that matches nothing is an explicit error — never the stale
	// active-link fallback.
	_, _, err = RenderWithOptions("shadowrocket", u, link, nodes, RenderOptions{AllowActiveLinkFallback: true, NameFilter: "nowhere"})
	if err == nil || !strings.Contains(err.Error(), "filter") {
		t.Fatalf("empty filter result should error, got %v", err)
	}

	// Unknown types value is a client error, not a silent no-op.
	_, _, err = RenderWithOptions("shadowrocket", u, link, nodes, RenderOptions{Protocols: []string{"wireguard"}})
	if err == nil || !strings.Contains(err.Error(), "unsupported protocol type") {
		t.Fatalf("unknown type should error, got %v", err)
	}
}

func TestRenderMihomoAliasAndUA(t *testing.T) {
	u := repo.User{ID: 1, Username: "alice", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour)}
	link := &repo.ProxyLink{UUID: "11111111-1111-1111-1111-111111111111"}
	nodes := []repo.Node{{ID: 1, Name: "n1", Protocol: "vless_reality", CoreType: "xray", Host: "example.com", Port: 443, PublicKey: "pbk", ShortID: "sid", Enabled: true}}

	for _, alias := range []string{"mihomo", "clashmeta", "clash.meta", "stash"} {
		out, ct, err := Render(alias, u, link, nodes)
		if err != nil {
			t.Fatalf("alias %s: %v", alias, err)
		}
		if !strings.Contains(ct, "yaml") || !strings.Contains(out, "proxies:") {
			t.Fatalf("alias %s did not produce clash yaml", alias)
		}
	}
	if got := DetectTargetFromUA("mihomo/1.18.0"); got != "clash" {
		t.Fatalf("mihomo UA detected as %q", got)
	}
}

func TestNormalizePreset(t *testing.T) {
	for raw, want := range map[string]string{
		"": "default", "default": "default", "GLOBAL": "global",
		" minimal ": "minimal",
	} {
		got, err := NormalizePreset(raw)
		if err != nil || got != want {
			t.Fatalf("NormalizePreset(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := NormalizePreset("fancy"); err == nil {
		t.Fatalf("unknown preset should error")
	}
}

func TestRenderClashPresets(t *testing.T) {
	uris := []string{
		"vless://11111111-1111-1111-1111-111111111111@example.com:443?encryption=none&type=tcp&security=reality&flow=xtls-rprx-vision&sni=example.com&fp=chrome&pbk=abc&sid=12#edge-a",
	}

	global := renderClashYAML(uris, "global")
	for _, want := range []string{
		"  - name: 'PROXY'",
		"  - name: 'AUTO'",
		"  - IP-CIDR,192.168.0.0/16,DIRECT,no-resolve",
		"  - MATCH,PROXY",
		"  enhanced-mode: fake-ip", // global keeps the DNS block
	} {
		if !strings.Contains(global, want) {
			t.Fatalf("global preset missing %q:\n%s", want, global)
		}
	}
	for _, banned := range []string{"rule-providers:", "RULE-SET,", "MATCH,Final", "- name: 'AI'"} {
		if strings.Contains(global, banned) {
			t.Fatalf("global preset must not contain %q:\n%s", banned, global)
		}
	}

	minimal := renderClashYAML(uris, "minimal")
	for _, want := range []string{"  - name: 'PROXY'", "  - name: 'AUTO'", "  - MATCH,PROXY", "  - name: 'edge-a'"} {
		if !strings.Contains(minimal, want) {
			t.Fatalf("minimal preset missing %q:\n%s", want, minimal)
		}
	}
	for _, banned := range []string{"rule-providers:", "RULE-SET,", "dns:", "IP-CIDR,"} {
		if strings.Contains(minimal, banned) {
			t.Fatalf("minimal preset must not contain %q:\n%s", banned, minimal)
		}
	}

	// All presets share the same proxies section.
	def := renderClashYAML(uris, "default")
	for _, out := range []string{def, global, minimal} {
		if !strings.Contains(out, "    server: 'example.com'") {
			t.Fatalf("preset output missing proxy server:\n%s", out)
		}
	}
}

func TestRenderWithOptionsPreset(t *testing.T) {
	u := repo.User{ID: 1, Username: "alice", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour)}
	link := &repo.ProxyLink{UUID: "11111111-1111-1111-1111-111111111111"}
	nodes := []repo.Node{{ID: 1, Name: "n1", Protocol: "vless_reality", CoreType: "xray", Host: "example.com", Port: 443, PublicKey: "pbk", ShortID: "sid", Enabled: true}}

	out, _, err := RenderWithOptions("clash", u, link, nodes, RenderOptions{Preset: "global"})
	if err != nil {
		t.Fatalf("preset=global err=%v", err)
	}
	if !strings.Contains(out, "  - MATCH,PROXY") || strings.Contains(out, "rule-providers:") {
		t.Fatalf("preset=global did not change clash shape:\n%s", out)
	}

	// Unknown preset is a render error (handler maps it to 400).
	if _, _, err := RenderWithOptions("clash", u, link, nodes, RenderOptions{Preset: "nope"}); err == nil {
		t.Fatalf("unknown preset should error")
	}

	// URI-list targets ignore presets: same bytes for any preset value.
	plain, _, err := RenderWithOptions("shadowrocket", u, link, nodes, RenderOptions{})
	if err != nil {
		t.Fatalf("shadowrocket err=%v", err)
	}
	withPreset, _, err := RenderWithOptions("shadowrocket", u, link, nodes, RenderOptions{Preset: "minimal"})
	if err != nil {
		t.Fatalf("shadowrocket preset err=%v", err)
	}
	if plain != withPreset {
		t.Fatalf("shadowrocket output must be preset-independent")
	}
}
