package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"neutrino/internal/config"
	"neutrino/internal/hostnet"
	"neutrino/internal/monitor"
	"neutrino/internal/notify"
	"neutrino/internal/repo"
	"neutrino/internal/service"
	"neutrino/internal/subscription"
)

type App struct {
	cfg         config.Config
	store       *repo.Store
	pages       map[string]*template.Template
	notifier    *notify.Notifier
	hostMonitor *monitor.HostMonitor
	rl          *rateLimiter
	csrfSecret  []byte

	userService   *service.UserService
	usageService  *service.UsageService
	nodeService   *service.NodeService
	opsService    *service.OpsService
	alertService  *service.AlertService
	backupService *service.BackupService
	auditService  *service.AuditService
	apiKeyService *service.APIKeyService

	wsHub              *wsHub
	metricHistoryQueue *nodeMetricHistoryQueue
	apiKeyPlainStash   oneTimePlainKeys

	usersSyncMu      sync.Mutex
	lastUsersSyncReq time.Time
}

// PageData is the envelope passed to every base-layout page.
type PageData struct {
	CurrentAdmin string
	ActivePage   string // sidebar highlight key
	CSRFToken    string
	Content      any // page-specific data struct
}

type indexData struct {
	Users []repo.User
	Nodes []repo.Node
	Error string
}

type userDetailData struct {
	User               repo.User
	SubscriptionURL    string
	AccessEvents       []repo.UserEvent
	OnlineSessions     []repo.OnlineUser
	OnlineDeviceCount  int
	OnlineSessionCount int
	OnlineWindowSec    int
	AllNodes           []repo.Node
	AllowedNodeIDs     map[int64]bool
	Error              string
}

type loginData struct {
	Error string
}

func New(cfg config.Config, store *repo.Store) *App {
	panelLoc := cfg.PanelLocation()
	panelTZJSON := func() template.JS {
		b, err := json.Marshal(panelLoc.String())
		if err != nil {
			return template.JS(`"UTC"`)
		}
		return template.JS(b)
	}
	funcs := template.FuncMap{
		"fmtBytesGB": func(v int64) string { return fmt.Sprintf("%.2f GB", float64(v)/(1024*1024*1024)) },
		"shortHash": func(s string) string {
			s = strings.TrimSpace(s)
			if len(s) <= 8 {
				return s
			}
			return s[:8]
		},
		"fmtTime": func(t time.Time) string {
			return t.In(panelLoc).Format("2006-01-02 15:04:05")
		},
		"fmtMaybeTime": func(t *time.Time) string {
			if t == nil {
				return "-"
			}
			return t.In(panelLoc).Format("2006-01-02 15:04:05")
		},
		"panelTZJSON": panelTZJSON,
		"opsPollIntervalSec": func() int {
			return int(cfg.OpsSnapshotInterval() / time.Second)
		},
		"opsPollIntervalMS": func() int {
			return int(cfg.OpsSnapshotInterval() / time.Millisecond)
		},
		"fmtMaybeInt64": func(v *int64) string {
			if v == nil {
				return "-"
			}
			return strconv.FormatInt(*v, 10)
		},
		"toJSON": func(v any) template.JS {
			b, err := json.Marshal(v)
			if err != nil {
				return template.JS("null")
			}
			return template.JS(b)
		},
		"statusClass": func(status string) string {
			switch status {
			case "active":
				return "bg-emerald-100 text-emerald-700"
			case "disabled":
				return "bg-amber-100 text-amber-700"
			case "expired", "over_limit":
				return "bg-red-100 text-red-700"
			case "over_ip_limit":
				return "bg-orange-100 text-orange-700"
			default:
				return "bg-slate-100 text-slate-600"
			}
		},
		"quotaPercent": func(used, limit int64) float64 {
			if limit <= 0 {
				return 0
			}
			p := float64(used) / float64(limit) * 100
			if p > 100 {
				p = 100
			}
			return p
		},
		// dict creates a map from key/value pairs for passing multiple values to sub-templates.
		"dict": func(values ...any) map[string]any {
			m := make(map[string]any, len(values)/2)
			for i := 0; i+1 < len(values); i += 2 {
				key, _ := values[i].(string)
				m[key] = values[i+1]
			}
			return m
		},
		"surfaceCardClass": func() string {
			return "overflow-hidden rounded-xl border border-slate-200 bg-white shadow-sm"
		},
		"softStatClass": func(extra string) string {
			base := "rounded-lg bg-slate-50 px-3 py-2"
			if strings.TrimSpace(extra) == "" {
				return base
			}
			return base + " " + strings.TrimSpace(extra)
		},
		"emptyStateClass": func() string {
			return "rounded-xl border border-dashed border-slate-300 bg-white px-4 py-12 text-center text-sm text-slate-400"
		},
		"chipButtonClass": func(variant string, fullWidth bool) string {
			base := "inline-flex items-center justify-center rounded-md px-2.5 text-xs font-medium transition-colors"
			if fullWidth {
				base += " w-full py-1.5 sm:w-auto sm:py-1"
			} else {
				base += " py-1"
			}
			switch variant {
			case "blue":
				return base + " bg-blue-50 text-blue-700 hover:bg-blue-100"
			case "slate":
				return base + " bg-slate-100 text-slate-700 hover:bg-slate-200"
			case "amber":
				return base + " bg-amber-50 text-amber-700 hover:bg-amber-100"
			case "emerald":
				return base + " bg-emerald-50 text-emerald-700 hover:bg-emerald-100"
			case "red":
				return base + " bg-red-50 text-red-700 hover:bg-red-100"
			default:
				return base + " bg-slate-100 text-slate-700 hover:bg-slate-200"
			}
		},
	}

	const tplDir = "templates"
	baseTmpl := tplDir + "/base.tmpl"
	partialFiles, _ := fs.Glob(templatesFS, tplDir+"/partials/*.tmpl")

	pageFiles := map[string]string{
		"users":        tplDir + "/users.tmpl",
		"user_detail":  tplDir + "/user_detail.tmpl",
		"nodes":        tplDir + "/nodes.tmpl",
		"node_deploy":  tplDir + "/node_deploy.tmpl",
		"traffic":      tplDir + "/traffic.tmpl",
		"ops":          tplDir + "/ops.tmpl",
		"enforcements": tplDir + "/enforcements.tmpl",
		"apikeys":      tplDir + "/apikeys.tmpl",
	}

	// Extra files that specific pages need (e.g. HTMX partials included via {{template}}).
	pageExtras := map[string][]string{
		"users": {tplDir + "/users_table.tmpl"},
	}

	pages := make(map[string]*template.Template, len(pageFiles)+2)

	// Login is standalone (no base layout).
	pages["login"] = template.Must(
		template.New("").Funcs(funcs).ParseFS(templatesFS, tplDir+"/login.tmpl"),
	)
	// users_table is an HTMX partial (no base layout).
	pages["users_table"] = template.Must(
		template.New("").Funcs(funcs).ParseFS(templatesFS, tplDir+"/users_table.tmpl"),
	)

	// Each page template gets its own template set: base + partials + page + extras.
	for name, pageFile := range pageFiles {
		files := make([]string, 0, 2+len(partialFiles)+2)
		files = append(files, baseTmpl, pageFile)
		files = append(files, partialFiles...)
		files = append(files, pageExtras[name]...)
		pages[name] = template.Must(
			template.New("").Funcs(funcs).ParseFS(templatesFS, files...),
		)
	}

	secret := loadCSRFSecret()

	a := &App{
		cfg:         cfg,
		store:       store,
		pages:       pages,
		notifier:    notify.New(cfg),
		hostMonitor: monitor.NewHostMonitor(1440, cfg.HostProcPath),
		rl:          newRateLimiter(),
		csrfSecret:  secret,
	}
	a.userService = service.NewUserService(store, a)
	a.usageService = service.NewUsageService(store, cfg.OnlineWindowSec, cfg.IPLimitStrikes, a)
	a.nodeService = service.NewNodeService(store, cfg.NodeStaleDeleteAfterSec, a)
	a.opsService = service.NewOpsService(store)
	a.alertService = service.NewAlertService(store)
	a.backupService = service.NewBackupService(store, cfg.DBPath, cfg.BackupDir)
	a.auditService = service.NewAuditService(store)
	a.apiKeyService = service.NewAPIKeyService(store)
	a.wsHub = newWSHub()
	a.metricHistoryQueue = newNodeMetricHistoryQueueFromConfig(store, cfg)
	return a
}

func (a *App) ensureServices() {
	if a.store == nil {
		return
	}
	if a.userService == nil {
		a.userService = service.NewUserService(a.store, a)
	}
	if a.usageService == nil {
		a.usageService = service.NewUsageService(a.store, a.cfg.OnlineWindowSec, a.cfg.IPLimitStrikes, a)
	}
	if a.nodeService == nil {
		a.nodeService = service.NewNodeService(a.store, a.cfg.NodeStaleDeleteAfterSec, a)
	}
	if a.opsService == nil {
		a.opsService = service.NewOpsService(a.store)
	}
	if a.alertService == nil {
		a.alertService = service.NewAlertService(a.store)
	}
	if a.backupService == nil {
		a.backupService = service.NewBackupService(a.store, a.cfg.DBPath, a.cfg.BackupDir)
	}
	if a.auditService == nil {
		a.auditService = service.NewAuditService(a.store)
	}
	if a.apiKeyService == nil {
		a.apiKeyService = service.NewAPIKeyService(a.store)
	}
	if a.metricHistoryQueue == nil {
		a.metricHistoryQueue = newNodeMetricHistoryQueueFromConfig(a.store, a.cfg)
	}
}

func newNodeMetricHistoryQueueFromConfig(store *repo.Store, cfg config.Config) *nodeMetricHistoryQueue {
	capacity := cfg.NodeMetricHistoryQueueCapacity
	if capacity < 0 {
		capacity = 0
	}
	if capacity == 0 && cfg.NodeMetricHistoryQueueDir == "" && cfg.NodeMetricHistoryQueueMaxBytes == 0 {
		capacity = 4096
	}
	diskDir := strings.TrimSpace(cfg.NodeMetricHistoryQueueDir)
	if diskDir == "" && cfg.NodeMetricHistoryQueueMaxBytes > 0 {
		dbDir := filepath.Dir(strings.TrimSpace(cfg.DBPath))
		if dbDir == "" || dbDir == "." {
			dbDir = "."
		}
		diskDir = filepath.Join(dbDir, "node_metric_history_queue")
	}
	return newNodeMetricHistoryQueueWithDisk(store, capacity, diskDir, cfg.NodeMetricHistoryQueueMaxBytes)
}

func (a *App) users() *service.UserService {
	a.ensureServices()
	return a.userService
}

func (a *App) usage() *service.UsageService {
	a.ensureServices()
	return a.usageService
}

func (a *App) nodes() *service.NodeService {
	a.ensureServices()
	return a.nodeService
}

func (a *App) ops() *service.OpsService {
	a.ensureServices()
	return a.opsService
}

func (a *App) alerts() *service.AlertService {
	a.ensureServices()
	return a.alertService
}

func (a *App) backups() *service.BackupService {
	a.ensureServices()
	return a.backupService
}

func (a *App) audit() *service.AuditService {
	a.ensureServices()
	return a.auditService
}

func (a *App) apikeys() *service.APIKeyService {
	a.ensureServices()
	return a.apiKeyService
}

func loadCSRFSecret() []byte {
	raw := strings.TrimSpace(os.Getenv("CSRF_SECRET"))
	if raw != "" {
		if decoded, err := hex.DecodeString(raw); err == nil && len(decoded) >= 16 {
			return decoded
		}
	}
	log.Printf("csrf: ephemeral secret generated; sessions will need reissue after restart")
	return generateCSRFSecret()
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if admin, ok := a.currentAdmin(r); ok && admin != "" {
		http.Redirect(w, r, "/users", http.StatusFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		_ = a.pages["login"].ExecuteTemplate(w, "login.tmpl", loginData{})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			_ = a.pages["login"].ExecuteTemplate(w, "login.tmpl", loginData{Error: "表单解析失败"})
			return
		}
		username := strings.TrimSpace(r.FormValue("username"))
		password := r.FormValue("password")
		ip := a.clientIPFromRequest(r)
		now := time.Now()
		if !a.rl.Allow("login:ip:"+ip, 60, time.Minute, now) || !a.rl.Allow("login:ipuser:"+ip+":"+username, 20, time.Minute, now) {
			auditActionAs(a, r, "admin", username, "admin.login", "admin_account", username, map[string]any{"ok": false, "reason": "rate_limited"})
			_ = a.pages["login"].ExecuteTemplate(w, "login.tmpl", loginData{Error: "尝试过于频繁，请稍后再试"})
			return
		}
		if err := a.store.AuthenticateAdmin(r.Context(), username, password); err != nil {
			auditActionAs(a, r, "admin", username, "admin.login", "admin_account", username, map[string]any{"ok": false, "reason": "bad_credentials"})
			_ = a.pages["login"].ExecuteTemplate(w, "login.tmpl", loginData{Error: "用户名或密码错误"})
			return
		}
		auditActionAs(a, r, "admin", username, "admin.login", "admin_account", username, map[string]any{"ok": true})
		ttl := time.Duration(a.cfg.AdminSessionHours) * time.Hour
		if ttl <= 0 {
			ttl = 24 * time.Hour
		}
		sid, err := a.store.CreateAdminSession(r.Context(), username, ttl)
		if err != nil {
			http.Error(w, "create session failed", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    sid,
			Path:     "/",
			HttpOnly: true,
			Secure:   requestIsHTTPS(r),
			SameSite: http.SameSiteLaxMode,
			Expires:  time.Now().Add(ttl),
		})
		a.issueCSRFCookie(w, r, sid)
		http.Redirect(w, r, "/users", http.StatusFound)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	auditAction(a, r, "admin.logout", "admin_session", "current", nil)
	if c, err := r.Cookie(sessionCookieName); err == nil {
		_ = a.store.DeleteAdminSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	a.expireCSRFCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (a *App) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/users", http.StatusFound)
}

func (a *App) handleOps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	current, _ := a.currentAdmin(r)
	if err := a.pages["ops"].ExecuteTemplate(w, "base.tmpl", PageData{
		CurrentAdmin: current,
		ActivePage:   "ops",
		CSRFToken:    a.csrfTokenFor(r),
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) handleOpsV2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.cfg.EnableOpsV2 {
		http.NotFound(w, r)
		return
	}
	staticDir := filepath.Join("frontend", "ops-demo", "dist")
	if r.URL.Path == "/ops-v2" || r.URL.Path == "/ops-v2/" {
		a.serveOpsV2Index(w, r, filepath.Join(staticDir, "index.html"))
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/ops-v2/")
	rel = filepath.Clean(rel)
	if rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(staticDir, rel))
}

func (a *App) serveOpsV2Index(w http.ResponseWriter, r *http.Request, path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	body := string(raw)
	if token := a.csrfTokenFor(r); token != "" && !strings.Contains(body, `name="csrf-token"`) {
		meta := fmt.Sprintf(`    <meta name="csrf-token" content="%s" />`, template.HTMLEscapeString(token))
		body = strings.Replace(body, "</head>", meta+"\n  </head>", 1)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

func (a *App) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.renderIndex(w, r, "")
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			a.renderUsersTableWithError(w, r, "表单解析失败")
			return
		}
		username := strings.TrimSpace(r.FormValue("username"))
		limitGB, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("monthly_limit_gb")), 10, 64)
		mode := strings.TrimSpace(r.FormValue("counting_mode"))
		quotaCycle := strings.TrimSpace(r.FormValue("quota_cycle"))
		quotaTZ := strings.TrimSpace(r.FormValue("quota_tz"))
		deviceLimit, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("device_limit")))
		planDays, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("plan_days")))
		if username == "" || limitGB <= 0 {
			a.renderUsersTableWithError(w, r, "用户名和流量上限必填")
			return
		}
		if mode != "single" && mode != "double" {
			mode = "double"
		}
		if planDays <= 0 {
			planDays = 30
		}
		if quotaCycle != "day" && quotaCycle != "week" && quotaCycle != "month" {
			quotaCycle = "month"
		}
		if quotaTZ == "" {
			quotaTZ = "Asia/Shanghai"
		}
		if deviceLimit <= 0 {
			deviceLimit = 2
		}
		var nodeIDs []int64
		for _, raw := range r.Form["node_ids"] {
			if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
				nodeIDs = append(nodeIDs, id)
			}
		}
		u, err := a.users().Create(r.Context(), repo.CreateUserInput{
			Username:       username,
			MonthlyLimitGB: limitGB,
			CountingMode:   mode,
			QuotaCycle:     quotaCycle,
			QuotaTZ:        quotaTZ,
			DeviceLimit:    deviceLimit,
			PlanDays:       planDays,
			NodeIDs:        nodeIDs,
		})
		if err != nil {
			a.renderUsersTableWithError(w, r, "创建用户失败: "+err.Error())
			return
		}
		auditAction(a, r, "user.create", "user", fmt.Sprintf("%d", u.ID), map[string]any{
			"username":         username,
			"monthly_limit_gb": limitGB,
			"counting_mode":    mode,
			"quota_cycle":      quotaCycle,
			"quota_tz":         quotaTZ,
			"device_limit":     deviceLimit,
			"plan_days":        planDays,
		})
		if r.Header.Get("HX-Request") == "true" {
			setHXBoolTrigger(w, "user-created")
			a.renderUsersTableOnly(w, r)
			return
		}
		http.Redirect(w, r, "/users", http.StatusFound)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *App) handleUsersTable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.renderUsersTableOnly(w, r)
}

func (a *App) handleUserRoutes(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/users/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	userID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || userID <= 0 {
		http.Error(w, "bad user id", http.StatusBadRequest)
		return
	}

	if r.Method == http.MethodGet && len(parts) == 1 {
		a.renderUserDetail(w, r, userID, "")
		return
	}

	if r.Method != http.MethodPost || len(parts) != 2 {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	switch parts[1] {
	case "links":
		linkUser, err := a.users().RotateProxyLink(r.Context(), userID)
		if err != nil {
			a.handleUserActionError(w, r, userID, "生成链接失败: "+err.Error())
			return
		}
		auditAction(a, r, "user.link.rotate", "user", fmt.Sprintf("%d", userID), map[string]any{
			"link_id": func() int64 {
				if linkUser.ActiveLink != nil {
					return linkUser.ActiveLink.ID
				}
				return 0
			}(),
		})
		a.handleUserActionSuccess(w, r, userID, "已生成新链接")
	case "enable":
		u, err := a.users().SetStatus(r.Context(), userID, "active")
		if err != nil {
			a.handleUserActionError(w, r, userID, "启用失败: "+err.Error())
			return
		}
		auditAction(a, r, "user.status.set", "user", fmt.Sprintf("%d", userID), map[string]any{"status": "active"})
		_ = u
		a.handleUserActionSuccess(w, r, userID, "用户已启用")
	case "disable":
		u, err := a.users().SetStatus(r.Context(), userID, "disabled")
		if err != nil {
			a.handleUserActionError(w, r, userID, "禁用失败: "+err.Error())
			return
		}
		auditAction(a, r, "user.status.set", "user", fmt.Sprintf("%d", userID), map[string]any{"status": "disabled"})
		_ = u
		a.handleUserActionSuccess(w, r, userID, "用户已禁用")
	case "delete":
		username, err := a.users().Delete(r.Context(), userID)
		if err != nil {
			a.handleUserActionError(w, r, userID, "删除失败: "+err.Error())
			return
		}
		auditAction(a, r, "user.delete", "user", fmt.Sprintf("%d", userID), map[string]any{"username": username})
		_ = username
		if r.Header.Get("HX-Request") == "true" {
			setHXToast(w, "success", "用户已删除")
			a.renderUsersTableOnly(w, r)
			return
		}
		http.Redirect(w, r, "/users", http.StatusFound)
	case "device_limit":
		if err := r.ParseForm(); err != nil {
			a.handleUserActionError(w, r, userID, "表单解析失败")
			return
		}
		limit, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("device_limit")))
		if limit <= 0 {
			a.handleUserActionError(w, r, userID, "设备上限必须大于 0")
			return
		}
		if _, err := a.users().SetDeviceLimit(r.Context(), userID, limit); err != nil {
			a.handleUserActionError(w, r, userID, "更新设备上限失败: "+err.Error())
			return
		}
		auditAction(a, r, "user.device_limit.set", "user", fmt.Sprintf("%d", userID), map[string]any{"device_limit": limit})
		a.handleUserActionSuccess(w, r, userID, "设备上限已更新")
	case "quota_reset":
		if err := r.ParseForm(); err != nil {
			a.handleUserActionError(w, r, userID, "表单解析失败")
			return
		}
		reason := strings.TrimSpace(r.FormValue("reason"))
		if err := a.users().ResetQuota(r.Context(), userID, reason); err != nil {
			a.handleUserActionError(w, r, userID, "重置失败: "+err.Error())
			return
		}
		auditAction(a, r, "user.quota.reset", "user", fmt.Sprintf("%d", userID), map[string]any{"reason": reason})
		a.handleUserActionSuccess(w, r, userID, "配额已重置")
	case "quota_credit":
		if err := r.ParseForm(); err != nil {
			a.handleUserActionError(w, r, userID, "表单解析失败")
			return
		}
		creditBytes := int64(0)
		if raw := strings.TrimSpace(r.FormValue("credit_bytes")); raw != "" {
			creditBytes, _ = strconv.ParseInt(raw, 10, 64)
		} else {
			gb, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("credit_gb")), 10, 64)
			if gb > 0 {
				creditBytes = gb * 1024 * 1024 * 1024
			}
		}
		if creditBytes <= 0 {
			a.handleUserActionError(w, r, userID, "补偿流量必须大于 0")
			return
		}
		if err := a.users().CreditQuota(r.Context(), userID, creditBytes); err != nil {
			a.handleUserActionError(w, r, userID, "补偿失败: "+err.Error())
			return
		}
		auditAction(a, r, "user.quota.credit", "user", fmt.Sprintf("%d", userID), map[string]any{"bytes": creditBytes})
		a.handleUserActionSuccess(w, r, userID, "已补偿流量")
	case "plan_extend":
		if err := r.ParseForm(); err != nil {
			a.handleUserActionError(w, r, userID, "表单解析失败")
			return
		}
		days, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("extend_days")))
		if days <= 0 {
			a.handleUserActionError(w, r, userID, "延期天数必须大于 0")
			return
		}
		if err := a.users().ExtendPlan(r.Context(), userID, days); err != nil {
			a.handleUserActionError(w, r, userID, "延期失败: "+err.Error())
			return
		}
		auditAction(a, r, "user.plan.extend", "user", fmt.Sprintf("%d", userID), map[string]any{"days": days})
		a.handleUserActionSuccess(w, r, userID, "已延期")
	case "node_access":
		if err := r.ParseForm(); err != nil {
			a.handleUserActionError(w, r, userID, "表单解析失败")
			return
		}
		var nodeIDs []int64
		for _, raw := range r.Form["node_ids"] {
			if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
				nodeIDs = append(nodeIDs, id)
			}
		}
		if err := a.users().SetNodeAccess(r.Context(), userID, nodeIDs); err != nil {
			a.handleUserActionError(w, r, userID, "更新节点权限失败: "+err.Error())
			return
		}
		auditAction(a, r, "user.node_access.set", "user", fmt.Sprintf("%d", userID), map[string]any{"node_ids": nodeIDs})
		a.handleUserActionSuccess(w, r, userID, "节点权限已更新")
	default:
		http.NotFound(w, r)
	}
}

// StartWorkers runs all background workers until ctx is canceled, then waits
// for every worker goroutine to exit (including the metric-history shutdown
// drain) before returning. Callers that need a graceful shutdown should wait
// for StartWorkers to return before closing the DB.
func (a *App) StartWorkers(ctx context.Context) {
	a.ensureServices()
	var wg sync.WaitGroup
	spawn := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}
	spawn(func() { a.hostMonitor.Start(ctx, a.cfg.HostMetricsInterval(), a.store.GetTrafficTotals) })
	spawn(func() { a.startHostNetMonthlyRecorder(ctx) })
	spawn(func() { a.startNodeReconciler(ctx) })
	spawn(func() { a.startNodeJobTimeoutSweeper(ctx) })
	spawn(func() { a.startOpsSnapshotPublisher(ctx) })
	spawn(func() { a.metricHistoryQueue.Run(ctx) })
	defer wg.Wait()
	if err := a.ops().WarmUp(ctx); err != nil {
		log.Printf("ops cache warmup error: %v", err)
	}
	// Delta users-sync rollout: every enabled node without a proven schema-1
	// baseline gets exactly one forced full backfill (idempotent via the
	// per-node backfill marker). Must not depend on operator action.
	if err := a.nodes().EnqueueUsersSyncBaselineBackfill(ctx); err != nil {
		log.Printf("users_sync baseline backfill error: %v", err)
	}

	interval := 5 * time.Second
	quotaInterval := time.Duration(a.cfg.QuotaSweepSec) * time.Second
	if quotaInterval < 5*time.Second {
		quotaInterval = 30 * time.Second
	}

	lastQuotaAt := time.Time{}
	lastPruneAt := time.Time{}
	lastOpsAlertsAt := time.Time{}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UTC()
			if lastQuotaAt.IsZero() || now.Sub(lastQuotaAt) >= quotaInterval {
				lastQuotaAt = now
				if err := a.users().RefreshLifecycleState(ctx); err != nil {
					log.Printf("lifecycle refresh error: %v", err)
				}
				if err := a.usage().EnforceIPLimits(ctx); err != nil {
					log.Printf("enforce ip limit error: %v", err)
				}
				if err := a.nodes().CleanupStaleNodes(ctx); err != nil {
					log.Printf("cleanup stale nodes error: %v", err)
				}
			}
			pruneInterval := time.Duration(a.cfg.PruneEverySec) * time.Second
			if pruneInterval <= 0 {
				pruneInterval = time.Hour
			}
			if pruneInterval < 5*time.Minute {
				pruneInterval = 5 * time.Minute
			}
			if lastPruneAt.IsZero() || now.Sub(lastPruneAt) >= pruneInterval {
				lastPruneAt = now
				if err := a.pruneOnce(ctx, now); err != nil {
					log.Printf("prune error: %v", err)
				}
			}
			if lastOpsAlertsAt.IsZero() || now.Sub(lastOpsAlertsAt) >= 30*time.Second {
				lastOpsAlertsAt = now
				if err := a.syncOpsAlerts(ctx, now); err != nil {
					log.Printf("ops alert sync error: %v", err)
				}
			}
			if err := a.dispatchPendingAlerts(ctx); err != nil {
				log.Printf("alert dispatch error: %v", err)
			}
		}
	}
}

func (a *App) pruneOnce(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}

	if a.cfg.XrayAccessRetentionDays > 0 {
		cutoff := now.AddDate(0, 0, -a.cfg.XrayAccessRetentionDays)
		if n, err := a.store.PruneTrafficEvents(ctx, "xray-access", cutoff, 2000); err != nil {
			return err
		} else if n > 0 {
			log.Printf("pruned traffic_events source=xray-access deleted=%d cutoff=%s", n, cutoff.UTC().Format(time.RFC3339))
		}
	}
	if a.cfg.XrayStatsRetentionDays > 0 {
		cutoff := now.AddDate(0, 0, -a.cfg.XrayStatsRetentionDays)
		if n, err := a.store.PruneTrafficEvents(ctx, "xray-stats", cutoff, 2000); err != nil {
			return err
		} else if n > 0 {
			log.Printf("pruned traffic_events source=xray-stats deleted=%d cutoff=%s", n, cutoff.UTC().Format(time.RFC3339))
		}
	}
	if a.cfg.OnlineSessionsRetentionDays > 0 {
		cutoff := now.AddDate(0, 0, -a.cfg.OnlineSessionsRetentionDays)
		if n, err := a.store.PruneOnlineSessions(ctx, cutoff, 2000); err != nil {
			return err
		} else if n > 0 {
			log.Printf("pruned online_sessions deleted=%d cutoff=%s", n, cutoff.UTC().Format(time.RFC3339))
		}
	}
	if a.cfg.UsageEventKeyRetentionDays > 0 {
		// Floor at 3 days: keys guard idempotency against agent replays
		// (26h backdate cap + disk-queue flush delay), so aggressive values
		// must not reopen the duplicate-usage window.
		days := a.cfg.UsageEventKeyRetentionDays
		if days < 3 {
			days = 3
		}
		cutoff := now.AddDate(0, 0, -days)
		if n, err := a.store.PruneUsageEventKeys(ctx, cutoff, 2000); err != nil {
			return err
		} else if n > 0 {
			log.Printf("pruned usage_event_keys deleted=%d cutoff=%s", n, cutoff.UTC().Format(time.RFC3339))
		}
	}
	counts, err := a.store.CleanupOpsData(ctx, now, a.cfg.NodeMetricSampleRetentionDays, a.cfg.NodeMetricDetailRetentionHours, a.cfg.NodeProbeResultRetentionDays, a.cfg.OpsAlertResolvedRetentionDays, 5000)
	if err != nil {
		return err
	}
	if counts.MetricSamples > 0 || counts.MetricDetails > 0 || counts.ProbeResults > 0 || counts.OpsAlerts > 0 {
		log.Printf("pruned ops data metric_samples=%d metric_details=%d probe_results=%d ops_alerts=%d", counts.MetricSamples, counts.MetricDetails, counts.ProbeResults, counts.OpsAlerts)
	}
	if a.cfg.NodeJobTerminalRetentionDays > 0 {
		cutoff := now.AddDate(0, 0, -a.cfg.NodeJobTerminalRetentionDays)
		if n, err := a.store.PruneTerminalNodeJobs(ctx, cutoff, 2000); err != nil {
			return err
		} else if n > 0 {
			log.Printf("pruned node_jobs deleted=%d cutoff=%s", n, cutoff.UTC().Format(time.RFC3339))
		}
	}
	if a.cfg.AuditLogRetentionDays > 0 {
		cutoff := now.AddDate(0, 0, -a.cfg.AuditLogRetentionDays)
		if n, err := a.store.PruneAuditLogs(ctx, cutoff, 2000); err != nil {
			return err
		} else if n > 0 {
			log.Printf("pruned audit_logs deleted=%d cutoff=%s", n, cutoff.UTC().Format(time.RFC3339))
		}
	}
	if a.cfg.AlertSentRetentionDays > 0 {
		cutoff := now.AddDate(0, 0, -a.cfg.AlertSentRetentionDays)
		if n, err := a.store.PruneSentAlerts(ctx, cutoff, 2000); err != nil {
			return err
		} else if n > 0 {
			log.Printf("pruned alerts deleted=%d cutoff=%s", n, cutoff.UTC().Format(time.RFC3339))
		}
	}
	if a.cfg.EnforcementLogRetentionDays > 0 {
		cutoff := now.AddDate(0, 0, -a.cfg.EnforcementLogRetentionDays)
		if n, err := a.store.PruneEnforcementLogs(ctx, cutoff, 2000); err != nil {
			return err
		} else if n > 0 {
			log.Printf("pruned enforcement_logs deleted=%d cutoff=%s", n, cutoff.UTC().Format(time.RFC3339))
		}
	}
	if a.cfg.QuotaWindowRetentionDays > 0 {
		// Floor at 35 days so the current month-cycle window can never be
		// pruned out from under quota enforcement.
		days := a.cfg.QuotaWindowRetentionDays
		if days < 35 {
			days = 35
		}
		cutoff := now.AddDate(0, 0, -days)
		if n, err := a.store.PruneQuotaWindows(ctx, cutoff, 2000); err != nil {
			return err
		} else if n > 0 {
			log.Printf("pruned quota_windows deleted=%d cutoff=%s", n, cutoff.UTC().Format(time.RFC3339))
		}
	}
	if n, err := a.store.DeleteExpiredAdminSessions(ctx, now); err != nil {
		return err
	} else if n > 0 {
		log.Printf("pruned admin_sessions deleted=%d", n)
	}
	return nil
}

func (a *App) startHostNetMonthlyRecorder(ctx context.Context) {
	// Separate from HostMonitor sampling: we need durable, natural-month aggregates.
	interval := 10 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	recordOnce := func() {
		totals, source, err := hostnet.ReadTotals(ctx, a.cfg.HostProcPath)
		if err != nil {
			return
		}
		_ = a.store.RecordHostNetTotals(ctx, totals.RX, totals.TX, source, time.Now())
	}

	recordOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			recordOnce()
		}
	}
}

func (a *App) requestUsersSync(ctx context.Context) {
	a.requestUsersSyncWithThrottle(ctx, true)
}

func (a *App) requestUsersSyncNow(ctx context.Context) {
	a.requestUsersSyncWithThrottle(ctx, false)
}

func (a *App) requestUsersSyncWithThrottle(ctx context.Context, throttle bool) {
	// Throttle: enforcement/usage paths may call this; do not enqueue too often.
	a.usersSyncMu.Lock()
	defer a.usersSyncMu.Unlock()
	now := time.Now().UTC()
	if throttle && !a.lastUsersSyncReq.IsZero() && now.Sub(a.lastUsersSyncReq) < 15*time.Second {
		return
	}
	a.lastUsersSyncReq = now
	if err := a.nodes().EnqueueUsersSyncForEnabledNodes(ctx); err != nil {
		log.Printf("list nodes for users_sync: %v", err)
	}
}

func (a *App) requestUsersSyncForNodeNow(ctx context.Context, nodeID int64) {
	if nodeID <= 0 {
		return
	}
	if err := a.nodes().EnqueueUsersSyncForNode(ctx, nodeID); err != nil {
		log.Printf("enqueue users_sync for node=%d: %v", nodeID, err)
	}
}

// Satisfy service.SyncRequester — delegates to the throttled unexported variants.
func (a *App) RequestUsersSync(ctx context.Context)    { a.requestUsersSync(ctx) }
func (a *App) RequestUsersSyncNow(ctx context.Context) { a.requestUsersSyncNow(ctx) }
func (a *App) RequestUsersSyncForNodeNow(ctx context.Context, nodeID int64) {
	a.requestUsersSyncForNodeNow(ctx, nodeID)
}
func (a *App) RequestManagedXrayReload(ctx context.Context) {
	if err := a.nodes().EnqueueManagedXrayReloadForEnabledNodes(ctx); err != nil {
		log.Printf("enqueue managed xray reload: %v", err)
	}
}

func (a *App) syncUsersAndMaybeRestartManagedXray(ctx context.Context, restartManagedXray bool) {
	if restartManagedXray {
		a.RequestManagedXrayReload(ctx)
	}
	a.requestUsersSyncNow(ctx)
}

func (a *App) listUsersWithOnlineStats(ctx context.Context) ([]repo.User, error) {
	return a.users().ListWithOnlineStats(ctx, a.cfg.OnlineDisplayWindowSec)
}

func (a *App) renderIndex(w http.ResponseWriter, r *http.Request, errMsg string) {
	users, err := a.listUsersWithOnlineStats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	nodes, _ := a.nodes().List(r.Context())
	current, _ := a.currentAdmin(r)
	data := PageData{
		CurrentAdmin: current,
		ActivePage:   "users",
		CSRFToken:    a.csrfTokenFor(r),
		Content: indexData{
			Users: users,
			Nodes: nodes,
			Error: errMsg,
		},
	}
	if err := a.pages["users"].ExecuteTemplate(w, "base.tmpl", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) renderUserDetail(w http.ResponseWriter, r *http.Request, userID int64, errMsg string) {
	detail, err := a.users().GetDetail(r.Context(), userID, 120, a.cfg.OnlineDisplayWindowSec)
	if err != nil {
		if errors.Is(err, repo.ErrUserNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	current, _ := a.currentAdmin(r)
	subURL := ""
	if detail.SubscriptionToken != nil {
		subURL = subscription.BuildSubscriptionURL(a.cfg.SubBaseURL, detail.SubscriptionToken.Token, "v2rayn")
	}
	data := PageData{
		CurrentAdmin: current,
		ActivePage:   "users",
		CSRFToken:    a.csrfTokenFor(r),
		Content: userDetailData{
			User:               detail.User,
			SubscriptionURL:    subURL,
			AccessEvents:       detail.AccessEvents,
			OnlineSessions:     detail.OnlineSessions,
			OnlineDeviceCount:  detail.OnlineDeviceCount,
			OnlineSessionCount: detail.OnlineSessionCount,
			OnlineWindowSec:    a.cfg.OnlineDisplayWindowSec,
			AllNodes:           detail.AllNodes,
			AllowedNodeIDs:     detail.AllowedNodeIDs,
			Error:              errMsg,
		},
	}
	if err := a.pages["user_detail"].ExecuteTemplate(w, "base.tmpl", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) renderUsersTableOnly(w http.ResponseWriter, r *http.Request) {
	users, err := a.listUsersWithOnlineStats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.pages["users_table"].ExecuteTemplate(w, "users_table.tmpl", users); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) renderUsersTableWithError(w http.ResponseWriter, r *http.Request, errMsg string) {
	if r.Header.Get("HX-Request") == "true" {
		setHXToast(w, "error", errMsg)
		a.renderUsersTableOnly(w, r)
		return
	}
	a.renderIndex(w, r, errMsg)
}

func (a *App) handleUserActionSuccess(w http.ResponseWriter, r *http.Request, userID int64, message string) {
	if r.Header.Get("HX-Request") == "true" {
		setHXToast(w, "success", message)
		a.renderUsersTableOnly(w, r)
		return
	}
	http.Redirect(w, r, "/users/"+strconv.FormatInt(userID, 10), http.StatusFound)
}

func (a *App) handleUserActionError(w http.ResponseWriter, r *http.Request, userID int64, message string) {
	if r.Header.Get("HX-Request") == "true" {
		a.renderUsersTableWithError(w, r, message)
		return
	}
	a.renderUserDetail(w, r, userID, message)
}
