package probe

import "testing"

func TestValidateRejectsUnsafeProbePayloads(t *testing.T) {
	cases := []struct {
		name string
		kind string
		p    Payload
	}{
		{"unknown kind", "shell", Payload{Target: "example.com"}},
		{"tcp missing port", KindTCP, Payload{Target: "example.com"}},
		{"tcp bad target", KindTCP, Payload{Target: "example.com/path", Port: 443}},
		{"tcp localhost", KindTCP, Payload{Target: "localhost", Port: 443}},
		{"tcp private ip", KindTCP, Payload{Target: "10.0.0.1", Port: 443}},
		{"tcp metadata ip", KindTCP, Payload{Target: "169.254.169.254", Port: 80}},
		{"http bad scheme", KindHTTP, Payload{URL: "file:///etc/passwd"}},
		{"http bad method", KindHTTP, Payload{URL: "https://example.com", Method: "POST"}},
		{"http bad status", KindHTTP, Payload{URL: "https://example.com", ExpectStatus: []int{99}}},
		{"http localhost", KindHTTP, Payload{URL: "http://localhost/healthz"}},
		{"http private ip", KindHTTP, Payload{URL: "http://192.168.1.10/healthz"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.kind, tc.p); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}
}

func TestValidateAcceptsBuiltInProbePayloads(t *testing.T) {
	cases := []struct {
		kind string
		p    Payload
	}{
		{KindDNS, Payload{Target: "example.com", Count: 3, TimeoutMS: 3000}},
		{KindPing, Payload{Target: "example.com", Count: 3, TimeoutMS: 3000}},
		{KindTCP, Payload{Target: "example.com", Port: 443, TimeoutMS: 3000}},
		{KindHTTP, Payload{URL: "https://example.com/healthz", Method: "GET", TimeoutMS: 3000, ExpectStatus: []int{200, 204}}},
		{KindTCP, Payload{Target: "10.0.0.1", Port: 443, TimeoutMS: 3000, AllowPrivate: true}},
	}
	for _, tc := range cases {
		if err := Validate(tc.kind, tc.p); err != nil {
			t.Fatalf("Validate(%s) err=%v", tc.kind, err)
		}
	}
}

func TestNormalizeProbePingToDNS(t *testing.T) {
	if got := NormalizeKind(KindPing); got != KindDNS {
		t.Fatalf("NormalizeKind(%q)=%q, want %q", KindPing, got, KindDNS)
	}
	if got := CorrelationID(KindPing, Payload{Target: "example.com"}); got != "probe_dns:example.com" {
		t.Fatalf("unexpected correlation id %q", got)
	}
}
