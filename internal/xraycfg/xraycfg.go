// Package xraycfg is the shared panel/agent contract for structured custom
// Xray config (custom outbounds + routing rules) shipped inside the managed
// xray_apply payload.
//
// The panel validates on save and renders a preview; the agent re-validates
// (it never trusts the panel) and merges the rendered objects into the final
// config after template-var expansion. Only an allowlisted, structured subset
// of Xray config is expressible here — never arbitrary JSON, shell commands,
// file paths (no ext:/regexp: rules), or key material.
package xraycfg

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// Marker is appended (on its own line) to the managed template whenever
// custom outbounds/routes are present. Agents that support custom config
// strip it before JSON validation; older agents render it into the config
// text and fail the apply with "rendered config is not valid json" — a loud,
// visible failure instead of silently applying a config with the custom
// routes missing.
const Marker = "/*neutrino:requires=custom-config-v1*/"

// Outbound is a structured custom outbound. Only these protocols are
// expressible; upstream addresses are plain host/IP (no schemes, no paths).
type Outbound struct {
	Tag      string `json:"tag"`
	Protocol string `json:"protocol"` // freedom | blackhole | socks | http
	// Address/Port are required for socks/http, forbidden otherwise.
	Address  string `json:"address,omitempty"`
	Port     int    `json:"port,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// DomainStrategy is freedom-only: AsIs | UseIP | UseIPv4 | UseIPv6.
	DomainStrategy string `json:"domain_strategy,omitempty"`
}

// Route is a structured routing rule (Xray "field" rule subset). At least one
// matcher must be set; OutboundTag must reference a built-in (direct/blocked)
// or a custom outbound defined alongside it.
type Route struct {
	OutboundTag string   `json:"outbound_tag"`
	Domains     []string `json:"domains,omitempty"`
	IPs         []string `json:"ips,omitempty"`
	Ports       string   `json:"ports,omitempty"`
	Network     string   `json:"network,omitempty"` // tcp | udp | tcp,udp
	Protocols   []string `json:"protocols,omitempty"`
}

const (
	maxOutbounds      = 16
	maxRoutes         = 64
	maxEntriesPerRule = 128
	maxEntryLen       = 256
	maxCredentialLen  = 128
	maxAddressLen     = 253
)

var (
	tagPattern      = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)
	portsPattern    = regexp.MustCompile(`^\d{1,5}(-\d{1,5})?(,\d{1,5}(-\d{1,5})?)*$`)
	hostPattern     = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
	domainPattern   = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
	keywordPattern  = regexp.MustCompile(`^[A-Za-z0-9._@!-]{1,128}$`)
	geoPattern      = regexp.MustCompile(`^[A-Za-z0-9._@!-]{1,64}$`)
	allowedNetworks = map[string]bool{"tcp": true, "udp": true, "tcp,udp": true, "udp,tcp": true}
	allowedSniffed  = map[string]bool{"http": true, "tls": true, "bittorrent": true, "quic": true}
)

// ReservedOutboundTags are outbound tags the managed template owns; custom
// outbounds may not redefine them, but routes may target direct/blocked.
var ReservedOutboundTags = map[string]bool{"api": true, "direct": true, "blocked": true}

// builtinRouteTargets are tags routes may reference without defining them.
var builtinRouteTargets = map[string]bool{"direct": true, "blocked": true}

// Validate checks the whole custom config as one unit (outbound tag
// uniqueness, route target resolution, every field against its allowlist).
func Validate(outbounds []Outbound, routes []Route) error {
	if len(outbounds) > maxOutbounds {
		return fmt.Errorf("too many custom outbounds: %d > %d", len(outbounds), maxOutbounds)
	}
	if len(routes) > maxRoutes {
		return fmt.Errorf("too many custom routes: %d > %d", len(routes), maxRoutes)
	}

	customTags := make(map[string]bool, len(outbounds))
	for i, o := range outbounds {
		if err := validateOutbound(o); err != nil {
			return fmt.Errorf("custom_outbounds[%d]: %w", i, err)
		}
		if customTags[o.Tag] {
			return fmt.Errorf("custom_outbounds[%d]: duplicate tag %q", i, o.Tag)
		}
		customTags[o.Tag] = true
	}

	for i, r := range routes {
		if err := validateRoute(r, customTags); err != nil {
			return fmt.Errorf("custom_routes[%d]: %w", i, err)
		}
	}
	return nil
}

func validateOutbound(o Outbound) error {
	if !tagPattern.MatchString(o.Tag) {
		return fmt.Errorf("tag must match %s", tagPattern.String())
	}
	if ReservedOutboundTags[o.Tag] {
		return fmt.Errorf("tag %q is reserved", o.Tag)
	}
	switch o.Protocol {
	case "freedom":
		if o.Address != "" || o.Port != 0 || o.Username != "" || o.Password != "" {
			return fmt.Errorf("freedom outbound takes no address/port/credentials")
		}
		switch o.DomainStrategy {
		case "", "AsIs", "UseIP", "UseIPv4", "UseIPv6":
		default:
			return fmt.Errorf("invalid domain_strategy %q", o.DomainStrategy)
		}
	case "blackhole":
		if o.Address != "" || o.Port != 0 || o.Username != "" || o.Password != "" || o.DomainStrategy != "" {
			return fmt.Errorf("blackhole outbound takes no settings")
		}
	case "socks", "http":
		if o.DomainStrategy != "" {
			return fmt.Errorf("domain_strategy is freedom-only")
		}
		if err := validateUpstreamHost(o.Address); err != nil {
			return err
		}
		if o.Port < 1 || o.Port > 65535 {
			return fmt.Errorf("port must be 1-65535")
		}
		if len(o.Username) > maxCredentialLen || len(o.Password) > maxCredentialLen {
			return fmt.Errorf("credentials too long (max %d)", maxCredentialLen)
		}
		if hasControlChars(o.Username) || hasControlChars(o.Password) {
			return fmt.Errorf("credentials must not contain control characters")
		}
		if o.Username == "" && o.Password != "" {
			return fmt.Errorf("password requires a username")
		}
	default:
		return fmt.Errorf("protocol must be one of freedom|blackhole|socks|http")
	}
	return nil
}

func validateUpstreamHost(addr string) error {
	if addr == "" {
		return fmt.Errorf("address is required")
	}
	if len(addr) > maxAddressLen {
		return fmt.Errorf("address too long")
	}
	if _, err := netip.ParseAddr(addr); err == nil {
		return nil
	}
	if !hostPattern.MatchString(addr) || strings.Contains(addr, "..") {
		return fmt.Errorf("address must be a hostname or IP (no scheme/path)")
	}
	return nil
}

func validateRoute(r Route, customTags map[string]bool) error {
	if !builtinRouteTargets[r.OutboundTag] && !customTags[r.OutboundTag] {
		return fmt.Errorf("outbound_tag %q is not direct/blocked or a defined custom outbound", r.OutboundTag)
	}
	if len(r.Domains) == 0 && len(r.IPs) == 0 && r.Ports == "" && r.Network == "" && len(r.Protocols) == 0 {
		return fmt.Errorf("route needs at least one matcher (domains/ips/ports/network/protocols)")
	}
	if len(r.Domains) > maxEntriesPerRule {
		return fmt.Errorf("too many domains: %d > %d", len(r.Domains), maxEntriesPerRule)
	}
	for i, d := range r.Domains {
		if err := validateDomainEntry(d); err != nil {
			return fmt.Errorf("domains[%d]: %w", i, err)
		}
	}
	if len(r.IPs) > maxEntriesPerRule {
		return fmt.Errorf("too many ips: %d > %d", len(r.IPs), maxEntriesPerRule)
	}
	for i, ip := range r.IPs {
		if err := validateIPEntry(ip); err != nil {
			return fmt.Errorf("ips[%d]: %w", i, err)
		}
	}
	if r.Ports != "" {
		if err := validatePorts(r.Ports); err != nil {
			return err
		}
	}
	if r.Network != "" && !allowedNetworks[r.Network] {
		return fmt.Errorf("network must be tcp, udp or tcp,udp")
	}
	for _, p := range r.Protocols {
		if !allowedSniffed[p] {
			return fmt.Errorf("protocol %q not allowed (http|tls|bittorrent|quic)", p)
		}
	}
	return nil
}

// validateDomainEntry allows plain domains and the domain:/full:/keyword:/
// geosite: prefixes. regexp: (DoS surface) and ext: (file path) are rejected
// by construction.
func validateDomainEntry(d string) error {
	if d == "" || len(d) > maxEntryLen {
		return fmt.Errorf("empty or too long")
	}
	prefix, rest := "", d
	if i := strings.Index(d, ":"); i >= 0 {
		prefix, rest = d[:i], d[i+1:]
	}
	switch prefix {
	case "":
		if !domainPattern.MatchString(rest) {
			return fmt.Errorf("invalid domain %q", d)
		}
	case "domain", "full":
		if !domainPattern.MatchString(rest) {
			return fmt.Errorf("invalid domain %q", d)
		}
	case "keyword":
		if !keywordPattern.MatchString(rest) {
			return fmt.Errorf("invalid keyword %q", d)
		}
	case "geosite":
		if !geoPattern.MatchString(rest) {
			return fmt.Errorf("invalid geosite category %q", d)
		}
	default:
		return fmt.Errorf("prefix %q not allowed (plain, domain:, full:, keyword:, geosite:)", prefix)
	}
	return nil
}

// validateIPEntry allows IPs, CIDRs and geoip: categories. ext: is rejected.
func validateIPEntry(s string) error {
	if s == "" || len(s) > maxEntryLen {
		return fmt.Errorf("empty or too long")
	}
	if rest, ok := strings.CutPrefix(s, "geoip:"); ok {
		if !geoPattern.MatchString(rest) {
			return fmt.Errorf("invalid geoip category %q", s)
		}
		return nil
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return nil
	}
	if _, err := netip.ParsePrefix(s); err == nil {
		return nil
	}
	return fmt.Errorf("invalid ip/cidr %q (ip, cidr or geoip:xx)", s)
}

func validatePorts(s string) error {
	if len(s) > maxEntryLen || !portsPattern.MatchString(s) {
		return fmt.Errorf("invalid ports %q (e.g. 443 or 1000-2000,8443)", s)
	}
	for _, part := range strings.Split(s, ",") {
		lo, hi, found := strings.Cut(part, "-")
		l, err := strconv.Atoi(lo)
		if err != nil || l < 1 || l > 65535 {
			return fmt.Errorf("port out of range in %q", part)
		}
		if found {
			h, err := strconv.Atoi(hi)
			if err != nil || h < 1 || h > 65535 || h < l {
				return fmt.Errorf("invalid port range %q", part)
			}
		}
	}
	return nil
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// RenderOutbound produces the Xray outbound object for a validated Outbound.
func RenderOutbound(o Outbound) map[string]any {
	out := map[string]any{"tag": o.Tag, "protocol": o.Protocol}
	switch o.Protocol {
	case "freedom":
		if o.DomainStrategy != "" {
			out["settings"] = map[string]any{"domainStrategy": o.DomainStrategy}
		}
	case "socks", "http":
		server := map[string]any{"address": o.Address, "port": o.Port}
		if o.Username != "" {
			server["users"] = []any{map[string]any{"user": o.Username, "pass": o.Password}}
		}
		out["settings"] = map[string]any{"servers": []any{server}}
	}
	return out
}

// RenderRouteRule produces the Xray routing rule object for a validated Route.
func RenderRouteRule(r Route) map[string]any {
	rule := map[string]any{"type": "field", "outboundTag": r.OutboundTag}
	if len(r.Domains) > 0 {
		rule["domain"] = toAnySlice(r.Domains)
	}
	if len(r.IPs) > 0 {
		rule["ip"] = toAnySlice(r.IPs)
	}
	if r.Ports != "" {
		rule["port"] = r.Ports
	}
	if r.Network != "" {
		rule["network"] = r.Network
	}
	if len(r.Protocols) > 0 {
		rule["protocol"] = toAnySlice(r.Protocols)
	}
	return rule
}

func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// StripMarker removes Marker lines from a rendered template and reports
// whether any were present.
func StripMarker(rendered string) (string, bool) {
	if !strings.Contains(rendered, Marker) {
		return rendered, false
	}
	lines := strings.Split(rendered, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if strings.TrimSpace(l) == Marker {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n"), true
}

// MergeIntoRendered validates and merges custom outbounds/routes into an
// already-rendered (post var-expansion, marker-stripped) config. Custom
// outbounds append after existing ones; custom rules insert directly after
// the last rule targeting the api outbound (so API traffic keeps priority)
// and before everything else, in input order.
func MergeIntoRendered(raw []byte, outbounds []Outbound, routes []Route) ([]byte, error) {
	if len(outbounds) == 0 && len(routes) == 0 {
		return raw, nil
	}
	if err := Validate(outbounds, routes); err != nil {
		return nil, err
	}

	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var cfg map[string]any
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config is not a json object: %w", err)
	}

	existingOut, err := asArray(cfg["outbounds"])
	if err != nil {
		return nil, fmt.Errorf("outbounds: %w", err)
	}
	existingTags := make(map[string]bool, len(existingOut))
	for _, e := range existingOut {
		if m, ok := e.(map[string]any); ok {
			if tag, ok := m["tag"].(string); ok && tag != "" {
				existingTags[tag] = true
			}
		}
	}
	for _, o := range outbounds {
		if existingTags[o.Tag] {
			return nil, fmt.Errorf("custom outbound tag %q already exists in config", o.Tag)
		}
		existingOut = append(existingOut, RenderOutbound(o))
	}
	cfg["outbounds"] = existingOut

	routing, ok := cfg["routing"].(map[string]any)
	if !ok {
		if cfg["routing"] != nil {
			return nil, fmt.Errorf("routing is not an object")
		}
		routing = map[string]any{}
	}
	rules, err := asArray(routing["rules"])
	if err != nil {
		return nil, fmt.Errorf("routing.rules: %w", err)
	}
	insertAt := 0
	for i, e := range rules {
		if m, ok := e.(map[string]any); ok {
			if tag, ok := m["outboundTag"].(string); ok && tag == "api" {
				insertAt = i + 1
			}
		}
	}
	customRules := make([]any, 0, len(routes))
	for _, r := range routes {
		customRules = append(customRules, RenderRouteRule(r))
	}
	merged := make([]any, 0, len(rules)+len(customRules))
	merged = append(merged, rules[:insertAt]...)
	merged = append(merged, customRules...)
	merged = append(merged, rules[insertAt:]...)
	routing["rules"] = merged
	cfg["routing"] = routing

	return json.MarshalIndent(cfg, "", "  ")
}

func asArray(v any) ([]any, error) {
	if v == nil {
		return []any{}, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("expected an array")
	}
	return arr, nil
}
