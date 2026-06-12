package app

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	csrfCookieName = "neutrino_csrf"
	csrfHeaderName = "X-CSRF-Token"
	csrfFormField  = "csrf_token"
)

func generateCSRFSecret() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		b = []byte("neutrino-csrf-fallback-secret-v1")
	}
	return b
}

func (a *App) computeCSRFToken(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" || len(a.csrfSecret) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, a.csrfSecret)
	mac.Write([]byte(sessionID))
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *App) csrfTokenFor(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c == nil {
		return ""
	}
	return a.computeCSRFToken(c.Value)
}

func (a *App) issueCSRFCookie(w http.ResponseWriter, r *http.Request, sessionID string) {
	token := a.computeCSRFToken(sessionID)
	if token == "" {
		return
	}
	ttl := time.Duration(a.cfg.AdminSessionHours) * time.Hour
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: false,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(ttl),
	})
}

func (a *App) expireCSRFCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: false,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func csrfSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

func requestHasMTLSPeer(r *http.Request) bool {
	return r != nil && r.TLS != nil && len(r.TLS.PeerCertificates) > 0
}

func requestHasAPIKeyHeader(r *http.Request, header string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	return strings.TrimSpace(r.Header.Get(header)) != ""
}

// crossSiteBrowserRequest reports whether an unsafe request provably comes
// from another site in a browser. Browsers attach cached Basic credentials to
// cross-site form posts, so Basic auth alone must not bypass CSRF for them.
// Non-browser clients (curl, SDKs) send neither Sec-Fetch-Site nor Origin and
// are never flagged, keeping ALLOW_BASIC_AUTH automation working.
func crossSiteBrowserRequest(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))) {
	case "cross-site":
		return true
	case "same-origin", "same-site", "none":
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	// "null" (sandboxed iframe, some redirect chains) is exactly CSRF-shaped
	// for an authenticated write.
	if strings.EqualFold(origin, "null") {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return true
	}
	return !strings.EqualFold(u.Host, r.Host)
}

func (a *App) csrfProtect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if csrfSafeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		if requestHasMTLSPeer(r) {
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(sessionCookieName)
		hasSession := err == nil && c != nil && strings.TrimSpace(c.Value) != ""
		if !hasSession {
			if requestHasAPIKeyHeader(r, a.cfg.APIKeyHeader) {
				next.ServeHTTP(w, r)
				return
			}
			if _, _, ok := r.BasicAuth(); ok {
				if crossSiteBrowserRequest(r) {
					http.Error(w, "cross-site request rejected", http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "missing csrf token", http.StatusForbidden)
			return
		}
		expected := a.computeCSRFToken(c.Value)
		if expected == "" {
			http.Error(w, "missing csrf token", http.StatusForbidden)
			return
		}
		provided := strings.TrimSpace(r.Header.Get(csrfHeaderName))
		if provided == "" {
			if err := r.ParseForm(); err == nil {
				provided = strings.TrimSpace(r.FormValue(csrfFormField))
			}
		}
		if provided == "" || !hmac.Equal([]byte(provided), []byte(expected)) {
			http.Error(w, "invalid csrf token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) apiV1SessionCSRFGate(r *http.Request) bool {
	if csrfSafeMethod(r.Method) {
		return true
	}
	if requestHasMTLSPeer(r) {
		return true
	}
	if requestHasAPIKeyHeader(r, a.cfg.APIKeyHeader) {
		return true
	}
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c == nil || strings.TrimSpace(c.Value) == "" {
		// No session cookie: the request authenticated via Basic. Allow
		// non-browser automation, but never a provably cross-site browser.
		return !crossSiteBrowserRequest(r)
	}
	if _, _, ok := r.BasicAuth(); ok {
		return !crossSiteBrowserRequest(r)
	}
	expected := a.computeCSRFToken(c.Value)
	if expected == "" {
		return false
	}
	provided := strings.TrimSpace(r.Header.Get(csrfHeaderName))
	if provided == "" {
		if err := r.ParseForm(); err == nil {
			provided = strings.TrimSpace(r.FormValue(csrfFormField))
		}
	}
	if provided == "" {
		return false
	}
	return hmac.Equal([]byte(provided), []byte(expected))
}
