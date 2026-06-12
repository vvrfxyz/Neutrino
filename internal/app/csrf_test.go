package app

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"neutrino/internal/config"
)

type csrfTestSink struct {
	called bool
}

func (s *csrfTestSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.called = true
	w.WriteHeader(http.StatusOK)
}

func newCSRFTestApp() *App {
	return &App{
		cfg:        config.Config{APIKeyHeader: "X-API-Key"},
		csrfSecret: []byte("test-csrf-secret-0123456789abcdef"),
	}
}

func TestCSRFSafeMethodsBypass(t *testing.T) {
	a := newCSRFTestApp()
	sink := &csrfTestSink{}
	handler := a.csrfProtect(sink)

	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		req := httptest.NewRequest(m, "/users", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200", m, rr.Code)
		}
	}
}

func TestCSRFRejectsMissingToken(t *testing.T) {
	a := newCSRFTestApp()
	sink := &csrfTestSink{}
	handler := a.csrfProtect(sink)

	req := httptest.NewRequest(http.MethodPost, "/users/1/links", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "sid-abc"})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rr.Code)
	}
	if sink.called {
		t.Fatalf("handler should not be invoked")
	}
}

func TestCSRFRejectsBadToken(t *testing.T) {
	a := newCSRFTestApp()
	sink := &csrfTestSink{}
	handler := a.csrfProtect(sink)

	req := httptest.NewRequest(http.MethodPost, "/users/1/links", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "sid-abc"})
	req.Header.Set(csrfHeaderName, "not-the-token")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rr.Code)
	}
}

func TestCSRFAcceptsHeaderToken(t *testing.T) {
	a := newCSRFTestApp()
	sink := &csrfTestSink{}
	handler := a.csrfProtect(sink)

	sid := "sid-xyz"
	token := a.computeCSRFToken(sid)
	if token == "" {
		t.Fatalf("empty csrf token")
	}

	req := httptest.NewRequest(http.MethodPost, "/users/1/links", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	req.Header.Set(csrfHeaderName, token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rr.Code)
	}
	if !sink.called {
		t.Fatalf("handler should be invoked")
	}
}

func TestCSRFAcceptsFormToken(t *testing.T) {
	a := newCSRFTestApp()
	sink := &csrfTestSink{}
	handler := a.csrfProtect(sink)

	sid := "sid-form"
	token := a.computeCSRFToken(sid)
	form := url.Values{}
	form.Set("csrf_token", token)

	req := httptest.NewRequest(http.MethodPost, "/users/1/links", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestCSRFSkipsAPIKey(t *testing.T) {
	a := newCSRFTestApp()
	sink := &csrfTestSink{}
	handler := a.csrfProtect(sink)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/users", nil)
	req.Header.Set("X-API-Key", "pre-provisioned-key")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rr.Code)
	}
}

func TestCSRFSkipsMTLS(t *testing.T) {
	a := newCSRFTestApp()
	sink := &csrfTestSink{}
	handler := a.csrfProtect(sink)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/usage", nil)
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{}},
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rr.Code)
	}
}

// Basic auth bypasses the CSRF token (no session, no token to bind to), but
// must not bypass it for provably cross-site browser requests: browsers
// attach cached Basic credentials to cross-site form posts.
func TestCSRFBasicAuthCrossSiteRules(t *testing.T) {
	a := newCSRFTestApp()

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no browser headers (curl)", nil, http.StatusOK},
		{"same-origin fetch", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"matching origin", map[string]string{"Origin": "http://example.com"}, http.StatusOK},
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"foreign origin", map[string]string{"Origin": "https://evil.test"}, http.StatusForbidden},
		{"null origin", map[string]string{"Origin": "null"}, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sink := &csrfTestSink{}
			handler := a.csrfProtect(sink)
			req := httptest.NewRequest(http.MethodPost, "http://example.com/users", nil)
			req.SetBasicAuth("admin", "pass")
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != c.want {
				t.Fatalf("got %d, want %d", rr.Code, c.want)
			}

			// The /api/v1 session gate must agree with csrfProtect.
			if gateAllows := a.apiV1SessionCSRFGate(req); gateAllows != (c.want == http.StatusOK) {
				t.Fatalf("apiV1SessionCSRFGate = %v, want %v", gateAllows, c.want == http.StatusOK)
			}
		})
	}
}
