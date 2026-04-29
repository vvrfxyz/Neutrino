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
		"port: 7890",
		"socks-port: 7891",
		"allow-lan: true",
		"mode: rule",
		"log-level: info",
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
				"  - name: "+strconv.Quote(name),
				"    type: vless",
				"    server: "+p.Host,
				"    port: "+strconv.Itoa(p.Port),
				"    uuid: "+p.User,
				"    network: "+firstNonEmpty(p.Query.Get("type"), "tcp"),
				"    tls: true",
				"    servername: "+p.Query.Get("sni"),
				"    flow: "+p.Query.Get("flow"),
				"    client-fingerprint: "+p.Query.Get("fp"),
			)
			if p.Query.Get("security") == "reality" {
				lines = append(lines,
					"    reality-opts:",
					"      public-key: "+p.Query.Get("pbk"),
					"      short-id: "+p.Query.Get("sid"),
				)
			}
		case "hysteria2":
			lines = append(lines,
				"  - name: "+strconv.Quote(name),
				"    type: hysteria2",
				"    server: "+p.Host,
				"    port: "+strconv.Itoa(p.Port),
				"    password: "+p.User,
				"    sni: "+p.Query.Get("sni"),
			)
		case "tuic":
			lines = append(lines,
				"  - name: "+strconv.Quote(name),
				"    type: tuic",
				"    server: "+p.Host,
				"    port: "+strconv.Itoa(p.Port),
				"    uuid: "+p.User,
				"    password: "+firstNonEmpty(p.Password, p.User),
				"    sni: "+p.Query.Get("sni"),
			)
		default:
			lines = append(lines,
				"  - name: "+strconv.Quote(name),
				"    type: vless",
				"    server: "+uri,
			)
		}
	}
	lines = append(lines,
		"proxy-groups:",
		"  - name: \"AUTO\"",
		"    type: select",
		"    proxies:",
	)
	for _, name := range proxyNames {
		lines = append(lines, "      - "+strconv.Quote(name))
	}
	lines = append(lines,
		"rules:",
		"  - MATCH,AUTO",
	)
	return strings.Join(lines, "\n")
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
