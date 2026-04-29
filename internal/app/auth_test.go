package app

import (
	"net/http/httptest"
	"testing"

	"neutrino/internal/config"
)

func TestRequiredScope_ProtectsOpsNodes(t *testing.T) {
	if got := requiredScope("GET", "/api/v1/ops/nodes"); got != "nodes:read" {
		t.Fatalf("requiredScope(GET, /api/v1/ops/nodes)=%q, want %q", got, "nodes:read")
	}
}

func TestRequestIsHTTPS(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com", nil)
	if requestIsHTTPS(req) {
		t.Fatalf("plain request unexpectedly detected as https")
	}

	req = httptest.NewRequest("GET", "http://example.com", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	if !requestIsHTTPS(req) {
		t.Fatalf("forwarded https request not detected")
	}
}

func TestClientIPFromRequestIgnoresForwardedForWithoutTrustedProxy(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com", nil)
	req.RemoteAddr = "203.0.113.10:54321"
	req.Header.Set("X-Forwarded-For", "198.51.100.99")

	a := &App{}
	if got := a.clientIPFromRequest(req); got != "203.0.113.10" {
		t.Fatalf("clientIPFromRequest=%q, want remote addr", got)
	}
}

func TestClientIPFromRequestTrustsForwardedForFromTrustedProxy(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Forwarded-For", "198.51.100.99, 10.0.0.5")

	a := &App{cfg: config.Config{TrustedProxyCIDRs: "127.0.0.1/32"}}
	if got := a.clientIPFromRequest(req); got != "198.51.100.99" {
		t.Fatalf("clientIPFromRequest=%q, want forwarded client ip", got)
	}
}
