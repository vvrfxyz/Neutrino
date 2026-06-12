package app

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"neutrino/internal/repo"
	"neutrino/internal/subscription"
)

func (a *App) handleSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/sub/")
	token = strings.Trim(token, "/")
	if token == "" {
		http.NotFound(w, r)
		return
	}
	// Public endpoint: rate-limit per client IP so invalid-token scans cannot
	// hammer the DB.
	if ip := a.clientIPFromRequest(r); ip != "" {
		if !a.rl.Allow("sub:ip:"+ip, 120, time.Minute, time.Now()) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
	}
	res, err := a.users().ResolveSubscription(r.Context(), token)
	if err != nil {
		if errors.Is(err, repo.ErrUserNotFound) || errors.Is(err, repo.ErrUserInactive) {
			http.Error(w, "invalid subscription", http.StatusNotFound)
			return
		}
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	u := res.User

	if u.ActiveLink == nil {
		http.Error(w, "subscription unavailable: no active link", http.StatusConflict)
		return
	}

	restrictedToNodes := res.Restricted
	nodes := res.Nodes
	if len(nodes) == 0 {
		// Explicit node restrictions must never fall back to the global default.
		if restrictedToNodes {
			http.Error(w, "subscription unavailable: no permitted nodes enabled", http.StatusServiceUnavailable)
			return
		}
		if !a.cfg.HasUsableSubscriptionFallback() {
			http.Error(w, "subscription unavailable: no enabled nodes", http.StatusServiceUnavailable)
			return
		}
		nodes = []repo.Node{{
			Name:      "default",
			CoreType:  "xray",
			Protocol:  "vless_reality",
			Host:      a.cfg.ProxyHost,
			Port:      a.cfg.ProxyPort,
			Transport: a.cfg.ProxyType,
			Security:  a.cfg.ProxySecurity,
			Flow:      a.cfg.ProxyFlow,
			SNI:       a.cfg.ProxySNI,
			FP:        a.cfg.RealityFP,
			PublicKey: a.cfg.RealityPublicK,
			ShortID:   a.cfg.RealityShortID,
			Enabled:   true,
		}}
	}
	q := r.URL.Query()
	// ?target= is canonical; ?flag= is the Xboard-style spelling clients
	// already emit. target wins when both are present.
	target := strings.TrimSpace(q.Get("target"))
	if target == "" {
		target = strings.TrimSpace(q.Get("flag"))
	}
	if target == "" {
		target = subscription.DetectTargetFromUA(r.Header.Get("User-Agent"))
	}
	var protocols []string
	if types := strings.TrimSpace(q.Get("types")); types != "" {
		protocols = strings.Split(types, ",")
	}
	payload, contentType, err := subscription.RenderWithOptions(target, u, u.ActiveLink, nodes, subscription.RenderOptions{
		AllowActiveLinkFallback: !restrictedToNodes,
		Protocols:               protocols,
		NameFilter:              q.Get("filter"),
	})
	if err != nil {
		if restrictedToNodes {
			http.Error(w, "subscription unavailable: no permitted nodes renderable", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Subscription-Userinfo", buildSubscriptionUserinfo(u))
	w.Header().Set("Profile-Update-Interval", "24")
	_, _ = w.Write([]byte(payload))
}

// buildSubscriptionUserinfo formats the standard Subscription-Userinfo header
// (recognized by Clash/Stash/Shadowrocket and others) so the client can show
// the user's quota usage, total, and expiry inline in its subscription panel.
//
// Convention from the client's perspective: "upload" is data the client sent
// to the server (== server inbound bytes), "download" is data the client
// received (== server outbound bytes). When MonthlyLimitBytes == 0 we report
// total=0, which clients display as "unlimited".
func buildSubscriptionUserinfo(u repo.User) string {
	upload := u.WindowInboundBytes
	download := u.WindowOutboundBytes
	if u.WindowStart == nil {
		// No active window yet: fall back to the lifetime counters so the
		// client still sees a sensible number rather than zero.
		upload = u.InboundBytes
		download = u.OutboundBytes
	}
	total := int64(0)
	if u.MonthlyLimitBytes > 0 {
		// Credit only extends a finite quota: an unlimited user (limit 0)
		// holding a credit must keep total=0, not display a tiny finite cap.
		total = u.MonthlyLimitBytes + u.WindowCreditBytes
	}
	if total < 0 {
		total = 0
	}
	expire := int64(0)
	if !u.ExpiresAt.IsZero() {
		expire = u.ExpiresAt.Unix()
	}
	return fmt.Sprintf("upload=%d; download=%d; total=%d; expire=%d", upload, download, total, expire)
}

func (a *App) handleAPIOnlineUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	out, err := a.ops().ListOnlineUsers(r.Context(), a.cfg.OnlineDisplayWindowSec)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "items": out})
}

func (a *App) handleAPIMetricsHost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	items := a.hostMonitor.Query(r.URL.Query().Get("range"))
	now := time.Now()
	monthKey := now.In(a.cfg.PanelLocation()).Format("2006-01")
	month := map[string]any{
		"month_key":   monthKey,
		"rx_bytes":    int64(0),
		"tx_bytes":    int64(0),
		"total_bytes": int64(0),
	}
	if usage, ok, err := a.ops().GetHostNetMonthlyUsage(r.Context(), now); err == nil && ok {
		month["rx_bytes"] = usage.RXBytes
		month["tx_bytes"] = usage.TXBytes
		month["total_bytes"] = usage.RXBytes + usage.TXBytes
		if !usage.UpdatedAt.IsZero() {
			month["updated_at"] = usage.UpdatedAt.UTC().Format(time.RFC3339)
		}
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"count": len(items), "items": items, "month": month})
}

func (a *App) handleAPIUsersV1(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		users, err := a.users().List(r.Context())
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		a.writeJSON(w, http.StatusOK, map[string]any{"count": len(users), "items": users})
	case http.MethodPost:
		var in repo.CreateUserInput
		if err := a.readJSON(r, &in); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		u, err := a.users().Create(r.Context(), in)
		if err != nil {
			message := "create failed"
			if errors.Is(err, repo.ErrNodeNotFound) {
				message = err.Error()
			}
			http.Error(w, message, http.StatusBadRequest)
			return
		}
		auditAction(a, r, "user.create", "user", fmt.Sprintf("%d", u.ID), in)
		tok, _ := a.users().GetOrCreateSubscriptionToken(r.Context(), u.ID)
		a.writeJSON(w, http.StatusCreated, map[string]any{
			"user": u,
			"subscription": map[string]any{
				"token": tok.Token,
				"url":   subscription.BuildSubscriptionURL(a.cfg.SubBaseURL, tok.Token, "v2rayn"),
			},
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *App) handleAPIUserRoutesV1(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1/users/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	userID, err := parseInt64Path(parts[0])
	if err != nil {
		http.Error(w, "bad user id", http.StatusBadRequest)
		return
	}
	if len(parts) == 1 {
		a.handleAPIUserByIDV1(w, r, userID)
		return
	}
	if len(parts) == 2 && parts[1] == "traffic" && r.Method == http.MethodGet {
		if _, err := a.users().Get(r.Context(), userID); err != nil {
			if errors.Is(err, repo.ErrUserNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			a.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "query failed"})
			return
		}
		period := strings.TrimSpace(r.URL.Query().Get("period"))
		if period == "" {
			period = "hourly"
		}
		series, err := a.usage().TrafficSeries(r.Context(), userID, period)
		if err != nil {
			status := http.StatusInternalServerError
			message := "query failed"
			if errors.Is(err, repo.ErrUnsupportedTrafficPeriod) {
				status = http.StatusBadRequest
				message = err.Error()
			}
			a.writeJSON(w, status, map[string]any{"error": message})
			return
		}
		a.writeJSON(w, http.StatusOK, map[string]any{
			"user_id": userID,
			"period":  period,
			"series":  series,
		})
		return
	}
	if len(parts) == 2 && parts[1] == "events" && r.Method == http.MethodGet {
		limit := 100
		if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil {
				limit = parsed
			}
		}
		source := strings.TrimSpace(r.URL.Query().Get("source"))
		var nodeID *int64
		if v := strings.TrimSpace(r.URL.Query().Get("node_id")); v != "" {
			if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
				nodeID = &parsed
			}
		}
		items, err := a.usage().UserEvents(r.Context(), userID, limit, source, nodeID)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		a.writeJSON(w, http.StatusOK, map[string]any{
			"user_id": userID,
			"count":   len(items),
			"events":  items,
		})
		return
	}
	if len(parts) == 3 && parts[1] == "quota" && parts[2] == "reset" && r.Method == http.MethodPost {
		var req struct {
			Reason string `json:"reason"`
		}
		if err := a.readJSON(r, &req); err != nil {
			if !errors.Is(err, io.EOF) {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}
		}
		if err := a.users().ResetQuota(r.Context(), userID, req.Reason); err != nil {
			http.Error(w, "reset failed", http.StatusBadRequest)
			return
		}
		auditAction(a, r, "user.quota.reset", "user", fmt.Sprintf("%d", userID), map[string]any{"reason": req.Reason})
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if len(parts) == 3 && parts[1] == "quota" && parts[2] == "credit" && r.Method == http.MethodPost {
		var req struct {
			Bytes int64 `json:"bytes"`
		}
		if err := a.readJSON(r, &req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if err := a.users().CreditQuota(r.Context(), userID, req.Bytes); err != nil {
			http.Error(w, "credit failed", http.StatusBadRequest)
			return
		}
		auditAction(a, r, "user.quota.credit", "user", fmt.Sprintf("%d", userID), map[string]any{"bytes": req.Bytes})
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if len(parts) == 3 && parts[1] == "plan" && parts[2] == "extend" && r.Method == http.MethodPost {
		var req struct {
			Days int `json:"days"`
		}
		if err := a.readJSON(r, &req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if err := a.users().ExtendPlan(r.Context(), userID, req.Days); err != nil {
			http.Error(w, "extend failed", http.StatusBadRequest)
			return
		}
		auditAction(a, r, "user.plan.extend", "user", fmt.Sprintf("%d", userID), map[string]any{"days": req.Days})
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if len(parts) == 2 && parts[1] == "subscription" && r.Method == http.MethodGet {
		tok, err := a.users().GetSubscriptionToken(r.Context(), userID)
		if err != nil {
			if errors.Is(err, repo.ErrUserNotFound) || errors.Is(err, sql.ErrNoRows) {
				a.writeJSON(w, http.StatusOK, map[string]any{})
				return
			}
			http.Error(w, "token query failed", http.StatusInternalServerError)
			return
		}
		a.writeJSON(w, http.StatusOK, map[string]any{
			"token": tok.Token,
			"url":   subscription.BuildSubscriptionURL(a.cfg.SubBaseURL, tok.Token, "v2rayn"),
		})
		return
	}
	if len(parts) == 3 && parts[1] == "subscription" && parts[2] == "rotate" && r.Method == http.MethodPost {
		tok, err := a.users().RotateSubscriptionToken(r.Context(), userID)
		if err != nil {
			http.Error(w, "rotate failed", http.StatusBadRequest)
			return
		}
		auditAction(a, r, "user.subscription.rotate", "user", fmt.Sprintf("%d", userID), map[string]any{"token_prefix": func() string {
			if len(tok.Token) > 8 {
				return tok.Token[:8]
			}
			return tok.Token
		}()})
		a.writeJSON(w, http.StatusOK, map[string]any{"token": tok.Token, "url": subscription.BuildSubscriptionURL(a.cfg.SubBaseURL, tok.Token, "v2rayn")})
		return
	}
	http.NotFound(w, r)
}

func (a *App) handleAPIUserByIDV1(w http.ResponseWriter, r *http.Request, userID int64) {
	switch r.Method {
	case http.MethodGet:
		u, err := a.users().Get(r.Context(), userID)
		if err != nil {
			if errors.Is(err, repo.ErrUserNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		a.writeJSON(w, http.StatusOK, u)
	case http.MethodPatch:
		var req struct {
			Status      *string `json:"status"`
			DeviceLimit *int    `json:"device_limit"`
		}
		if err := a.readJSON(r, &req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if req.Status == nil && req.DeviceLimit == nil {
			http.Error(w, "empty patch", http.StatusBadRequest)
			return
		}
		targetStatus := ""
		if req.Status != nil {
			targetStatus = strings.TrimSpace(*req.Status)
			if targetStatus != "active" && targetStatus != "disabled" {
				http.Error(w, "update failed", http.StatusBadRequest)
				return
			}
		}
		if req.DeviceLimit != nil && *req.DeviceLimit <= 0 {
			http.Error(w, "update failed", http.StatusBadRequest)
			return
		}
		if req.Status != nil {
			if _, err := a.users().SetStatus(r.Context(), userID, targetStatus); err != nil {
				http.Error(w, "update failed", http.StatusBadRequest)
				return
			}
			auditAction(a, r, "user.status.set", "user", fmt.Sprintf("%d", userID), map[string]any{"status": targetStatus})
		}
		if req.DeviceLimit != nil {
			if _, err := a.users().SetDeviceLimit(r.Context(), userID, *req.DeviceLimit); err != nil {
				http.Error(w, "update failed", http.StatusBadRequest)
				return
			}
			auditAction(a, r, "user.device_limit.set", "user", fmt.Sprintf("%d", userID), map[string]any{"device_limit": *req.DeviceLimit})
		}
		u, err := a.users().Get(r.Context(), userID)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		a.writeJSON(w, http.StatusOK, u)
	case http.MethodDelete:
		username, err := a.users().Delete(r.Context(), userID)
		if err != nil {
			http.Error(w, "delete failed", http.StatusBadRequest)
			return
		}
		auditAction(a, r, "user.delete", "user", fmt.Sprintf("%d", userID), map[string]any{"username": username})
		_ = username
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *App) handleAPINodesV1(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items, err := a.nodes().List(r.Context())
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		a.writeJSON(w, http.StatusOK, map[string]any{"count": len(items), "items": items})
	case http.MethodPost:
		var in repo.CreateNodeInput
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "application/json") {
			if err := a.readJSON(r, &in); err != nil {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}
		} else {
			// Support HTMX form posts.
			if err := r.ParseForm(); err != nil {
				http.Error(w, "invalid form", http.StatusBadRequest)
				return
			}
			in.Name = r.FormValue("name")
			in.CoreType = r.FormValue("core_type")
			in.Protocol = r.FormValue("protocol")
			in.Host = r.FormValue("host")
			if v := strings.TrimSpace(r.FormValue("port")); v != "" {
				if p, err := parseInt64Path(v); err == nil {
					in.Port = int(p)
				}
			}
			in.Transport = r.FormValue("transport")
			in.Security = r.FormValue("security")
			in.Flow = r.FormValue("flow")
			in.SNI = r.FormValue("sni")
			in.FP = r.FormValue("fp")
			in.PublicKey = r.FormValue("public_key")
			in.ShortID = r.FormValue("short_id")
			in.ExtraJSON = r.FormValue("extra_json")
			in.AgentURL = r.FormValue("agent_url")
			if v := strings.TrimSpace(r.FormValue("enabled")); v != "" {
				in.Enabled = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "on")
			} else {
				// HTML forms omit unchecked values; default to enabled.
				in.Enabled = true
			}
			if v := strings.TrimSpace(r.FormValue("managed")); v != "" {
				in.Managed = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "on")
			}
		}

		item, managed, err := a.nodes().Create(r.Context(), in)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		auditAction(a, r, "node.create", "node", fmt.Sprintf("%d", item.ID), nil)

		if managed.DesiredVersion != "" {
			auditActionAs(a, r, "admin", "system", "node.xray.desired.sync", "node", fmt.Sprintf("%d", item.ID),
				map[string]any{"desired_version": managed.DesiredVersion, "job_id": managed.JobID, "enqueued": managed.Enqueued})
		}
		_ = a.ops().RefreshNode(r.Context(), item.ID)

		a.writeJSON(w, http.StatusCreated, item)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type patchNodeInput struct {
	Name      *string `json:"name"`
	CoreType  *string `json:"core_type"`
	Protocol  *string `json:"protocol"`
	Host      *string `json:"host"`
	Port      *int    `json:"port"`
	Transport *string `json:"transport"`
	Security  *string `json:"security"`
	Flow      *string `json:"flow"`
	SNI       *string `json:"sni"`
	FP        *string `json:"fp"`
	PublicKey *string `json:"public_key"`
	ShortID   *string `json:"short_id"`
	ExtraJSON *string `json:"extra_json"`
	AgentURL  *string `json:"agent_url"`
	Enabled   *bool   `json:"enabled"`
	Managed   *bool   `json:"managed"`
}

func nodeToCreateInput(n repo.Node) repo.CreateNodeInput {
	return repo.CreateNodeInput{
		Name:      n.Name,
		CoreType:  n.CoreType,
		Protocol:  n.Protocol,
		Host:      n.Host,
		Port:      n.Port,
		Transport: n.Transport,
		Security:  n.Security,
		Flow:      n.Flow,
		SNI:       n.SNI,
		FP:        n.FP,
		PublicKey: n.PublicKey,
		ShortID:   n.ShortID,
		ExtraJSON: n.ExtraJSON,
		AgentURL:  n.AgentURL,
		Enabled:   n.Enabled,
		Managed:   n.Managed,
	}
}

func applyNodePatchInput(in repo.CreateNodeInput, patch patchNodeInput) repo.CreateNodeInput {
	if patch.Name != nil {
		in.Name = *patch.Name
	}
	if patch.CoreType != nil {
		in.CoreType = *patch.CoreType
	}
	if patch.Protocol != nil {
		in.Protocol = *patch.Protocol
	}
	if patch.Host != nil {
		in.Host = *patch.Host
	}
	if patch.Port != nil {
		in.Port = *patch.Port
	}
	if patch.Transport != nil {
		in.Transport = *patch.Transport
	}
	if patch.Security != nil {
		in.Security = *patch.Security
	}
	if patch.Flow != nil {
		in.Flow = *patch.Flow
	}
	if patch.SNI != nil {
		in.SNI = *patch.SNI
	}
	if patch.FP != nil {
		in.FP = *patch.FP
	}
	if patch.PublicKey != nil {
		in.PublicKey = *patch.PublicKey
	}
	if patch.ShortID != nil {
		in.ShortID = *patch.ShortID
	}
	if patch.ExtraJSON != nil {
		in.ExtraJSON = *patch.ExtraJSON
	}
	if patch.AgentURL != nil {
		in.AgentURL = *patch.AgentURL
	}
	if patch.Enabled != nil {
		in.Enabled = *patch.Enabled
	}
	if patch.Managed != nil {
		in.Managed = *patch.Managed
	}
	return in
}

func (a *App) handleAPINodeByIDV1(w http.ResponseWriter, r *http.Request) {
	idPart := strings.TrimPrefix(r.URL.Path, "/api/v1/nodes/")
	idPart = strings.Trim(idPart, "/")
	nodeID, err := parseInt64Path(idPart)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		item, err := a.nodes().Get(r.Context(), nodeID)
		if err != nil {
			if errors.Is(err, repo.ErrNodeNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		a.writeJSON(w, http.StatusOK, item)
	case http.MethodPut, http.MethodPatch:
		before, beforeErr := a.nodes().Get(r.Context(), nodeID)
		if beforeErr != nil {
			if errors.Is(beforeErr, repo.ErrNodeNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		var in repo.CreateNodeInput
		ct := r.Header.Get("Content-Type")
		if r.Method == http.MethodPatch {
			in = nodeToCreateInput(before)
			if strings.HasPrefix(ct, "application/json") {
				var patch patchNodeInput
				if err := a.readJSON(r, &patch); err != nil {
					http.Error(w, "invalid json", http.StatusBadRequest)
					return
				}
				in = applyNodePatchInput(in, patch)
			} else {
				if err := r.ParseForm(); err != nil {
					http.Error(w, "invalid form", http.StatusBadRequest)
					return
				}
				if _, ok := r.Form["name"]; ok {
					in.Name = r.FormValue("name")
				}
				if _, ok := r.Form["core_type"]; ok {
					in.CoreType = r.FormValue("core_type")
				}
				if _, ok := r.Form["protocol"]; ok {
					in.Protocol = r.FormValue("protocol")
				}
				if _, ok := r.Form["host"]; ok {
					in.Host = r.FormValue("host")
				}
				if _, ok := r.Form["port"]; ok {
					if p, err := parseInt64Path(r.FormValue("port")); err == nil {
						in.Port = int(p)
					}
				}
				if _, ok := r.Form["transport"]; ok {
					in.Transport = r.FormValue("transport")
				}
				if _, ok := r.Form["security"]; ok {
					in.Security = r.FormValue("security")
				}
				if _, ok := r.Form["flow"]; ok {
					in.Flow = r.FormValue("flow")
				}
				if _, ok := r.Form["sni"]; ok {
					in.SNI = r.FormValue("sni")
				}
				if _, ok := r.Form["fp"]; ok {
					in.FP = r.FormValue("fp")
				}
				if _, ok := r.Form["public_key"]; ok {
					in.PublicKey = r.FormValue("public_key")
				}
				if _, ok := r.Form["short_id"]; ok {
					in.ShortID = r.FormValue("short_id")
				}
				if _, ok := r.Form["extra_json"]; ok {
					in.ExtraJSON = r.FormValue("extra_json")
				}
				if _, ok := r.Form["agent_url"]; ok {
					in.AgentURL = r.FormValue("agent_url")
				}
				if _, ok := r.Form["enabled"]; ok {
					v := strings.TrimSpace(r.FormValue("enabled"))
					in.Enabled = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "on")
				}
				if _, ok := r.Form["managed"]; ok {
					v := strings.TrimSpace(r.FormValue("managed"))
					in.Managed = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "on")
				}
			}
		} else {
			if strings.HasPrefix(ct, "application/json") {
				if err := a.readJSON(r, &in); err != nil {
					http.Error(w, "invalid json", http.StatusBadRequest)
					return
				}
			} else {
				if err := r.ParseForm(); err != nil {
					http.Error(w, "invalid form", http.StatusBadRequest)
					return
				}
				in.Name = r.FormValue("name")
				in.CoreType = r.FormValue("core_type")
				in.Protocol = r.FormValue("protocol")
				in.Host = r.FormValue("host")
				if v := strings.TrimSpace(r.FormValue("port")); v != "" {
					if p, err := parseInt64Path(v); err == nil {
						in.Port = int(p)
					}
				}
				in.Transport = r.FormValue("transport")
				in.Security = r.FormValue("security")
				in.Flow = r.FormValue("flow")
				in.SNI = r.FormValue("sni")
				in.FP = r.FormValue("fp")
				in.PublicKey = r.FormValue("public_key")
				in.ShortID = r.FormValue("short_id")
				in.ExtraJSON = r.FormValue("extra_json")
				in.AgentURL = r.FormValue("agent_url")
				if v := strings.TrimSpace(r.FormValue("enabled")); v != "" {
					in.Enabled = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "on")
				}
				if v := strings.TrimSpace(r.FormValue("managed")); v != "" {
					in.Managed = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "on")
				}
			}
		}

		update, err := a.nodes().Update(r.Context(), nodeID, in)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		item := update.Node
		auditAction(a, r, "node.update", "node", fmt.Sprintf("%d", item.ID), nil)
		if update.ManagedXray.DesiredVersion != "" {
			auditActionAs(a, r, "admin", "system", "node.xray.desired.sync", "node", fmt.Sprintf("%d", item.ID),
				map[string]any{"desired_version": update.ManagedXray.DesiredVersion, "job_id": update.ManagedXray.JobID, "enqueued": update.ManagedXray.Enqueued})
		}
		_ = a.ops().RefreshNode(r.Context(), nodeID)

		a.writeJSON(w, http.StatusOK, item)
	case http.MethodDelete:
		result, err := a.nodes().Delete(r.Context(), nodeID)
		if err != nil {
			status := http.StatusInternalServerError
			message := err.Error()
			switch {
			case errors.Is(err, repo.ErrNodeNotFound):
				status = http.StatusNotFound
				message = "not found"
			case errors.Is(err, repo.ErrNodeDeleteWouldWidenAccess):
				status = http.StatusConflict
				message = "node delete blocked: restricted users would implicitly gain access to other nodes"
			}
			if r.Header.Get("HX-Request") == "true" {
				setHXToast(w, "error", message)
			}
			http.Error(w, message, status)
			return
		}
		if result.PendingDelete {
			_ = a.ops().RefreshNode(r.Context(), nodeID)
			if result.DisabledForDelete {
				auditAction(a, r, "node.disable_for_delete", "node", fmt.Sprintf("%d", nodeID), nil)
			}
			a.writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "pending_delete": true})
			return
		}
		if result.Deleted {
			auditAction(a, r, "node.delete", "node", fmt.Sprintf("%d", nodeID), nil)
		}
		a.ops().RemoveNode(nodeID)
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
