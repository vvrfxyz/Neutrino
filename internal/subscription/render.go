package subscription

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"neutrino/internal/repo"
)

func BuildSubscriptionURL(baseURL, token, target string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8080"
	}
	if target == "" {
		target = "v2rayn"
	}
	return baseURL + "/sub/" + token + "?target=" + url.QueryEscape(target)
}

type RenderOptions struct {
	AllowActiveLinkFallback bool
	// Protocols restricts output to these node protocols (panel names:
	// vless_reality | hysteria2 | tuic). Empty means all the target supports.
	Protocols []string
	// NameFilter keeps only nodes whose display name contains one of the
	// "|"-separated keywords (case-insensitive). Empty keeps everything.
	NameFilter string
	// Preset picks a built-in profile shape for profile-emitting targets
	// (clash today): default | global | minimal. Empty means default.
	// Plain URI-list targets (v2rayn/shadowrocket) and the singbox outbound
	// fragment ignore it — same philosophy as the protocol capability set:
	// what a target cannot express is dropped, not an error.
	Preset string
}

// Renderer turns a list of per-node proxy URIs into a client-specific payload.
type Renderer struct {
	ContentType string
	Render      func(uris []string, preset string) (string, error)
	// Protocols is the client capability set: node protocols this target can
	// express. Nodes outside it are dropped instead of emitting entries the
	// client cannot parse (or dangling group references in clash YAML).
	Protocols map[string]bool
}

var allProtocols = map[string]bool{"vless_reality": true, "hysteria2": true, "tuic": true}

// renderers is the target registry. Keys are canonical lowercase target names;
// aliases map onto the same Renderer. DetectTargetFromUA must only return keys
// that exist here.
var renderers = map[string]Renderer{
	"v2rayn": {
		ContentType: "text/plain; charset=utf-8",
		Protocols:   allProtocols,
		Render: func(uris []string, _ string) (string, error) {
			raw := strings.Join(uris, "\n")
			return base64.StdEncoding.EncodeToString([]byte(raw)), nil
		},
	},
	"shadowrocket": {
		ContentType: "text/plain; charset=utf-8",
		Protocols:   allProtocols,
		Render: func(uris []string, _ string) (string, error) {
			return strings.Join(uris, "\n"), nil
		},
	},
	"clash": {
		ContentType: "text/yaml; charset=utf-8",
		// The YAML is Mihomo/Clash.Meta-shaped; Meta cores speak all three.
		Protocols: allProtocols,
		Render: func(uris []string, preset string) (string, error) {
			return renderClashYAML(uris, preset), nil
		},
	},
	"singbox": {
		ContentType: "application/json",
		Protocols:   allProtocols,
		Render: func(uris []string, _ string) (string, error) {
			return renderSingBoxJSON(uris)
		},
	},
}

// targetAliases maps accepted spellings onto canonical registry keys.
var targetAliases = map[string]string{
	"sing-box":   "singbox",
	"mihomo":     "clash",
	"clashmeta":  "clash",
	"clash.meta": "clash",
	"stash":      "clash",
}

// Targets returns the canonical renderer names (sorted), for docs/UI.
func Targets() []string {
	out := make([]string, 0, len(renderers))
	for name := range renderers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// presetNames is the built-in profile preset registry. Presets are fixed
// shapes compiled into the panel — never user-supplied URLs or rule text —
// so a subscription link can pick a profile style without opening an
// injection surface.
//
//   - default: full split-routing profile (external geosite rule-providers).
//   - global:  no external rule-providers; LAN/private direct, all else PROXY.
//   - minimal: nodes + PROXY/AUTO groups + MATCH only, for users who keep
//     their own rules client-side.
var presetNames = map[string]bool{"default": true, "global": true, "minimal": true}

// Presets returns the canonical preset names (sorted), for docs/UI.
func Presets() []string {
	out := make([]string, 0, len(presetNames))
	for name := range presetNames {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// NormalizePreset canonicalizes a ?preset= value. Empty means default;
// unknown names are an error so a typo never silently serves the wrong
// profile shape.
func NormalizePreset(raw string) (string, error) {
	p := strings.ToLower(strings.TrimSpace(raw))
	if p == "" {
		return "default", nil
	}
	if !presetNames[p] {
		return "", fmt.Errorf("unsupported preset: %s", p)
	}
	return p, nil
}

func resolveRenderer(target string) (Renderer, bool) {
	if canonical, ok := targetAliases[target]; ok {
		target = canonical
	}
	r, ok := renderers[target]
	return r, ok
}

func Render(target string, user repo.User, activeLink *repo.ProxyLink, nodes []repo.Node) (string, string, error) {
	return RenderWithOptions(target, user, activeLink, nodes, RenderOptions{AllowActiveLinkFallback: true})
}

func RenderWithOptions(target string, user repo.User, activeLink *repo.ProxyLink, nodes []repo.Node, opts RenderOptions) (string, string, error) {
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "" {
		target = "v2rayn"
	}
	renderer, ok := resolveRenderer(target)
	if !ok {
		return "", "", fmt.Errorf("unsupported target: %s", target)
	}

	wantProtocols, err := normalizeProtocolFilter(opts.Protocols)
	if err != nil {
		return "", "", err
	}
	preset, err := NormalizePreset(opts.Preset)
	if err != nil {
		return "", "", err
	}
	nameKeywords := normalizeNameFilter(opts.NameFilter)

	// Prefer per-node URIs when available.
	// The user's stored "active link" can become stale after config changes; it is used only as a fallback
	// when no node URIs can be rendered yet.
	uris := make([]string, 0, len(nodes))
	seen := make(map[string]struct{}, len(nodes))
	addURI := func(uri string) {
		uri = strings.TrimSpace(uri)
		if uri == "" {
			return
		}
		key := canonicalProxyURIKey(uri)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		uris = append(uris, uri)
	}

	// Render node URIs first, applying the capability set and query filters.
	for _, node := range nodes {
		if renderer.Protocols != nil && !renderer.Protocols[node.Protocol] {
			continue
		}
		if wantProtocols != nil && !wantProtocols[node.Protocol] {
			continue
		}
		if !nodeNameMatches(node, nameKeywords) {
			continue
		}
		uri := renderNodeURI(node, user, activeLink)
		addURI(uri)
	}
	// Fallback to active link only if nodes are not renderable (e.g. missing
	// pbk/sid report) AND the caller did not narrow the selection — an explicit
	// filter that matches nothing must say so, not serve unrelated config.
	filtered := wantProtocols != nil || len(nameKeywords) > 0
	if opts.AllowActiveLinkFallback && !filtered && len(uris) == 0 && activeLink != nil {
		addURI(activeLink.Link)
	}
	if len(uris) == 0 {
		if filtered {
			return "", "", fmt.Errorf("no nodes match the requested filter")
		}
		return "", "", fmt.Errorf("no available nodes")
	}

	payload, err := renderer.Render(uris, preset)
	if err != nil {
		return "", "", err
	}
	return payload, renderer.ContentType, nil
}

// protocolFilterAliases accepts common client-side spellings for panel
// protocol names in the ?types= parameter.
var protocolFilterAliases = map[string]string{
	"vless":         "vless_reality",
	"vless_reality": "vless_reality",
	"reality":       "vless_reality",
	"hysteria2":     "hysteria2",
	"hy2":           "hysteria2",
	"tuic":          "tuic",
}

func normalizeProtocolFilter(in []string) (map[string]bool, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]bool, len(in))
	for _, raw := range in {
		p := strings.ToLower(strings.TrimSpace(raw))
		if p == "" {
			continue
		}
		canonical, ok := protocolFilterAliases[p]
		if !ok {
			return nil, fmt.Errorf("unsupported protocol type: %s", p)
		}
		out[canonical] = true
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func normalizeNameFilter(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func nodeNameMatches(node repo.Node, keywords []string) bool {
	if len(keywords) == 0 {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(node.Name))
	if name == "" {
		name = fmt.Sprintf("node-%d", node.ID)
	}
	for _, kw := range keywords {
		if strings.Contains(name, kw) {
			return true
		}
	}
	return false
}

// canonicalProxyURIKey normalizes a proxy URI for comparison.
// We only use it as a de-dup key; the original string is preserved in output.
//
// Today it mainly fixes duplicates caused by different query-parameter orderings.
func canonicalProxyURIKey(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u == nil || strings.TrimSpace(u.Scheme) == "" {
		return raw
	}
	u.Scheme = strings.ToLower(strings.TrimSpace(u.Scheme))
	u.RawQuery = u.Query().Encode()
	return u.String()
}

func renderNodeURI(node repo.Node, user repo.User, activeLink *repo.ProxyLink) string {
	nodeName := strings.TrimSpace(node.Name)
	if nodeName == "" {
		nodeName = fmt.Sprintf("node-%d", node.ID)
	}
	// Escape for the URI fragment. PathEscape encodes a space as %20, which
	// url.Parse round-trips back to a space; QueryEscape would use "+", which
	// fragment decoding does NOT reverse — names like "HK 01" rendered as
	// "HK+01-alice" in clash/sing-box outputs.
	nodeName = url.PathEscape(nodeName + "-" + user.Username)

	// A per-user UUID is required to render node URIs.
	// Subscriptions should ensure an active link exists (create one on-demand) rather than emitting placeholders.
	if activeLink == nil || strings.TrimSpace(activeLink.UUID) == "" {
		return ""
	}

	// Host can be configured by admin; if empty, fall back to observed IP from node mTLS connection.
	host := strings.TrimSpace(node.Host)
	if host == "" {
		host = strings.TrimSpace(node.ObservedIP)
	}
	if host == "" {
		return ""
	}
	// ObservedIP can legitimately be IPv6; an unbracketed IPv6 host makes the
	// URI unparseable for every client.
	hostPort := net.JoinHostPort(host, strconv.Itoa(node.Port))

	switch node.Protocol {
	case "vless_reality":
		q := url.Values{}
		q.Set("encryption", "none")
		if strings.TrimSpace(node.Transport) != "" {
			q.Set("type", node.Transport)
		} else {
			q.Set("type", "tcp")
		}
		if strings.TrimSpace(node.Security) != "" {
			q.Set("security", node.Security)
		} else {
			q.Set("security", "reality")
		}
		if strings.TrimSpace(node.Flow) != "" {
			q.Set("flow", node.Flow)
		}
		if strings.TrimSpace(node.SNI) != "" {
			q.Set("sni", node.SNI)
		}
		if strings.TrimSpace(node.FP) != "" {
			q.Set("fp", node.FP)
		}
		if strings.TrimSpace(node.PublicKey) != "" {
			q.Set("pbk", node.PublicKey)
		}
		if strings.TrimSpace(node.ShortID) != "" {
			q.Set("sid", node.ShortID)
		}
		// For REALITY links, pbk+sid are required; omit nodes that haven't reported them yet.
		if q.Get("security") == "reality" && (strings.TrimSpace(q.Get("pbk")) == "" || strings.TrimSpace(q.Get("sid")) == "") {
			return ""
		}
		return "vless://" + activeLink.UUID + "@" + hostPort + "?" + q.Encode() + "#" + nodeName
	case "hysteria2":
		q := url.Values{}
		if strings.TrimSpace(node.SNI) != "" {
			q.Set("sni", node.SNI)
		}
		// node.ExtraJSON is server-side node configuration (managed Xray
		// template/vars). It must never be embedded in subscriber-facing URIs:
		// that leaks panel internals to every subscription holder, and no
		// client consumes an "extra" query param anyway.
		return "hysteria2://" + activeLink.UUID + "@" + hostPort + "?" + q.Encode() + "#" + nodeName
	case "tuic":
		q := url.Values{}
		if strings.TrimSpace(node.SNI) != "" {
			q.Set("sni", node.SNI)
		}
		return "tuic://" + activeLink.UUID + ":" + activeLink.UUID + "@" + hostPort + "?" + q.Encode() + "#" + nodeName
	default:
		return ""
	}
}

// clashBasePrelude is the general-settings header every clash preset shares.
var clashBasePrelude = []string{
	"# Generated for Mihomo / Clash.Meta. Legacy Clash does not support VLESS REALITY.",
	"mixed-port: 7890",
	"allow-lan: true",
	"mode: rule",
	"log-level: info",
	"ipv6: true",
	"unified-delay: true",
	"tcp-concurrent: true",
	"global-client-fingerprint: chrome",
	"profile:",
	"  store-selected: true",
	"  store-fake-ip: true",
}

// clashDNSPrelude is the opinionated fake-ip DNS block. The minimal preset
// omits it so client-side DNS choices are left alone.
var clashDNSPrelude = []string{
	"dns:",
	"  enable: true",
	"  listen: 0.0.0.0:1053",
	"  ipv6: true",
	"  enhanced-mode: fake-ip",
	"  fake-ip-range: 198.18.0.1/16",
	"  default-nameserver:",
	"    - 223.5.5.5",
	"    - 119.29.29.29",
	"    - 1.1.1.1",
	"  nameserver:",
	"    - https://dns.alidns.com/dns-query",
	"    - https://doh.pub/dns-query",
	"  fallback:",
	"    - https://dns.google/dns-query",
	"    - https://cloudflare-dns.com/dns-query",
	"  fallback-filter:",
	"    geoip: true",
	"    geoip-code: CN",
	"    ipcidr:",
	"      - 240.0.0.0/4",
	"  fake-ip-filter:",
	"    - '*.lan'",
	"    - '*.local'",
	"    - '*.localhost'",
	"    - time.*.com",
	"    - ntp.*.com",
	"    - '*.msftconnecttest.com'",
	"    - '*.msftncsi.com'",
	"    - localhost.ptlogin2.qq.com",
}

func renderClashYAML(uris []string, preset string) string {
	lines := append([]string{}, clashBasePrelude...)
	if preset != "minimal" {
		lines = append(lines, clashDNSPrelude...)
	}
	lines = append(lines, "proxies:")
	lines, proxyNames := appendClashProxies(lines, uris)
	switch preset {
	case "global":
		// No external rule-providers: LAN/private stays direct, everything
		// else goes through PROXY. Usable where rule-set CDNs are unreachable.
		lines = appendClashCoreProxyGroups(lines, proxyNames)
		lines = append(lines, "rules:")
		for _, rule := range clashLanDirectRules {
			lines = append(lines, "  - "+rule)
		}
		lines = append(lines, "  - MATCH,PROXY")
	case "minimal":
		// Bare nodes + core groups + MATCH, for users who keep their own
		// rules client-side or merge this into an existing profile.
		lines = appendClashCoreProxyGroups(lines, proxyNames)
		lines = append(lines, "rules:", "  - MATCH,PROXY")
	default:
		lines = appendClashProxyGroups(lines, proxyNames)
		lines = appendClashRuleProviders(lines)
		lines = append(lines, "rules:")
		for _, rule := range clashDefaultRules {
			lines = append(lines, "  - "+rule)
		}
	}
	return strings.Join(lines, "\n")
}

func appendClashProxies(lines []string, uris []string) ([]string, []string) {
	proxyNames := make([]string, 0, len(uris))
	for i, uri := range uris {
		p := parseProxyURI(uri)
		name := p.Name
		if name == "" {
			name = fmt.Sprintf("node-%d", i+1)
		}
		proxyNames = append(proxyNames, name)
		switch p.Scheme {
		case "vless":
			lines = append(lines,
				"  - name: "+quoteClashYAML(name),
				"    type: vless",
				"    server: "+quoteClashYAML(p.Host),
				"    port: "+strconv.Itoa(p.Port),
				"    uuid: "+quoteClashYAML(p.User),
				"    encryption: none",
				"    network: "+quoteClashYAML(firstNonEmpty(p.Query.Get("type"), "tcp")),
				"    tls: true",
				"    udp: true",
				"    servername: "+quoteClashYAML(p.Query.Get("sni")),
				"    flow: "+quoteClashYAML(p.Query.Get("flow")),
				"    client-fingerprint: "+quoteClashYAML(p.Query.Get("fp")),
			)
			if p.Query.Get("security") == "reality" {
				lines = append(lines,
					"    reality-opts:",
					"      public-key: "+quoteClashYAML(p.Query.Get("pbk")),
					"      short-id: "+quoteClashYAML(p.Query.Get("sid")),
				)
			}
		case "hysteria2":
			lines = append(lines,
				"  - name: "+quoteClashYAML(name),
				"    type: hysteria2",
				"    server: "+quoteClashYAML(p.Host),
				"    port: "+strconv.Itoa(p.Port),
				"    password: "+quoteClashYAML(p.User),
				"    sni: "+quoteClashYAML(p.Query.Get("sni")),
			)
		case "tuic":
			lines = append(lines,
				"  - name: "+quoteClashYAML(name),
				"    type: tuic",
				"    server: "+quoteClashYAML(p.Host),
				"    port: "+strconv.Itoa(p.Port),
				"    uuid: "+quoteClashYAML(p.User),
				"    password: "+quoteClashYAML(firstNonEmpty(p.Password, p.User)),
				"    sni: "+quoteClashYAML(p.Query.Get("sni")),
			)
		default:
			lines = append(lines,
				"  - name: "+quoteClashYAML(name),
				"    type: vless",
				"    server: "+quoteClashYAML(uri),
			)
		}
	}
	return lines, proxyNames
}

// appendClashCoreProxyGroups emits the two groups every preset relies on:
// PROXY (manual select) and AUTO (latency url-test).
func appendClashCoreProxyGroups(lines []string, proxyNames []string) []string {
	lines = append(lines, "proxy-groups:")
	lines = appendClashProxyGroup(lines, "PROXY", "select", append([]string{"AUTO", "DIRECT"}, proxyNames...), "", 0, 0)
	lines = appendClashProxyGroup(lines, "AUTO", "url-test", proxyNames, "https://www.gstatic.com/generate_204", 300, 50)
	return lines
}

func appendClashProxyGroups(lines []string, proxyNames []string) []string {
	lines = appendClashCoreProxyGroups(lines, proxyNames)
	selectProxyOptions := append([]string{"PROXY", "AUTO"}, proxyNames...)
	selectProxyOptions = append(selectProxyOptions, "DIRECT")
	for _, name := range []string{"AI", "Streaming", "Telegram", "Google"} {
		lines = appendClashProxyGroup(lines, name, "select", selectProxyOptions, "", 0, 0)
	}
	directFirstOptions := append([]string{"DIRECT", "PROXY", "AUTO"}, proxyNames...)
	lines = appendClashProxyGroup(lines, "Apple", "select", directFirstOptions, "", 0, 0)
	lines = appendClashProxyGroup(lines, "Microsoft", "select", directFirstOptions, "", 0, 0)
	finalOptions := append([]string{"PROXY", "DIRECT", "AUTO"}, proxyNames...)
	lines = appendClashProxyGroup(lines, "Final", "select", finalOptions, "", 0, 0)
	lines = appendClashProxyGroup(lines, "GLOBAL", "select", []string{
		"PROXY", "AUTO", "AI", "Streaming", "Telegram", "Google", "Apple", "Microsoft", "Final", "DIRECT",
	}, "", 0, 0)
	return lines
}

func appendClashProxyGroup(lines []string, name, groupType string, proxies []string, testURL string, interval int, tolerance int) []string {
	lines = append(lines,
		"  - name: "+quoteClashYAML(name),
		"    type: "+groupType,
		"    proxies:",
	)
	for _, proxy := range proxies {
		lines = append(lines, "      - "+quoteClashYAML(proxy))
	}
	if testURL != "" {
		lines = append(lines,
			"    url: "+quoteClashYAML(testURL),
			"    interval: "+strconv.Itoa(interval),
			"    tolerance: "+strconv.Itoa(tolerance),
		)
	}
	return lines
}

type clashRuleProvider struct {
	Name     string
	Behavior string
	Path     string
	URL      string
}

var clashDefaultRuleProviders = []clashRuleProvider{
	{Name: "reject", Behavior: "domain", Path: "./ruleset/reject.yaml", URL: "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geosite/category-ads-all.yaml"},
	{Name: "private", Behavior: "domain", Path: "./ruleset/private.yaml", URL: "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geosite/private.yaml"},
	{Name: "openai", Behavior: "domain", Path: "./ruleset/openai.yaml", URL: "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geosite/openai.yaml"},
	{Name: "telegram", Behavior: "domain", Path: "./ruleset/telegram.yaml", URL: "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geosite/telegram.yaml"},
	{Name: "youtube", Behavior: "domain", Path: "./ruleset/youtube.yaml", URL: "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geosite/youtube.yaml"},
	{Name: "netflix", Behavior: "domain", Path: "./ruleset/netflix.yaml", URL: "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geosite/netflix.yaml"},
	{Name: "tiktok", Behavior: "domain", Path: "./ruleset/tiktok.yaml", URL: "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geosite/tiktok.yaml"},
	{Name: "spotify", Behavior: "domain", Path: "./ruleset/spotify.yaml", URL: "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geosite/spotify.yaml"},
	{Name: "google", Behavior: "domain", Path: "./ruleset/google.yaml", URL: "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geosite/google.yaml"},
	{Name: "apple", Behavior: "domain", Path: "./ruleset/apple.yaml", URL: "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geosite/apple.yaml"},
	{Name: "microsoft", Behavior: "domain", Path: "./ruleset/microsoft.yaml", URL: "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geosite/microsoft.yaml"},
	{Name: "geolocation-!cn", Behavior: "domain", Path: "./ruleset/geolocation-!cn.yaml", URL: "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geosite/geolocation-!cn.yaml"},
	{Name: "cn", Behavior: "domain", Path: "./ruleset/cn.yaml", URL: "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geosite/cn.yaml"},
	{Name: "geoip-cn", Behavior: "ipcidr", Path: "./ruleset/geoip-cn.yaml", URL: "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/geoip/cn.yaml"},
}

func appendClashRuleProviders(lines []string) []string {
	lines = append(lines, "rule-providers:")
	for _, provider := range clashDefaultRuleProviders {
		lines = append(lines,
			"  "+provider.Name+":",
			"    type: http",
			"    behavior: "+provider.Behavior,
			"    format: yaml",
			"    path: "+quoteClashYAML(provider.Path),
			"    url: "+quoteClashYAML(provider.URL),
			"    interval: 86400",
		)
	}
	return lines
}

// clashLanDirectRules keeps loopback/RFC1918/link-local traffic direct
// without any external rule-provider; shared by default and global presets.
var clashLanDirectRules = []string{
	"DOMAIN-SUFFIX,local,DIRECT",
	"DOMAIN-SUFFIX,lan,DIRECT",
	"DOMAIN-SUFFIX,localhost,DIRECT",
	"IP-CIDR,127.0.0.0/8,DIRECT,no-resolve",
	"IP-CIDR,10.0.0.0/8,DIRECT,no-resolve",
	"IP-CIDR,172.16.0.0/12,DIRECT,no-resolve",
	"IP-CIDR,192.168.0.0/16,DIRECT,no-resolve",
	"IP-CIDR,169.254.0.0/16,DIRECT,no-resolve",
	"IP-CIDR6,::1/128,DIRECT,no-resolve",
	"IP-CIDR6,fc00::/7,DIRECT,no-resolve",
	"IP-CIDR6,fe80::/10,DIRECT,no-resolve",
}

var clashDefaultRules = append(
	append([]string{"RULE-SET,reject,REJECT"}, clashLanDirectRules...),
	"RULE-SET,private,DIRECT",
	"RULE-SET,openai,AI",
	"RULE-SET,telegram,Telegram",
	"RULE-SET,youtube,Streaming",
	"RULE-SET,netflix,Streaming",
	"RULE-SET,tiktok,Streaming",
	"RULE-SET,spotify,Streaming",
	"RULE-SET,google,Google",
	"RULE-SET,apple,Apple",
	"RULE-SET,microsoft,Microsoft",
	"RULE-SET,geolocation-!cn,PROXY",
	"RULE-SET,cn,DIRECT",
	"RULE-SET,geoip-cn,DIRECT,no-resolve",
	"MATCH,Final",
)

func quoteClashYAML(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

func renderSingBoxJSON(uris []string) (string, error) {
	outbounds := make([]map[string]any, 0, len(uris))
	for i, uri := range uris {
		p := parseProxyURI(uri)
		tag := p.Name
		if tag == "" {
			tag = fmt.Sprintf("node-%d", i+1)
		}
		switch p.Scheme {
		case "vless":
			item := map[string]any{
				"type":            "vless",
				"tag":             tag,
				"server":          p.Host,
				"server_port":     p.Port,
				"uuid":            p.User,
				"flow":            p.Query.Get("flow"),
				"packet_encoding": "xudp",
				"tls": map[string]any{
					"enabled":     true,
					"server_name": p.Query.Get("sni"),
					"utls":        map[string]any{"enabled": true, "fingerprint": firstNonEmpty(p.Query.Get("fp"), "chrome")},
					"reality":     map[string]any{"enabled": p.Query.Get("security") == "reality", "public_key": p.Query.Get("pbk"), "short_id": p.Query.Get("sid")},
				},
			}
			outbounds = append(outbounds, item)
		case "hysteria2":
			outbounds = append(outbounds, map[string]any{
				"type":        "hysteria2",
				"tag":         tag,
				"server":      p.Host,
				"server_port": p.Port,
				"password":    p.User,
				"tls": map[string]any{
					"enabled":     true,
					"server_name": p.Query.Get("sni"),
				},
			})
		case "tuic":
			outbounds = append(outbounds, map[string]any{
				"type":        "tuic",
				"tag":         tag,
				"server":      p.Host,
				"server_port": p.Port,
				"uuid":        p.User,
				"password":    firstNonEmpty(p.Password, p.User),
				"tls": map[string]any{
					"enabled":     true,
					"server_name": p.Query.Get("sni"),
				},
			})
		default:
			outbounds = append(outbounds, map[string]any{
				"type": "direct",
				"tag":  tag,
			})
		}
	}
	if len(outbounds) == 0 {
		outbounds = append(outbounds, map[string]any{"type": "direct", "tag": "direct"})
	}
	payload := map[string]any{
		"log":       map[string]any{"level": "info"},
		"outbounds": outbounds,
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type parsedProxy struct {
	Scheme   string
	Host     string
	Port     int
	User     string
	Password string
	Name     string
	Query    url.Values
}

func parseProxyURI(raw string) parsedProxy {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return parsedProxy{Scheme: "", Query: url.Values{}}
	}
	out := parsedProxy{
		Scheme: strings.ToLower(u.Scheme),
		Host:   u.Hostname(),
		Port:   0,
		Name:   strings.TrimSpace(u.Fragment),
		Query:  u.Query(),
	}
	if out.Name == "" {
		out.Name = u.Hostname()
	}
	port, _ := strconv.Atoi(u.Port())
	out.Port = port
	if u.User != nil {
		out.User = u.User.Username()
		pw, _ := u.User.Password()
		out.Password = pw
	}
	return out
}

func firstNonEmpty(v string, fallback string) string {
	v = strings.TrimSpace(v)
	if v != "" {
		return v
	}
	return fallback
}
