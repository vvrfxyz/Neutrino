package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
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
		var req struct {
			Name      string   `json:"name"`
			Scopes    []string `json:"scopes"`
			NodeID    *int64   `json:"node_id,omitempty"`
			ExpiresAt string   `json:"expires_at,omitempty"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
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
				http.Error(w, "expires_at must be RFC3339", http.StatusBadRequest)
				return
			}
			in.ExpiresAt = &t
		}
		plain, meta, err := a.apikeys().Create(r.Context(), in)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, repo.ErrNodeNotFound) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		auditAction(a, r, "apikey.create", "api_key", strconv.FormatInt(meta.ID, 10), map[string]any{
			"name":    meta.Name,
			"scopes":  meta.Scopes,
			"node_id": meta.NodeID,
		})
		a.writeJSON(w, http.StatusCreated, map[string]any{
			"key":  plain, // shown exactly once; only the hash is stored
			"item": meta,
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *App) handleAPIKeyByIDV1(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/apikeys/"), "/")
	id, err := parseInt64Path(rest)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		item, err := a.apikeys().Get(r.Context(), id)
		if err != nil {
			a.writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		a.writeJSON(w, http.StatusOK, item)
	case http.MethodDelete:
		item, err := a.apikeys().Revoke(r.Context(), id)
		if err != nil {
			if errors.Is(err, service.ErrAPIKeyNotFound) {
				a.writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
				return
			}
			a.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "revoke failed"})
			return
		}
		auditAction(a, r, "apikey.revoke", "api_key", strconv.FormatInt(id, 10), map[string]any{
			"name": item.Name,
		})
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "item": item})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// SSR admin page: /apikeys (list + create form), /apikeys/{id}/revoke.

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
		a.renderAPIKeysPage(w, r, "", "")
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
			a.renderAPIKeysPage(w, r, "", "创建失败: "+err.Error())
			return
		}
		auditAction(a, r, "apikey.create", "api_key", strconv.FormatInt(meta.ID, 10), map[string]any{
			"name":    meta.Name,
			"scopes":  meta.Scopes,
			"node_id": meta.NodeID,
		})
		a.renderAPIKeysPage(w, r, plain, "")
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *App) handleAPIKeyPageRoutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/apikeys/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[1] != "revoke" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	id, err := parseInt64Path(parts[0])
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
