package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"neutrino/internal/config"
	"neutrino/internal/db"
	"neutrino/internal/repo"
)

func TestRequiredScope_ProtectsOpsNodes(t *testing.T) {
	if got := requiredScope("GET", "/api/v1/ops/nodes"); got != "nodes:read" {
		t.Fatalf("requiredScope(GET, /api/v1/ops/nodes)=%q, want %q", got, "nodes:read")
	}
}

func TestRequiredScope_ProtectsOpsAlerts(t *testing.T) {
	if got := requiredScope("GET", "/api/v1/ops/alerts"); got != "nodes:read" {
		t.Fatalf("requiredScope(GET, /api/v1/ops/alerts)=%q, want %q", got, "nodes:read")
	}
	if got := requiredScope("POST", "/api/v1/ops/alerts"); got != "nodes:write" {
		t.Fatalf("requiredScope(POST, /api/v1/ops/alerts)=%q, want %q", got, "nodes:write")
	}
}

func TestRequiredScope_ProtectsOpsConfig(t *testing.T) {
	if got := requiredScope("GET", "/api/v1/ops/config"); got != "nodes:read" {
		t.Fatalf("requiredScope(GET, /api/v1/ops/config)=%q, want %q", got, "nodes:read")
	}
}

func TestRequiredScope_ProtectsNodeMetrics(t *testing.T) {
	paths := []string{
		"/api/v1/nodes/1/metrics",
		"/api/v1/nodes/1/metric-details/latest",
		"/api/v1/nodes/1/static-facts/latest",
		"/api/v1/nodes/1/static-facts/history",
	}
	for _, path := range paths {
		if got := requiredScope("GET", path); got != "metrics:read" {
			t.Fatalf("requiredScope(GET, %s)=%q, want %q", path, got, "metrics:read")
		}
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

func TestAPIKeyNodesReadCannotWriteOpsOrNodeControl(t *testing.T) {
	ctx := context.Background()
	store := newAuthTestStore(t)
	node, err := store.CreateNode(ctx, repo.CreateNodeInput{
		Name:     "readonly-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	readKey, _, err := store.CreateAPIKey(ctx, "readonly", "nodes:read", nil, nil)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	a := newAuthTestApp(store)

	cases := []struct {
		method string
		path   string
		body   any
		want   int
	}{
		{http.MethodGet, "/api/v1/ops/nodes", nil, http.StatusOK},
		{http.MethodGet, "/api/v1/ops/alerts", nil, http.StatusOK},
		{http.MethodPost, "/api/v1/ops/alerts", map[string]any{"kind": "test", "severity": "warning", "message": "x", "dedupe_key": "readonly-test"}, http.StatusForbidden},
		{http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/probes", node.ID), map[string]any{"kind": "probe_http", "url": "https://example.com"}, http.StatusForbidden},
		{http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/metadata", node.ID), map[string]any{"provider": "test"}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := authJSONRequest(t, tc.method, tc.path, tc.body)
			req.Header.Set("X-API-Key", readKey)
			rr := httptest.NewRecorder()
			a.Routes().ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("got %d body=%s, want %d", rr.Code, rr.Body.String(), tc.want)
			}
		})
	}
}

func TestAdminSessionCanWriteOpsAlert(t *testing.T) {
	ctx := context.Background()
	store := newAuthTestStore(t)
	a := newAuthTestApp(store)
	sid, err := store.CreateAdminSession(ctx, "admin", time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	req := authJSONRequest(t, http.MethodPost, "/api/v1/ops/alerts", map[string]any{
		"kind":       "admin-test",
		"severity":   "warning",
		"message":    "admin can write",
		"dedupe_key": "admin-can-write",
	})
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	req.Header.Set(csrfHeaderName, a.computeCSRFToken(sid))
	rr := httptest.NewRecorder()
	a.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("got %d body=%s, want 201", rr.Code, rr.Body.String())
	}
}

func newAuthTestStore(t *testing.T) *repo.Store {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "auth-test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return repo.New(conn, config.Config{})
}

func newAuthTestApp(store *repo.Store) *App {
	return &App{
		cfg:        config.Config{APIKeyHeader: "X-API-Key"},
		store:      store,
		rl:         newRateLimiter(),
		csrfSecret: []byte("auth-test-csrf-secret-0123456789abcdef"),
	}
}

func authJSONRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	var rbody *bytes.Reader
	if body == nil {
		rbody = bytes.NewReader(nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rbody = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rbody)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}
