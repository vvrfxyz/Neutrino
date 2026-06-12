package app

import "net/http"

// Routes registers the admin/public HTTP surface. API routes use Go 1.22+
// method+wildcard ServeMux patterns; handlers read path params via
// r.PathValue instead of hand-parsing r.URL.Path.
func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/login", a.handleLogin)
	mux.Handle("/logout", a.csrfProtect(http.HandlerFunc(a.handleLogout)))

	// SSR pages.
	mux.Handle("/", a.adminOnly(http.HandlerFunc(a.handleRoot)))
	mux.Handle("/users", a.adminOnly(a.csrfProtect(http.HandlerFunc(a.handleUsers))))
	// "GET /users/table" must carry the method: a method-less literal would
	// conflict with "GET /users/{id}" under the 1.22 ServeMux precedence rules.
	mux.Handle("GET /users/table", a.adminOnly(http.HandlerFunc(a.handleUsersTable)))
	mux.Handle("GET /users/{id}", a.adminOnly(a.csrfProtect(http.HandlerFunc(a.handleUserDetailPage))))
	mux.Handle("POST /users/{id}/{action}", a.adminOnly(a.csrfProtect(http.HandlerFunc(a.handleUserAction))))
	mux.Handle("/nodes", a.adminOnly(http.HandlerFunc(a.handleNodesPage)))
	mux.Handle("GET /nodes/{id}/deploy", a.adminOnly(a.csrfProtect(http.HandlerFunc(a.handleNodeDeployPageRoute))))
	mux.Handle("POST /nodes/{id}/deploy", a.adminOnly(a.csrfProtect(http.HandlerFunc(a.handleNodeDeployRotateRoute))))
	mux.Handle("/traffic", a.adminOnly(http.HandlerFunc(a.handleTrafficPage)))
	mux.Handle("/enforcements", a.adminOnly(http.HandlerFunc(a.handleEnforcementsPage)))
	mux.Handle("/apikeys", a.adminOnly(a.csrfProtect(http.HandlerFunc(a.handleAPIKeysPage))))
	mux.Handle("POST /apikeys/{id}/revoke", a.adminOnly(a.csrfProtect(http.HandlerFunc(a.handleAPIKeyRevokePage))))
	mux.Handle("/ops", a.adminOnly(http.HandlerFunc(a.handleOps)))
	mux.Handle("/ops-v2", a.adminOnly(http.HandlerFunc(a.handleOpsV2)))
	mux.Handle("/ops-v2/", a.adminOnly(http.HandlerFunc(a.handleOpsV2)))
	mux.HandleFunc("/sub/", a.handleSubscription)

	// JSON API v1. Every route is wrapped in apiV1Auth (admin session, API key,
	// or node mTLS; the enroll route is carved out inside the middleware).
	api := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, a.apiV1Auth(h))
	}
	// nodeAPI/userAPI additionally parse the numeric {id} wildcard.
	nodeAPI := func(pattern string, h func(http.ResponseWriter, *http.Request, int64)) {
		api(pattern, a.withID("bad node id", h))
	}
	userAPI := func(pattern string, h func(http.ResponseWriter, *http.Request, int64)) {
		api(pattern, a.withID("bad user id", h))
	}

	api("GET /api/v1/online-users", a.handleAPIOnlineUsers)
	api("GET /api/v1/metrics/host", a.handleAPIMetricsHost)

	api("/api/v1/users", a.handleAPIUsersV1)             // GET list / POST create
	userAPI("/api/v1/users/{id}", a.handleAPIUserByIDV1) // GET / PATCH / DELETE
	userAPI("GET /api/v1/users/{id}/traffic", a.handleAPIUserTrafficV1)
	userAPI("GET /api/v1/users/{id}/events", a.handleAPIUserEventsV1)
	userAPI("POST /api/v1/users/{id}/quota/reset", a.handleAPIUserQuotaResetV1)
	userAPI("POST /api/v1/users/{id}/quota/credit", a.handleAPIUserQuotaCreditV1)
	userAPI("POST /api/v1/users/{id}/plan/extend", a.handleAPIUserPlanExtendV1)
	userAPI("GET /api/v1/users/{id}/subscription", a.handleAPIUserSubscriptionV1)
	userAPI("POST /api/v1/users/{id}/subscription/rotate", a.handleAPIUserSubscriptionRotateV1)

	api("/api/v1/nodes", a.handleAPINodesV1)             // GET list / POST create
	nodeAPI("/api/v1/nodes/{id}", a.handleAPINodeByIDV1) // GET / PUT / PATCH / DELETE
	nodeAPI("POST /api/v1/nodes/{id}/enroll", a.handleAPINodeEnroll)
	nodeAPI("POST /api/v1/nodes/{id}/cert/revoke", a.handleAPINodeCertRevoke)
	nodeAPI("POST /api/v1/nodes/{id}/enable", a.handleAPINodeEnable)
	nodeAPI("POST /api/v1/nodes/{id}/disable", a.handleAPINodeDisable)
	nodeAPI("POST /api/v1/nodes/{id}/report", a.handleAPINodeReport)
	nodeAPI("GET /api/v1/nodes/{id}/jobs", a.handleAPINodeJobsV1)
	nodeAPI("/api/v1/nodes/{id}/metadata", a.handleAPINodeMetadata) // GET / PUT / POST
	nodeAPI("POST /api/v1/nodes/{id}/probes", a.handleAPINodeProbes)
	nodeAPI("GET /api/v1/nodes/{id}/probe-results", a.handleAPINodeProbeResultsV1)
	nodeAPI("GET /api/v1/nodes/{id}/metrics", a.handleAPINodeMetricsV1)
	nodeAPI("GET /api/v1/nodes/{id}/metric-details/latest", a.handleAPINodeMetricDetailsLatestV1)
	nodeAPI("GET /api/v1/nodes/{id}/static-facts/latest", a.handleAPINodeStaticFactsLatestV1)
	nodeAPI("GET /api/v1/nodes/{id}/static-facts/history", a.handleAPINodeStaticFactsHistoryV1)
	nodeAPI("GET /api/v1/nodes/{id}/agent/users", a.handleAPINodeAgentUsers)
	nodeAPI("POST /api/v1/nodes/{id}/managed/xray/preview", a.handleAPINodeManagedXrayPreview)
	nodeAPI("POST /api/v1/nodes/{id}/managed/xray/deploy", a.handleAPINodeManagedXrayDeploy)
	nodeAPI("POST /api/v1/nodes/{id}/managed/xray/rollback", a.handleAPINodeManagedXrayRollback)

	// Usage ingest stays method-less: the mTLS-only middleware must reject
	// unauthenticated requests before any method handling, and this main-mux
	// registration can never authenticate on the plain-HTTP listener — the
	// agent mTLS listener is the only live ingest path.
	mux.Handle("/api/v1/usage", a.apiV1AuthMTLSOnly(http.HandlerFunc(a.handleAPIUsageV1)))

	api("GET /api/v1/traffic/summary", a.handleAPITrafficSummaryV1)
	api("GET /api/v1/ops/config", a.handleAPIOpsConfigV1)
	api("GET /api/v1/ops/nodes", a.handleAPIOpsNodesV1)
	api("/api/v1/ops/alerts", a.handleAPIOpsAlertsV1) // GET list / POST upsert+resolve
	api("GET /api/v1/backups", a.handleAPIListBackups)
	api("POST /api/v1/backups", a.handleAPICreateBackup)
	api("POST /api/v1/backups/restore", a.handleAPIRestoreBackup)
	api("GET /api/v1/backups/{id}", a.withID("invalid id", a.handleAPIGetBackup))
	api("DELETE /api/v1/backups/{id}", a.withID("invalid id", a.handleAPIDeleteBackup))
	api("GET /api/v1/backups/{id}/download", a.withID("invalid id", a.handleAPIDownloadBackup))
	api("/api/v1/apikeys", a.handleAPIKeysV1)                                 // GET list / POST create
	api("/api/v1/apikeys/{id}", a.withID("invalid id", a.handleAPIKeyByIDV1)) // GET / DELETE

	// Real-time admin stream: session-only (no API key) since browsers can't
	// attach custom headers to native WebSocket connections.
	mux.Handle("/api/v1/stream", a.adminOnly(http.HandlerFunc(a.handleAdminWebSocket)))

	// Auth-then-404 for unmatched API paths, mirroring the old prefix routing:
	// a valid credential probing an unknown path gets 404, an invalid one 401.
	api("/api/", func(w http.ResponseWriter, r *http.Request) {
		a.apiError(w, http.StatusNotFound, "not found")
	})

	return withRequestID(mux)
}

// AgentRoutes is served on the dedicated agent mTLS listener. Keep this surface area minimal.
func (a *App) AgentRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mtls := func(pattern string, h func(http.ResponseWriter, *http.Request, int64)) {
		mux.Handle(pattern, a.apiV1AuthMTLSOnly(a.withID("bad node id", h)))
	}

	mux.Handle("/api/v1/usage", a.apiV1AuthMTLSOnly(http.HandlerFunc(a.handleAPIUsageV1)))
	mtls("POST /api/v1/nodes/{id}/report", a.handleAPINodeReport)
	mtls("GET /api/v1/nodes/{id}/agent/users", a.handleAPINodeAgentUsers)
	mtls("POST /api/v1/nodes/{id}/jobs/claim", a.handleAPINodeJobsClaim)
	mtls("POST /api/v1/nodes/{id}/jobs/{job}/finish", a.handleAPINodeJobFinishRoute)
	mtls("POST /api/v1/nodes/{id}/cert/renew", a.handleAPINodeCertRenew)
	// Auth-then-404 for anything else under the node prefix (mirrors the old
	// prefix dispatcher).
	mux.Handle("/api/v1/nodes/", a.apiV1AuthMTLSOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.apiError(w, http.StatusNotFound, "not found")
	})))

	return withRequestID(mux)
}
