package config

import (
	"os"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

type Config struct {
	Addr string
	// Optional host /proc mount path for host-level ops network telemetry when running inside a container.
	HostProcPath string
	// Panel agent mTLS listener (agent -> panel). When empty, the listener is disabled.
	PanelAgentMTLSAddr string
	// PanelAgentMTLSCACertPaths is a CSV list of CA cert paths used to verify node client certs.
	PanelAgentMTLSCACertPaths    string
	PanelAgentMTLSServerCertPath string
	PanelAgentMTLSServerKeyPath  string
	// PanelAgentMTLSSigningCACertPath/KeyPath signs node client certs during enroll/renew.
	// Best practice: use an intermediate CA key here; do not store root CA key on the server.
	PanelAgentMTLSSigningCACertPath string
	PanelAgentMTLSSigningCAKeyPath  string
	// Hardening: require issuing CA to be an intermediate (not self-signed) and key perms to be strict.
	PanelAgentMTLSSigningRequireIntermediate   bool
	PanelAgentMTLSSigningRequireStrictKeyPerms bool

	DBPath string
	// BackupDir is the local directory where on-demand SQLite backups are written.
	BackupDir string

	SubBaseURL string

	AdminUser string
	AdminPass string

	AdminSessionHours int
	AllowBasicAuth    bool
	// TrustedProxyCIDRs is a CSV list of reverse-proxy CIDRs whose forwarded
	// client IP headers may be trusted. Empty means forwarded headers are ignored.
	TrustedProxyCIDRs string

	APIKeyHeader string

	NodeReconcileEverySec   int
	NodeReconcileBackoffSec int
	NodeJobMaxAttempts      int
	NodeStaleDeleteAfterSec int
	// Node deployment helper defaults.
	NodeDefaultAgentImage   string
	NodeDefaultXrayImage    string
	NodeDefaultDeployDir    string
	NodeDefaultPanelMTLSURL string
	NodeEnrollCodeTTLMin    int

	ProxyHost      string
	ProxyPort      int
	ProxyType      string
	ProxySecurity  string
	ProxyFlow      string
	ProxySNI       string
	RealityFP      string
	RealityPublicK string
	RealityShortID string

	OnlineWindowSec                int
	OnlineDisplayWindowSec         int
	OpsSnapshotIntervalSec         int
	HostMetricsIntervalSec         int
	NodeMetricHistoryQueueCapacity int
	NodeMetricHistoryQueueDir      string
	NodeMetricHistoryQueueMaxBytes int64
	IPLimitStrikes                 int
	QuotaSweepSec                  int

	// Data retention + pruning.
	XrayAccessRetentionDays        int
	XrayStatsRetentionDays         int
	OnlineSessionsRetentionDays    int
	NodeMetricSampleRetentionDays  int
	NodeMetricDetailRetentionHours int
	NodeProbeResultRetentionDays   int
	OpsAlertResolvedRetentionDays  int
	PruneEverySec                  int

	TelegramBotToken     string
	TelegramAdminChatIDs string

	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	SMTPFrom string
	SMTPTo   string

	// PanelTZ drives panel-side natural-month host-net rollup + display.
	// Defaults to "UTC" (not process time.Local) for reproducibility.
	PanelTZ string

	EnableOpsV2 bool
}

func Load() Config {
	onlineWindowSec := envInt("ONLINE_WINDOW_SEC", 120)
	onlineDisplayWindowSec := envInt("ONLINE_DISPLAY_WINDOW_SEC", 0)
	if onlineDisplayWindowSec <= 0 {
		onlineDisplayWindowSec = onlineWindowSec
		if onlineDisplayWindowSec < 300 {
			onlineDisplayWindowSec = 300
		}
	}

	return Config{
		Addr:                                       env("ADDR", ":8080"),
		HostProcPath:                               env("HOST_PROC_PATH", ""),
		PanelAgentMTLSAddr:                         env("PANEL_AGENT_MTLS_ADDR", ""),
		PanelAgentMTLSCACertPaths:                  env("PANEL_AGENT_MTLS_CA_CERT_PATHS", ""),
		PanelAgentMTLSServerCertPath:               env("PANEL_AGENT_MTLS_SERVER_CERT_PATH", ""),
		PanelAgentMTLSServerKeyPath:                env("PANEL_AGENT_MTLS_SERVER_KEY_PATH", ""),
		PanelAgentMTLSSigningCACertPath:            env("PANEL_AGENT_MTLS_SIGNING_CA_CERT_PATH", ""),
		PanelAgentMTLSSigningCAKeyPath:             env("PANEL_AGENT_MTLS_SIGNING_CA_KEY_PATH", ""),
		PanelAgentMTLSSigningRequireIntermediate:   envBool("PANEL_AGENT_MTLS_SIGNING_REQUIRE_INTERMEDIATE", false),
		PanelAgentMTLSSigningRequireStrictKeyPerms: envBool("PANEL_AGENT_MTLS_SIGNING_REQUIRE_STRICT_KEY_PERMS", true),

		DBPath:     env("DB_PATH", "neutrino.db"),
		BackupDir:  env("BACKUP_DIR", "backups"),
		SubBaseURL: env("SUB_BASE_URL", "http://127.0.0.1:8080"),

		AdminUser: env("ADMIN_USER", "admin"),
		AdminPass: env("ADMIN_PASS", "admin123"),

		AdminSessionHours: envInt("ADMIN_SESSION_HOURS", 24),
		AllowBasicAuth:    envBool("ALLOW_BASIC_AUTH", false),
		TrustedProxyCIDRs: env("TRUSTED_PROXY_CIDRS", ""),
		APIKeyHeader:      env("API_KEY_HEADER", "X-API-Key"),

		NodeReconcileEverySec:   envInt("NODE_RECONCILE_EVERY_SEC", 30),
		NodeReconcileBackoffSec: envInt("NODE_RECONCILE_BACKOFF_SEC", 300),
		NodeJobMaxAttempts:      envInt("NODE_JOB_MAX_ATTEMPTS", 5),
		NodeStaleDeleteAfterSec: envInt("NODE_STALE_DELETE_AFTER_SEC", 0),
		NodeDefaultAgentImage:   env("NODE_DEFAULT_AGENT_IMAGE", "ghcr.io/neutrino-proxy/agent:latest"),
		// Prefer upstream official image with pinned tag (greenfield; breaking changes allowed).
		NodeDefaultXrayImage:    env("NODE_DEFAULT_XRAY_IMAGE", "ghcr.io/xtls/xray-core:26.2.6"),
		NodeDefaultDeployDir:    env("NODE_DEFAULT_DEPLOY_DIR", "/root/neutrino-node"),
		NodeDefaultPanelMTLSURL: env("NODE_DEFAULT_PANEL_MTLS_URL", ""),
		NodeEnrollCodeTTLMin:    envInt("NODE_ENROLL_CODE_TTL_MIN", 10),

		ProxyHost:      env("PROXY_PUBLIC_HOST", "example.com"),
		ProxyPort:      envInt("PROXY_PUBLIC_PORT", 443),
		ProxyType:      env("PROXY_TYPE", "tcp"),
		ProxySecurity:  env("PROXY_SECURITY", "reality"),
		ProxyFlow:      env("PROXY_FLOW", "xtls-rprx-vision"),
		ProxySNI:       env("PROXY_SNI", "www.microsoft.com"),
		RealityFP:      env("REALITY_FP", "chrome"),
		RealityPublicK: env("REALITY_PUBLIC_KEY", "REPLACE_WITH_REALITY_PUBLIC_KEY"),
		RealityShortID: env("REALITY_SHORT_ID", "REPLACE_WITH_SHORT_ID"),

		OnlineWindowSec:                onlineWindowSec,
		OnlineDisplayWindowSec:         onlineDisplayWindowSec,
		OpsSnapshotIntervalSec:         envInt("OPS_SNAPSHOT_INTERVAL_SEC", 2),
		HostMetricsIntervalSec:         envInt("HOST_METRICS_INTERVAL_SEC", 2),
		NodeMetricHistoryQueueCapacity: envInt("NODE_METRIC_HISTORY_QUEUE_CAPACITY", 4096),
		NodeMetricHistoryQueueDir:      env("NODE_METRIC_HISTORY_QUEUE_DIR", ""),
		NodeMetricHistoryQueueMaxBytes: envInt64("NODE_METRIC_HISTORY_QUEUE_MAX_BYTES", 64*1024*1024),
		IPLimitStrikes:                 envInt("IP_LIMIT_STRIKES", 3),
		QuotaSweepSec:                  envInt("QUOTA_SWEEP_SEC", 30),

		XrayAccessRetentionDays:        envInt("XRAY_ACCESS_RETENTION_DAYS", 7),
		XrayStatsRetentionDays:         envInt("XRAY_STATS_RETENTION_DAYS", 2),
		OnlineSessionsRetentionDays:    envInt("ONLINE_SESSIONS_RETENTION_DAYS", 1),
		NodeMetricSampleRetentionDays:  envInt("NODE_METRIC_SAMPLE_RETENTION_DAYS", 14),
		NodeMetricDetailRetentionHours: envInt("NODE_METRIC_DETAIL_RETENTION_HOURS", 72),
		NodeProbeResultRetentionDays:   envInt("NODE_PROBE_RESULT_RETENTION_DAYS", 30),
		OpsAlertResolvedRetentionDays:  envInt("OPS_ALERT_RESOLVED_RETENTION_DAYS", 90),
		PruneEverySec:                  envInt("PRUNE_EVERY_SEC", 3600),

		TelegramBotToken:     env("TELEGRAM_BOT_TOKEN", ""),
		TelegramAdminChatIDs: env("TELEGRAM_ADMIN_CHAT_IDS", ""),

		SMTPHost: env("SMTP_HOST", ""),
		SMTPPort: envInt("SMTP_PORT", 587),
		SMTPUser: env("SMTP_USER", ""),
		SMTPPass: env("SMTP_PASS", ""),
		SMTPFrom: env("SMTP_FROM", ""),
		SMTPTo:   env("SMTP_TO", ""),

		PanelTZ: env("PANEL_TZ", "UTC"),

		EnableOpsV2: envBool("ENABLE_OPS_V2", false),
	}
}

func (c Config) OpsSnapshotInterval() time.Duration {
	return intervalSeconds(c.OpsSnapshotIntervalSec, 2, 1) * time.Second
}

func (c Config) HostMetricsInterval() time.Duration {
	return intervalSeconds(c.HostMetricsIntervalSec, 2, 1) * time.Second
}

func intervalSeconds(value, fallback, min int) time.Duration {
	if fallback < min {
		fallback = min
	}
	if value <= 0 {
		value = fallback
	}
	if value < min {
		value = min
	}
	return time.Duration(value)
}

// PanelLocation returns the configured panel timezone, defaulting to UTC.
// Invalid TZ names fall back to UTC silently.
func (c Config) PanelLocation() *time.Location {
	name := strings.TrimSpace(c.PanelTZ)
	if name == "" || strings.EqualFold(name, "local") {
		return time.UTC
	}
	if loc, err := time.LoadLocation(name); err == nil && loc != nil {
		return loc
	}
	return time.UTC
}

func (c Config) HasDefaultAdminCredentials() bool {
	return strings.TrimSpace(c.AdminUser) == "admin" && strings.TrimSpace(c.AdminPass) == "admin123"
}

func (c Config) BasicAuthAllowed() bool {
	return c.AllowBasicAuth && !c.HasDefaultAdminCredentials()
}

func (c Config) HasUsableSubscriptionFallback() bool {
	return strings.TrimSpace(c.ProxyHost) != "" &&
		!strings.EqualFold(strings.TrimSpace(c.ProxyHost), "example.com") &&
		strings.TrimSpace(c.RealityPublicK) != "" &&
		strings.TrimSpace(c.RealityPublicK) != "REPLACE_WITH_REALITY_PUBLIC_KEY" &&
		strings.TrimSpace(c.RealityShortID) != "" &&
		strings.TrimSpace(c.RealityShortID) != "REPLACE_WITH_SHORT_ID"
}

func env(k, fallback string) string {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	return v
}

func envInt(k string, fallback int) int {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt64(k string, fallback int64) int64 {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(k string, fallback bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return parsed
}
