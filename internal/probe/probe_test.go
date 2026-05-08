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
		{"http bad scheme", KindHTTP, Payload{URL: "file:///etc/passwd"}},
		{"http bad method", KindHTTP, Payload{URL: "https://example.com", Method: "POST"}},
		{"http bad status", KindHTTP, Payload{URL: "https://example.com", ExpectStatus: []int{99}}},
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
		{KindPing, Payload{Target: "example.com", Count: 3, TimeoutMS: 3000}},
		{KindTCP, Payload{Target: "example.com", Port: 443, TimeoutMS: 3000}},
		{KindHTTP, Payload{URL: "https://example.com/healthz", Method: "GET", TimeoutMS: 3000, ExpectStatus: []int{200, 204}}},
	}
	for _, tc := range cases {
		if err := Validate(tc.kind, tc.p); err != nil {
			t.Fatalf("Validate(%s) err=%v", tc.kind, err)
		}
	}
}
