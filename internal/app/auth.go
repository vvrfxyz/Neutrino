package app

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const sessionCookieName = "neutrino_session"

type apiKeyContextKey struct{}

type adminIdentityContextKey struct{}

type adminIdentity struct {
	username string
	ok       bool
}

type APIKeyContext struct {
	KeyID  int64
	NodeID *int64
	Scopes map[string]struct{}
}

func (a *App) adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, _, ok := a.ensureAdminIdentity(r)
		if ok {
			next.ServeHTTP(w, r)
			return
		}

		if strings.HasPrefix(r.URL.Path, "/api/") {
			a.apiError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/login")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	})
}

// ensureAdminIdentity resolves the admin identity at most once per request and
// stashes it on the returned *http.Request's context. Callers MUST forward the
// returned *http.Request so downstream currentAdmin() reads hit the cache.
func (a *App) ensureAdminIdentity(r *http.Request) (*http.Request, string, bool) {
	if cached, ok := r.Context().Value(adminIdentityContextKey{}).(*adminIdentity); ok && cached != nil {
		return r, cached.username, cached.ok
	}
	username, ok := a.resolveAdmin(r)
	cache := &adminIdentity{username: username, ok: ok}
	r = r.WithContext(context.WithValue(r.Context(), adminIdentityContextKey{}, cache))
	return r, username, ok
}

func (a *App) currentAdmin(r *http.Request) (string, bool) {
	if cached, ok := r.Context().Value(adminIdentityContextKey{}).(*adminIdentity); ok && cached != nil {
		return cached.username, cached.ok
	}
	return a.resolveAdmin(r)
}

func (a *App) resolveAdmin(r *http.Request) (string, bool) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		if username, ok, _ := a.store.ValidateAdminSession(r.Context(), c.Value); ok {
			return username, true
		}
	}
	if a.cfg.BasicAuthAllowed() {
		user, pass, ok := r.BasicAuth()
		if !ok {
			return "", false
		}
		ip := a.clientIPFromRequest(r)
		now := time.Now()
		if !a.rl.Allow("basic:ip:"+ip, 60, time.Minute, now) ||
			!a.rl.Allow("basic:ipuser:"+ip+":"+user, 20, time.Minute, now) {
			return "", false
		}
		if a.store.AuthenticateAdmin(r.Context(), user, pass) == nil {
			return user, true
		}
	}
	return "", false
}

func (a *App) apiV1Auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Enrollment endpoint is authenticated by an enroll_code in the request body (not by admin/api key/mTLS).
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/nodes/") && strings.HasSuffix(r.URL.Path, "/enroll") {
			ip := a.clientIPFromRequest(r)
			if !a.rl.Allow("enroll:ip:"+ip, 60, time.Minute, time.Now()) {
				a.apiError(w, http.StatusTooManyRequests, "too many requests")
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if r2, _, ok := a.ensureAdminIdentity(r); ok {
			if !a.apiV1SessionCSRFGate(r2) {
				a.apiError(w, http.StatusForbidden, "invalid csrf token")
				return
			}
			next.ServeHTTP(w, r2)
			return
		}

		// Agent -> panel: mTLS node identity. This is the only supported auth for node-agent.
		if nodeID, ok := nodeIDFromMTLS(r); ok {
			if !a.rl.Allow("node:"+strconv.FormatInt(nodeID, 10), 20000, time.Minute, time.Now()) {
				a.apiError(w, http.StatusTooManyRequests, "too many requests")
				return
			}
			node, err := a.store.GetNode(r.Context(), nodeID)
			if err != nil {
				a.apiError(w, http.StatusForbidden, "forbidden")
				return
			}
			if !node.Enabled && !allowDisabledNodeControlPlane(r.Method, r.URL.Path) {
				a.apiError(w, http.StatusForbidden, "forbidden")
				return
			}
			// Enforce node cert allowlist / revocation.
			cert := r.TLS.PeerCertificates[0]
			if err := a.store.ValidateNodeMTLSCert(r.Context(), nodeID, cert, time.Now().UTC()); err != nil {
				a.apiError(w, http.StatusForbidden, "forbidden")
				return
			}

			scopes := map[string]struct{}{
				"nodes:report": {},
				"usage:write":  {},
			}
			scope := requiredScope(r.Method, r.URL.Path)
			if scope != "" && !hasScope(scopes, scope) {
				a.apiError(w, http.StatusForbidden, "forbidden")
				return
			}
			ctx := context.WithValue(r.Context(), apiKeyContextKey{}, &APIKeyContext{
				KeyID:  0,
				NodeID: &nodeID,
				Scopes: scopes,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		if strings.HasPrefix(r.URL.Path, "/api/v1/usage") {
			a.apiError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		rawKey := strings.TrimSpace(r.Header.Get(a.cfg.APIKeyHeader))
		auth, ok, err := a.store.ValidateAPIKey(r.Context(), rawKey)
		if err != nil {
			a.apiError(w, http.StatusInternalServerError, "auth failed")
			return
		}
		if !ok {
			a.apiError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if auth.NodeID != nil {
			node, nodeErr := a.store.GetNode(r.Context(), *auth.NodeID)
			if nodeErr != nil || !node.Enabled {
				a.apiError(w, http.StatusForbidden, "forbidden")
				return
			}
		}
		if !a.rl.Allow("key:"+strconv.FormatInt(auth.ID, 10), 10000, time.Minute, time.Now()) {
			a.apiError(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		scope := requiredScope(r.Method, r.URL.Path)
		if scope != "" && !hasScope(auth.Scopes, scope) {
			a.apiError(w, http.StatusForbidden, "forbidden")
			return
		}
		ctx := context.WithValue(r.Context(), apiKeyContextKey{}, &APIKeyContext{
			KeyID:  auth.ID,
			NodeID: auth.NodeID,
			Scopes: auth.Scopes,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// apiV1AuthMTLSOnly enforces node-agent auth strictly via mTLS (no admin, no API key).
func (a *App) apiV1AuthMTLSOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodeID, ok := nodeIDFromMTLS(r)
		if !ok {
			a.apiError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !a.rl.Allow("node:"+strconv.FormatInt(nodeID, 10), 20000, time.Minute, time.Now()) {
			a.apiError(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		node, err := a.store.GetNode(r.Context(), nodeID)
		if err != nil {
			a.apiError(w, http.StatusForbidden, "forbidden")
			return
		}
		if !node.Enabled && !allowDisabledNodeControlPlane(r.Method, r.URL.Path) {
			a.apiError(w, http.StatusForbidden, "forbidden")
			return
		}
		// Enforce node cert allowlist / revocation.
		cert := r.TLS.PeerCertificates[0]
		if err := a.store.ValidateNodeMTLSCert(r.Context(), nodeID, cert, time.Now().UTC()); err != nil {
			a.apiError(w, http.StatusForbidden, "forbidden")
			return
		}

		scopes := map[string]struct{}{
			"nodes:report": {},
			"usage:write":  {},
		}
		scope := requiredScope(r.Method, r.URL.Path)
		if scope != "" && !hasScope(scopes, scope) {
			a.apiError(w, http.StatusForbidden, "forbidden")
			return
		}
		ctx := context.WithValue(r.Context(), apiKeyContextKey{}, &APIKeyContext{
			KeyID:  0,
			NodeID: &nodeID,
			Scopes: scopes,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func nodeIDFromMTLS(r *http.Request) (int64, bool) {
	if r == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return 0, false
	}
	cn := strings.TrimSpace(r.TLS.PeerCertificates[0].Subject.CommonName)
	cn = strings.TrimPrefix(cn, "node-")
	if cn == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(cn, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func getAPIKeyContext(ctx context.Context) *APIKeyContext {
	if v, ok := ctx.Value(apiKeyContextKey{}).(*APIKeyContext); ok {
		return v
	}
	return nil
}

func requiredScope(method, path string) string {
	path = strings.TrimSpace(path)
	switch {
	// Agent report endpoints (agent -> panel).
	case strings.HasPrefix(path, "/api/v1/nodes/") && strings.HasSuffix(path, "/report") && method == http.MethodPost:
		return "nodes:report"
	case strings.HasPrefix(path, "/api/v1/nodes/") && strings.Contains(path, "/agent/users") && method == http.MethodGet:
		return "nodes:report"
	case strings.HasPrefix(path, "/api/v1/nodes/") && strings.Contains(path, "/jobs/") && method == http.MethodPost:
		return "nodes:report"
	case strings.HasPrefix(path, "/api/v1/nodes/") && strings.HasSuffix(path, "/jobs/claim") && method == http.MethodPost:
		return "nodes:report"
	case strings.HasPrefix(path, "/api/v1/nodes/") && strings.HasSuffix(path, "/cert/renew") && method == http.MethodPost:
		return "nodes:report"
	case strings.HasPrefix(path, "/api/v1/usage"):
		return "usage:write"
	case strings.HasPrefix(path, "/api/v1/traffic"):
		return "traffic:read"
	case strings.HasPrefix(path, "/api/v1/ops/alerts"):
		if method == http.MethodGet {
			return "nodes:read"
		}
		return "nodes:write"
	case strings.HasPrefix(path, "/api/v1/ops/nodes"), strings.HasPrefix(path, "/api/v1/ops/config"):
		return "nodes:read"
	case strings.HasPrefix(path, "/api/v1/nodes/") && method == http.MethodGet &&
		(strings.Contains(path, "/metrics") || strings.Contains(path, "/metric-details") || strings.Contains(path, "/static-facts")):
		return "metrics:read"
	case strings.HasPrefix(path, "/api/v1/users"):
		if method == http.MethodGet {
			return "users:read"
		}
		return "users:write"
	case strings.HasPrefix(path, "/api/v1/nodes"):
		if method == http.MethodGet {
			return "nodes:read"
		}
		return "nodes:write"
	case strings.HasPrefix(path, "/api/v1/online-users"):
		return "online:read"
	case strings.HasPrefix(path, "/api/v1/metrics"):
		return "metrics:read"
	case strings.HasPrefix(path, "/api/v1/backups"):
		if method == http.MethodGet {
			return "backups:read"
		}
		return "backups:write"
	// Key management is admin-only: a narrowly-scoped key must never be able
	// to mint or revoke keys (privilege escalation). Only "admin"/"*" pass.
	case strings.HasPrefix(path, "/api/v1/apikeys"):
		return "admin"
	default:
		return ""
	}
}

func allowDisabledNodeControlPlane(method, path string) bool {
	path = strings.TrimSpace(path)
	switch {
	case strings.HasPrefix(path, "/api/v1/nodes/") && strings.Contains(path, "/agent/users") && method == http.MethodGet:
		return true
	case strings.HasPrefix(path, "/api/v1/nodes/") && strings.HasSuffix(path, "/jobs/claim") && method == http.MethodPost:
		return true
	case strings.HasPrefix(path, "/api/v1/nodes/") && strings.Contains(path, "/jobs/") && strings.HasSuffix(path, "/finish") && method == http.MethodPost:
		return true
	default:
		return false
	}
}

func hasScope(scopes map[string]struct{}, needed string) bool {
	if needed == "" {
		return true
	}
	if _, ok := scopes["*"]; ok {
		return true
	}
	if _, ok := scopes["admin"]; ok {
		return true
	}
	_, ok := scopes[needed]
	return ok
}
