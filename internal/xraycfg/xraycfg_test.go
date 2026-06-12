package xraycfg

import (
	"encoding/json"
	"strings"
	"testing"
)

func validOutbounds() []Outbound {
	return []Outbound{
		{Tag: "us-proxy", Protocol: "socks", Address: "10.0.0.2", Port: 1080, Username: "u", Password: "p"},
		{Tag: "direct2", Protocol: "freedom", DomainStrategy: "UseIP"},
		{Tag: "sinkhole", Protocol: "blackhole"},
	}
}

func validRoutes() []Route {
	return []Route{
		{OutboundTag: "us-proxy", Domains: []string{"geosite:openai", "domain:example.com", "full:api.example.com", "keyword:tracker"}},
		{OutboundTag: "blocked", IPs: []string{"203.0.113.0/24", "geoip:private", "2001:db8::1"}},
		{OutboundTag: "direct", Ports: "443,1000-2000", Network: "tcp"},
		{OutboundTag: "sinkhole", Protocols: []string{"bittorrent"}},
	}
}

func TestValidateAccepts(t *testing.T) {
	if err := Validate(validOutbounds(), validRoutes()); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestValidateRejections(t *testing.T) {
	cases := []struct {
		name string
		outs []Outbound
		rts  []Route
		want string
	}{
		{"reserved tag", []Outbound{{Tag: "direct", Protocol: "freedom"}}, nil, "reserved"},
		{"bad tag chars", []Outbound{{Tag: "a b", Protocol: "freedom"}}, nil, "tag must match"},
		{"dup tag", []Outbound{{Tag: "x", Protocol: "freedom"}, {Tag: "x", Protocol: "blackhole"}}, nil, "duplicate tag"},
		{"unknown protocol", []Outbound{{Tag: "x", Protocol: "vmess"}}, nil, "protocol must be"},
		{"socks no addr", []Outbound{{Tag: "x", Protocol: "socks", Port: 1080}}, nil, "address is required"},
		{"socks bad port", []Outbound{{Tag: "x", Protocol: "socks", Address: "h", Port: 70000}}, nil, "port must be"},
		{"addr with scheme", []Outbound{{Tag: "x", Protocol: "http", Address: "http://h", Port: 80}}, nil, "hostname or IP"},
		{"freedom with addr", []Outbound{{Tag: "x", Protocol: "freedom", Address: "h"}}, nil, "no address"},
		{"bad domain strategy", []Outbound{{Tag: "x", Protocol: "freedom", DomainStrategy: "Wat"}}, nil, "domain_strategy"},
		{"route unknown target", nil, []Route{{OutboundTag: "ghost", Ports: "443"}}, "not direct/blocked"},
		{"route no matcher", nil, []Route{{OutboundTag: "direct"}}, "at least one matcher"},
		{"route ext file path", nil, []Route{{OutboundTag: "direct", Domains: []string{"ext:geosite.dat:cn"}}}, "not allowed"},
		{"route regexp", nil, []Route{{OutboundTag: "direct", Domains: []string{"regexp:.*\\.cn$"}}}, "not allowed"},
		{"route bad ip", nil, []Route{{OutboundTag: "direct", IPs: []string{"not-an-ip"}}}, "invalid ip"},
		{"route ext ip", nil, []Route{{OutboundTag: "direct", IPs: []string{"ext:geoip.dat:cn"}}}, "invalid ip"},
		{"route bad ports", nil, []Route{{OutboundTag: "direct", Ports: "443;1080"}}, "invalid ports"},
		{"route inverted range", nil, []Route{{OutboundTag: "direct", Ports: "2000-1000"}}, "invalid port range"},
		{"route bad network", nil, []Route{{OutboundTag: "direct", Network: "icmp"}}, "network must be"},
		{"route bad protocol", nil, []Route{{OutboundTag: "direct", Protocols: []string{"ssh"}}}, "not allowed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.outs, c.rts)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}

func TestRouteMayTargetCustomOutbound(t *testing.T) {
	outs := []Outbound{{Tag: "up", Protocol: "http", Address: "proxy.internal", Port: 8080}}
	rts := []Route{{OutboundTag: "up", Domains: []string{"example.com"}}}
	if err := Validate(outs, rts); err != nil {
		t.Fatalf("route to custom outbound rejected: %v", err)
	}
}

const mergeBase = `{
  "log": {"loglevel": "warning"},
  "inbounds": [{"tag": "vless-reality", "port": 443}],
  "outbounds": [
    {"protocol": "freedom", "tag": "direct"},
    {"protocol": "blackhole", "tag": "blocked"}
  ],
  "routing": {
    "rules": [
      {"type": "field", "inboundTag": ["api-in"], "outboundTag": "api"},
      {"type": "field", "ip": ["geoip:private"], "outboundTag": "blocked"}
    ]
  }
}`

func TestMergeIntoRendered(t *testing.T) {
	outs := []Outbound{{Tag: "up", Protocol: "socks", Address: "10.0.0.2", Port: 1080}}
	rts := []Route{{OutboundTag: "up", Domains: []string{"geosite:openai"}}}

	merged, err := MergeIntoRendered([]byte(mergeBase), outs, rts)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(merged, &cfg); err != nil {
		t.Fatalf("merged config not valid json: %v", err)
	}

	outArr := cfg["outbounds"].([]any)
	if len(outArr) != 3 {
		t.Fatalf("expected 3 outbounds, got %d", len(outArr))
	}
	last := outArr[2].(map[string]any)
	if last["tag"] != "up" || last["protocol"] != "socks" {
		t.Fatalf("custom outbound not appended: %+v", last)
	}

	rules := cfg["routing"].(map[string]any)["rules"].([]any)
	if len(rules) != 3 {
		t.Fatalf("expected 3 rules, got %d", len(rules))
	}
	// Custom rule must land after the api rule, before the pre-existing rule.
	if tag := rules[0].(map[string]any)["outboundTag"]; tag != "api" {
		t.Fatalf("api rule no longer first: %v", tag)
	}
	if tag := rules[1].(map[string]any)["outboundTag"]; tag != "up" {
		t.Fatalf("custom rule not inserted after api rule: %v", tag)
	}
	if tag := rules[2].(map[string]any)["outboundTag"]; tag != "blocked" {
		t.Fatalf("existing rule displaced: %v", tag)
	}

	// Numbers must survive the round-trip un-mangled.
	if !strings.Contains(string(merged), `"port": 443`) {
		t.Fatalf("inbound port mangled:\n%s", merged)
	}
}

func TestMergeRejectsTagCollisionWithConfig(t *testing.T) {
	outs := []Outbound{{Tag: "vless-reality2", Protocol: "freedom"}}
	base := `{"outbounds": [{"protocol": "freedom", "tag": "vless-reality2"}], "routing": {"rules": []}}`
	if _, err := MergeIntoRendered([]byte(base), outs, nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected tag collision error, got %v", err)
	}
}

func TestMergeNoCustomsIsIdentity(t *testing.T) {
	got, err := MergeIntoRendered([]byte(mergeBase), nil, nil)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if string(got) != mergeBase {
		t.Fatalf("identity merge changed bytes")
	}
}

func TestMergeCreatesMissingSections(t *testing.T) {
	outs := []Outbound{{Tag: "up", Protocol: "freedom"}}
	rts := []Route{{OutboundTag: "up", Ports: "8443"}}
	merged, err := MergeIntoRendered([]byte(`{"log": {}}`), outs, rts)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(merged, &cfg); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(cfg["outbounds"].([]any)) != 1 {
		t.Fatalf("outbounds not created")
	}
	if len(cfg["routing"].(map[string]any)["rules"].([]any)) != 1 {
		t.Fatalf("routing.rules not created")
	}
}

func TestStripMarker(t *testing.T) {
	in := "{\n  \"log\": {}\n}\n" + Marker + "\n"
	out, found := StripMarker(in)
	if !found {
		t.Fatalf("marker not detected")
	}
	if strings.Contains(out, Marker) {
		t.Fatalf("marker not stripped")
	}
	if !json.Valid([]byte(out)) {
		t.Fatalf("stripped output not valid json:\n%q", out)
	}

	// Without support (old agent), the marker makes the render invalid JSON —
	// the loud-failure contract this package relies on.
	if json.Valid([]byte(in)) {
		t.Fatalf("marker unexpectedly yields valid json; old agents would silently drop customs")
	}

	out2, found2 := StripMarker("{}")
	if found2 || out2 != "{}" {
		t.Fatalf("strip on marker-less input changed it")
	}
}
