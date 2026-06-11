package app

import "net/http"

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/login", a.handleLogin)
	mux.Handle("/logout", a.csrfProtect(http.HandlerFunc(a.handleLogout)))

	mux.Handle("/", a.adminOnly(http.HandlerFunc(a.handleRoot)))
	mux.Handle("/users", a.adminOnly(a.csrfProtect(http.HandlerFunc(a.handleUsers))))
	mux.Handle("/users/table", a.adminOnly(http.HandlerFunc(a.handleUsersTable)))
	mux.Handle("/users/", a.adminOnly(a.csrfProtect(http.HandlerFunc(a.handleUserRoutes))))
	mux.Handle("/nodes", a.adminOnly(http.HandlerFunc(a.handleNodesPage)))
	mux.Handle("/nodes/", a.adminOnly(a.csrfProtect(http.HandlerFunc(a.handleNodeRoutesPage))))
	mux.Handle("/traffic", a.adminOnly(http.HandlerFunc(a.handleTrafficPage)))
	mux.Handle("/enforcements", a.adminOnly(http.HandlerFunc(a.handleEnforcementsPage)))
	mux.Handle("/apikeys", a.adminOnly(a.csrfProtect(http.HandlerFunc(a.handleAPIKeysPage))))
	mux.Handle("/apikeys/", a.adminOnly(a.csrfProtect(http.HandlerFunc(a.handleAPIKeyPageRoutes))))
	mux.Handle("/ops", a.adminOnly(http.HandlerFunc(a.handleOps)))
	mux.Handle("/ops-v2", a.adminOnly(http.HandlerFunc(a.handleOpsV2)))
	mux.Handle("/ops-v2/", a.adminOnly(http.HandlerFunc(a.handleOpsV2)))
	mux.HandleFunc("/sub/", a.handleSubscription)

	mux.Handle("/api/v1/online-users", a.apiV1Auth(http.HandlerFunc(a.handleAPIOnlineUsers)))
	mux.Handle("/api/v1/metrics/host", a.apiV1Auth(http.HandlerFunc(a.handleAPIMetricsHost)))
	mux.Handle("/api/v1/users", a.apiV1Auth(http.HandlerFunc(a.handleAPIUsersV1)))
	mux.Handle("/api/v1/users/", a.apiV1Auth(http.HandlerFunc(a.handleAPIUserRoutesV1)))
	mux.Handle("/api/v1/nodes", a.apiV1Auth(http.HandlerFunc(a.handleAPINodesV1)))
	mux.Handle("/api/v1/nodes/", a.apiV1Auth(http.HandlerFunc(a.handleAPINodeByIDV1Extended)))
	mux.Handle("/api/v1/usage", a.apiV1AuthMTLSOnly(http.HandlerFunc(a.handleAPIUsageV1)))
	mux.Handle("/api/v1/traffic/summary", a.apiV1Auth(http.HandlerFunc(a.handleAPITrafficSummaryV1)))
	mux.Handle("/api/v1/ops/config", a.apiV1Auth(http.HandlerFunc(a.handleAPIOpsConfigV1)))
	mux.Handle("/api/v1/ops/nodes", a.apiV1Auth(http.HandlerFunc(a.handleAPIOpsNodesV1)))
	mux.Handle("/api/v1/ops/alerts", a.apiV1Auth(http.HandlerFunc(a.handleAPIOpsAlertsV1)))
	mux.Handle("/api/v1/backups", a.apiV1Auth(http.HandlerFunc(a.handleAPIBackupsV1)))
	mux.Handle("/api/v1/backups/", a.apiV1Auth(http.HandlerFunc(a.handleAPIBackupRoutesV1)))
	mux.Handle("/api/v1/apikeys", a.apiV1Auth(http.HandlerFunc(a.handleAPIKeysV1)))
	mux.Handle("/api/v1/apikeys/", a.apiV1Auth(http.HandlerFunc(a.handleAPIKeyByIDV1)))
	// Real-time admin stream: session-only (no API key) since browsers can't
	// attach custom headers to native WebSocket connections.
	mux.Handle("/api/v1/stream", a.adminOnly(http.HandlerFunc(a.handleAdminWebSocket)))

	return withRequestID(mux)
}

// AgentRoutes is served on the dedicated agent mTLS listener. Keep this surface area minimal.
func (a *App) AgentRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.Handle("/api/v1/usage", a.apiV1AuthMTLSOnly(http.HandlerFunc(a.handleAPIUsageV1)))
	mux.Handle("/api/v1/nodes/", a.apiV1AuthMTLSOnly(http.HandlerFunc(a.handleAgentNodeRoutes)))

	return withRequestID(mux)
}
