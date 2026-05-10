package subscription

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
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
}

func Render(target string, user repo.User, activeLink *repo.ProxyLink, nodes []repo.Node) (string, string, error) {
	return RenderWithOptions(target, user, activeLink, nodes, RenderOptions{AllowActiveLinkFallback: true})
}

func RenderWithOptions(target string, user repo.User, activeLink *repo.ProxyLink, nodes []repo.Node, opts RenderOptions) (string, string, error) {
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "" {
		target = "v2rayn"
	}

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

	// Render node URIs first.
	for _, node := range nodes {
		uri := renderNodeURI(node, user, activeLink)
		addURI(uri)
	}
	// Fallback to active link only if nodes are not renderable (e.g. missing pbk/sid report).
	if opts.AllowActiveLinkFallback && len(uris) == 0 && activeLink != nil {
		addURI(activeLink.Link)
	}
	if len(uris) == 0 {
		return "", "", fmt.Errorf("no available nodes")
	}

	switch target {
	case "v2rayn":
		raw := strings.Join(uris, "\n")
		return base64.StdEncoding.EncodeToString([]byte(raw)), "text/plain; charset=utf-8", nil
	case "shadowrocket":
		return strings.Join(uris, "\n"), "text/plain; charset=utf-8", nil
	case "clash":
		yamlText := renderClashYAML(uris)
		return yamlText, "text/yaml; charset=utf-8", nil
	case "singbox", "sing-box":
		jsonText, err := renderSingBoxJSON(uris)
		if err != nil {
			return "", "", err
		}
		return jsonText, "application/json", nil
	default:
		return "", "", fmt.Errorf("unsupported target: %s", target)
	}
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
	nodeName = url.QueryEscape(nodeName + "-" + user.Username)

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
		return "vless://" + activeLink.UUID + "@" + host + ":" + strconv.Itoa(node.Port) + "?" + q.Encode() + "#" + nodeName
	case "hysteria2":
		q := url.Values{}
		if strings.TrimSpace(node.SNI) != "" {
			q.Set("sni", node.SNI)
		}
		if strings.TrimSpace(node.ExtraJSON) != "" {
			q.Set("extra", node.ExtraJSON)
		}
		return "hysteria2://" + activeLink.UUID + "@" + host + ":" + strconv.Itoa(node.Port) + "?" + q.Encode() + "#" + nodeName
	case "tuic":
		q := url.Values{}
		if strings.TrimSpace(node.SNI) != "" {
			q.Set("sni", node.SNI)
		}
		if strings.TrimSpace(node.ExtraJSON) != "" {
			q.Set("extra", node.ExtraJSON)
		}
		return "tuic://" + activeLink.UUID + ":" + activeLink.UUID + "@" + host + ":" + strconv.Itoa(node.Port) + "?" + q.Encode() + "#" + nodeName
	default:
		return ""
	}
}

func renderClashYAML(uris []string) string {
	lines := []string{
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
		"proxies:",
	}
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
	lines = appendClashProxyGroups(lines, proxyNames)
	lines = appendClashRuleProviders(lines)
	lines = append(lines, "rules:")
	for _, rule := range clashDefaultRules {
		lines = append(lines, "  - "+rule)
	}
	return strings.Join(lines, "\n")
}

func appendClashProxyGroups(lines []string, proxyNames []string) []string {
	lines = append(lines, "proxy-groups:")
	lines = appendClashProxyGroup(lines, "PROXY", "select", append([]string{"AUTO", "DIRECT"}, proxyNames...), "", 0, 0)
	lines = appendClashProxyGroup(lines, "AUTO", "url-test", proxyNames, "https://www.gstatic.com/generate_204", 300, 50)
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

var clashDefaultRules = []string{
	"RULE-SET,reject,REJECT",
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
}

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
