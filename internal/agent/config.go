package agent

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	// Local health endpoint (no auth). Not required for panel connectivity.
	AgentHTTPAddr string

	NodeID int64

	// Optional host /proc mount used to sample host-network counters from inside the container.
	HostProcPath string

	// Timezone used when parsing Xray access-log timestamps (which carry no offset).
	// Empty means time.Local, which matches the historical behavior.
	AccessLogTZ string

	// Timezone used to compute the natural-month key for node RX/TX rollups.
	// Empty falls back to time.Local, then UTC if time.Local is UTC.
	MonthTZ string

	// Panel URLs.
	PanelURL     string // HTTPS, used only for /enroll (no mTLS)
	PanelMTLSURL string // HTTPS mTLS listener, used for jobs/usage/report

	EnrollCode string

	// Agent -> panel mTLS client files.
	PanelMTLSCACertPath     string
	PanelMTLSClientCertPath string
	PanelMTLSClientKeyPath  string

	// mTLS client cert auto-renew.
	CertRenewEnabled         bool
	CertRenewBeforeDays      int
	CertRenewCheckEveryHours int
	CertRenewMinRetrySec     int
	CertRenewMaxRetrySec     int

	// xray
	XrayAPIAddr       string
	XrayInboundTag    string
	XrayFlow          string
	XrayAccessLogPath string
	// managed xray config apply (agent-local only; panel must not send commands/paths)
	XrayConfigPath     string
	XrayTestArgsJSON   string // JSON array, e.g. ["docker","exec","neutrino-xray","/usr/local/bin/xray","run","-test","-c","${NEUTRINO_CONFIG_PATH}"]
	XrayReloadArgsJSON string // JSON array, e.g. ["docker","restart","neutrino-xray"]

	// state + queue
	StatePath          string
	QueueDir           string
	QueueMaxBytes      int64
	PushBatchMaxEvents int

	// polling
	StatsPollSec         int
	AccessPollSec        int
	RuntimeReportSec     int
	StaticFactsReportSec int

	// access 0-byte sampling
	AccessZeroByteMaxPerMinPerUser     int
	AccessZeroByteMaxPerMinPerUserDest int
}

func ConfigFromEnv() Config {
	cfg := Config{
		AgentHTTPAddr:     getEnv("AGENT_HTTP_ADDR", "127.0.0.1:9090"),
		HostProcPath:      getEnv("HOST_PROC_PATH", ""),
		AccessLogTZ:       getEnv("AGENT_ACCESS_LOG_TZ", ""),
		MonthTZ:           getEnv("AGENT_MONTH_TZ", ""),
		XrayAPIAddr:       getEnv("XRAY_API_ADDR", "127.0.0.1:10085"),
		XrayInboundTag:    getEnv("XRAY_INBOUND_TAG", "vless-reality"),
		XrayFlow:          getEnv("XRAY_FLOW", "xtls-rprx-vision"),
		XrayAccessLogPath: getEnv("XRAY_ACCESS_LOG_PATH", "/var/log/xray/access.log"),
		// Match upstream Xray container default confdir (/usr/local/etc/xray).
		XrayConfigPath:     getEnv("XRAY_CONFIG_PATH", "/usr/local/etc/xray/config.json"),
		XrayTestArgsJSON:   getEnv("XRAY_TEST_ARGS_JSON", ""),
		XrayReloadArgsJSON: getEnv("XRAY_RELOAD_ARGS_JSON", ""),

		StatePath:          getEnv("STATE_PATH", "state.json"),
		QueueDir:           getEnv("QUEUE_DIR", ""),
		QueueMaxBytes:      getEnvInt64("QUEUE_MAX_BYTES", 200000000),
		PushBatchMaxEvents: getEnvInt("PUSH_BATCH_MAX_EVENTS", 500),

		PanelURL:     getEnv("PANEL_URL", ""),
		PanelMTLSURL: getEnv("PANEL_MTLS_URL", ""),
		EnrollCode:   getEnv("ENROLL_CODE", ""),

		PanelMTLSCACertPath:     getEnv("PANEL_MTLS_CA_CERT_PATH", "/data/mtls/ca-bundle.crt"),
		PanelMTLSClientCertPath: getEnv("PANEL_MTLS_CLIENT_CERT_PATH", "/data/mtls/node.crt"),
		PanelMTLSClientKeyPath:  getEnv("PANEL_MTLS_CLIENT_KEY_PATH", "/data/mtls/node.key"),

		CertRenewEnabled:         getEnvBool("CERT_RENEW_ENABLED", true),
		CertRenewBeforeDays:      getEnvInt("CERT_RENEW_BEFORE_DAYS", 7),
		CertRenewCheckEveryHours: getEnvInt("CERT_RENEW_CHECK_EVERY_HOURS", 12),
		CertRenewMinRetrySec:     getEnvInt("CERT_RENEW_MIN_RETRY_SEC", 30),
		CertRenewMaxRetrySec:     getEnvInt("CERT_RENEW_MAX_RETRY_SEC", 1800),

		StatsPollSec:         getEnvInt("STATS_POLL_SEC", 5),
		AccessPollSec:        getEnvInt("ACCESS_POLL_SEC", 2),
		RuntimeReportSec:     getEnvInt("AGENT_RUNTIME_REPORT_SEC", 2),
		StaticFactsReportSec: getEnvInt("AGENT_STATIC_FACTS_REPORT_SEC", 1800),

		AccessZeroByteMaxPerMinPerUser:     getEnvInt("ACCESS_ZERO_BYTE_MAX_PER_MIN_PER_USER", 30),
		AccessZeroByteMaxPerMinPerUserDest: getEnvInt("ACCESS_ZERO_BYTE_MAX_PER_MIN_PER_USER_DEST", 5),
	}

	if nodeIDStr := getEnv("NODE_ID", ""); nodeIDStr != "" {
		if id, err := strconv.ParseInt(nodeIDStr, 10, 64); err == nil {
			cfg.NodeID = id
		}
	}
	if strings.TrimSpace(cfg.QueueDir) == "" {
		cfg.QueueDir = filepath.Join(filepath.Dir(cfg.StatePath), "queue")
	}
	return cfg
}

func getEnv(key, defaultVal string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return defaultVal
}

func getEnvInt64(key string, defaultVal int64) int64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			return i
		}
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return defaultVal
}

// AccessLogLocation returns the timezone used to parse Xray access-log timestamps,
// defaulting to time.Local when AccessLogTZ is empty or invalid.
func (c Config) AccessLogLocation() *time.Location {
	if name := strings.TrimSpace(c.AccessLogTZ); name != "" {
		if loc, err := time.LoadLocation(name); err == nil && loc != nil {
			return loc
		}
	}
	return time.Local
}

var monthLocationUTCWarnOnce sync.Once

// MonthLocation returns the timezone used to compute natural-month buckets for node
// RX/TX rollups. Falls back to time.Local, then UTC (with a one-time warning) so the
// rollup key is stable even when the host has no configured timezone.
func (c Config) MonthLocation() *time.Location {
	if name := strings.TrimSpace(c.MonthTZ); name != "" {
		if loc, err := time.LoadLocation(name); err == nil && loc != nil {
			return loc
		}
	}
	if time.Local != nil && time.Local != time.UTC {
		return time.Local
	}
	monthLocationUTCWarnOnce.Do(func() {
		log.Printf("agent: monthly-net timezone falling back to UTC (set AGENT_MONTH_TZ to override)")
	})
	return time.UTC
}
