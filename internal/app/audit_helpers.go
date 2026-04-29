package app

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
)

type auditActor struct {
	typ string
	id  string
}

func actorFromRequest(a *App, r *http.Request) auditActor {
	if a != nil && r != nil {
		if u, ok := a.currentAdmin(r); ok && strings.TrimSpace(u) != "" {
			return auditActor{typ: "admin", id: u}
		}
	}
	if r != nil {
		if ctx := getAPIKeyContext(r.Context()); ctx != nil {
			if ctx.KeyID > 0 {
				return auditActor{typ: "apikey", id: fmt.Sprintf("%d", ctx.KeyID)}
			}
			if ctx.NodeID != nil && *ctx.NodeID > 0 {
				return auditActor{typ: "node", id: fmt.Sprintf("%d", *ctx.NodeID)}
			}
		}
	}
	return auditActor{}
}

func auditAction(a *App, r *http.Request, action, targetType, targetID string, detail any) {
	if a == nil || r == nil {
		return
	}
	act := actorFromRequest(a, r)
	if act.typ == "" || act.id == "" {
		return
	}
	detailJSON := ""
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			detailJSON = string(b)
		}
	}
	_ = a.store.InsertAuditLog(r.Context(), act.typ, act.id, action, targetType, targetID, detailJSON, a.clientIPFromRequest(r), r.UserAgent(), requestIDFromContext(r.Context()))
}

func (a *App) clientIPFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	remoteIP := remoteIPFromRequest(r)
	if remoteIP == "" || a == nil || !trustedProxyContains(a.cfg.TrustedProxyCIDRs, remoteIP) {
		return remoteIP
	}
	if ip := forwardedClientIPFromRequest(r); ip != "" {
		return ip
	}
	return remoteIP
}

func remoteIPFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && net.ParseIP(host) != nil {
		return host
	}
	if net.ParseIP(strings.TrimSpace(r.RemoteAddr)) != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return ""
}

func forwardedClientIPFromRequest(r *http.Request) string {
	xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if xff != "" {
		for _, part := range strings.Split(xff, ",") {
			ip := strings.TrimSpace(part)
			if net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(ip) != nil {
		return ip
	}
	return ""
}

func trustedProxyContains(rawCIDRs string, remoteIP string) bool {
	remote := net.ParseIP(strings.TrimSpace(remoteIP))
	if remote == nil {
		return false
	}
	for _, raw := range strings.Split(rawCIDRs, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if ip := net.ParseIP(raw); ip != nil {
			if ip.Equal(remote) {
				return true
			}
			continue
		}
		_, cidr, err := net.ParseCIDR(raw)
		if err == nil && cidr.Contains(remote) {
			return true
		}
	}
	return false
}
