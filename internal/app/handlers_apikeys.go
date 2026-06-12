package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"neutrino/internal/repo"
	"neutrino/internal/service"
)

// API key lifecycle endpoints.
//
//	GET    /api/v1/apikeys        — list key metadata (never plaintext/hash)
//	POST   /api/v1/apikeys        — create; the plaintext key appears in this response only
//	GET    /api/v1/apikeys/{id}   — one key's metadata
//	DELETE /api/v1/apikeys/{id}   — revoke (idempotent)
//
// requiredScope maps /api/v1/apikeys to the "admin" scope, so admin sessions
// and only admin/* scoped keys can manage keys — a narrow key must not be able
// to mint itself a wider one.

func (a *App) handleAPIKeysV1(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		items, err := a.apikeys().List(r.Context(), limit)
		if err != nil {
			a.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "list failed"})
			return
		}
		a.writeJSON(w, http.StatusOK, map[string]any{"count": len(items), "items": items})
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		var req struct {
			Name      string   `json:"name"`
			Scopes    []string `json:"scopes"`
			NodeID    *int64   `json:"node_id,omitempty"`
			ExpiresAt string   `json:"expires_at,omitempty"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			a.apiError(w, http.StatusBadRequest, "invalid json")
			return
		}
		in := service.CreateAPIKeyInput{
			Name:   req.Name,
			Scopes: req.Scopes,
			NodeID: req.NodeID,
		}
		if strings.TrimSpace(req.ExpiresAt) != "" {
			t, err := time.Parse(time.RFC3339, strings.TrimSpace(req.ExpiresAt))
			if err != nil {
				a.apiError(w, http.StatusBadRequest, "expires_at must be RFC3339")
				return
			}
			in.ExpiresAt = &t
		}
		plain, meta, err := a.apikeys().Create(r.Context(), in)
		if err != nil {
			switch {
			case errors.Is(err, repo.ErrNodeNotFound):
				a.apiError(w, http.StatusNotFound, err.Error())
			case errors.Is(err, service.ErrAPIKeyInvalidScope), errors.Is(err, service.ErrAPIKeyInvalidInput):
				a.apiError(w, http.StatusBadRequest, err.Error())
			default:
				a.apiError(w, http.StatusInternalServerError, "create failed")
			}
			return
		}
		auditAction(a, r, "apikey.create", "api_key", strconv.FormatInt(meta.ID, 10), map[string]any{
			"name":    meta.Name,
			"scopes":  meta.Scopes,
			"node_id": meta.NodeID,
		})
		// The one response that ever carries the plaintext — keep it out of
		// shared caches and the browser bfcache.
		w.Header().Set("Cache-Control", "no-store")
		a.writeJSON(w, http.StatusCreated, map[string]any{
			"key":  plain, // shown exactly once; only the hash is stored
			"item": meta,
		})
	default:
		a.apiError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *App) handleAPIKeyByIDV1(w http.ResponseWriter, r *http.Request, id int64) {
	switch r.Method {
	case http.MethodGet:
		item, err := a.apikeys().Get(r.Context(), id)
		if err != nil {
			if errors.Is(err, service.ErrAPIKeyNotFound) {
				a.apiError(w, http.StatusNotFound, "not found")
				return
			}
			a.apiError(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		a.writeJSON(w, http.StatusOK, item)
	case http.MethodDelete:
		item, err := a.apikeys().Revoke(r.Context(), id)
		if err != nil {
			if errors.Is(err, service.ErrAPIKeyNotFound) {
				a.apiError(w, http.StatusNotFound, "not found")
				return
			}
			a.apiError(w, http.StatusInternalServerError, "revoke failed")
			return
		}
		auditAction(a, r, "apikey.revoke", "api_key", strconv.FormatInt(id, 10), map[string]any{
			"name": item.Name,
		})
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "item": item})
	default:
		a.apiError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// SSR admin page: /apikeys (list + create form), /apikeys/{id}/revoke.

// oneTimePlainKeys hands the just-created plaintext key from the create POST
// to the redirected GET exactly once (PRG: refreshing after create must
// re-render the page, not re-submit the form and mint another key).
type oneTimePlainKeys struct {
	mu      sync.Mutex
	entries map[string]oneTimePlainKey
}

type oneTimePlainKey struct {
	plainKey  string
	sessionID string
	expiresAt time.Time
}

const plainKeyStashTTL = 2 * time.Minute

func (s *oneTimePlainKeys) put(sessionID, plainKey string) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(buf)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]oneTimePlainKey)
	}
	for k, e := range s.entries {
		if now.After(e.expiresAt) {
			delete(s.entries, k)
		}
	}
	s.entries[nonce] = oneTimePlainKey{
		plainKey:  plainKey,
		sessionID: sessionID,
		expiresAt: now.Add(plainKeyStashTTL),
	}
	return nonce, nil
}

func (s *oneTimePlainKeys) pop(nonce, sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[nonce]
	if !ok {
		return ""
	}
	delete(s.entries, nonce)
	if time.Now().After(e.expiresAt) || e.sessionID != sessionID {
		return ""
	}
	return e.plainKey
}

func sessionIDFromRequest(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c == nil {
		return ""
	}
	return c.Value
}

type apikeysPageData struct {
	Items       []repo.APIKey
	ValidScopes []string
	PlainKey    string
	Error       string
}

func (a *App) renderAPIKeysPage(w http.ResponseWriter, r *http.Request, plainKey, errMsg string) {
	items, err := a.apikeys().List(r.Context(), 0)
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	if plainKey != "" {
		// The one page render that ever carries the plaintext.
		w.Header().Set("Cache-Control", "no-store")
	}
	current, _ := a.currentAdmin(r)
	data := PageData{
		CurrentAdmin: current,
		ActivePage:   "apikeys",
		CSRFToken:    a.csrfTokenFor(r),
		Content: apikeysPageData{
			Items:       items,
			ValidScopes: service.KnownAPIKeyScopes,
			PlainKey:    plainKey,
			Error:       errMsg,
		},
	}
	if err := a.pages["apikeys"].ExecuteTemplate(w, "base.tmpl", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) handleAPIKeysPage(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		plain := ""
		if nonce := strings.TrimSpace(r.URL.Query().Get("new")); nonce != "" {
			plain = a.apiKeyPlainStash.pop(nonce, sessionIDFromRequest(r))
		}
		a.renderAPIKeysPage(w, r, plain, "")
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			a.renderAPIKeysPage(w, r, "", "表单解析失败")
			return
		}
		in := service.CreateAPIKeyInput{
			Name:   strings.TrimSpace(r.FormValue("name")),
			Scopes: r.Form["scopes"],
		}
		if v := strings.TrimSpace(r.FormValue("expires_days")); v != "" {
			days, err := strconv.Atoi(v)
			if err != nil || days <= 0 {
				a.renderAPIKeysPage(w, r, "", "有效期必须是正整数天数")
				return
			}
			t := time.Now().UTC().AddDate(0, 0, days)
			in.ExpiresAt = &t
		}
		plain, meta, err := a.apikeys().Create(r.Context(), in)
		if err != nil {
			// Show validation messages; mask anything else (DB/driver error
			// text must not reach the page, mirroring the JSON handler).
			msg := "创建失败"
			if errors.Is(err, service.ErrAPIKeyInvalidInput) {
				msg = "创建失败: " + err.Error()
			}
			a.renderAPIKeysPage(w, r, "", msg)
			return
		}
		auditAction(a, r, "apikey.create", "api_key", strconv.FormatInt(meta.ID, 10), map[string]any{
			"name":    meta.Name,
			"scopes":  meta.Scopes,
			"node_id": meta.NodeID,
		})
		// PRG: stash the plaintext for the redirected GET so a browser
		// refresh re-renders the page instead of re-minting a key.
		nonce, stashErr := a.apiKeyPlainStash.put(sessionIDFromRequest(r), plain)
		if stashErr != nil {
			// Out of entropy is effectively unreachable; rather than lose the
			// one-time plaintext, fall back to rendering it directly.
			a.renderAPIKeysPage(w, r, plain, "")
			return
		}
		http.Redirect(w, r, "/apikeys?new="+nonce, http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAPIKeyRevokePage serves POST /apikeys/{id}/revoke from the admin page.
func (a *App) handleAPIKeyRevokePage(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	item, err := a.apikeys().Revoke(r.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrAPIKeyNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "revoke failed", http.StatusInternalServerError)
		return
	}
	auditAction(a, r, "apikey.revoke", "api_key", strconv.FormatInt(id, 10), map[string]any{
		"name": item.Name,
	})
	http.Redirect(w, r, "/apikeys", http.StatusSeeOther)
}
