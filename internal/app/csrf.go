package app

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
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
		return true
	}
	if _, _, ok := r.BasicAuth(); ok {
		return true
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
