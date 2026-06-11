package db

import (
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

var reSQLIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// isValidSQLIdent reports whether s is safe to splice into a SQL statement as a
// table or column identifier. It does not accept quoted identifiers, just bare
// ASCII word characters starting with a letter or underscore.
func isValidSQLIdent(s string) bool {
	if s == "" {
		return false
	}
	return reSQLIdent.MatchString(s)
}

func Open(path string) (*sql.DB, error) {
	return sql.Open("sqlite3", sqliteDSN(path))
}

func sqliteDSN(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return path
	}

	if !strings.HasPrefix(path, "file:") {
		path = "file:" + path
	}

	base, rawQuery, hasQuery := strings.Cut(path, "?")
	if !hasQuery {
		rawQuery = ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		sep := "?"
		if hasQuery {
			sep = "&"
		}
		return path + sep + "_foreign_keys=on&_busy_timeout=5000"
	}
	values.Del("_fk")
	values.Set("_foreign_keys", "on")
	if strings.TrimSpace(values.Get("_busy_timeout")) == "" {
		values.Set("_busy_timeout", "15000")
	}
	if strings.TrimSpace(values.Get("_txlock")) == "" {
		values.Set("_txlock", "immediate")
	}
	return base + "?" + values.Encode()
}

func Migrate(conn *sql.DB) error {
	stmts := []string{
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA foreign_keys = ON;`,
		`CREATE TABLE IF NOT EXISTS admin_accounts (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS admin_sessions (
			session_id TEXT PRIMARY KEY,
			username TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE,
			monthly_limit_bytes INTEGER NOT NULL,
			counting_mode TEXT NOT NULL CHECK(counting_mode IN ('single','double')),
			plan_days INTEGER NOT NULL DEFAULT 30, -- account validity in days
			quota_cycle TEXT NOT NULL DEFAULT 'month' CHECK(quota_cycle IN ('day','week','month')),
			quota_tz TEXT NOT NULL DEFAULT 'Asia/Shanghai',
			device_limit INTEGER NOT NULL DEFAULT 2,
			status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','disabled','expired','over_limit','over_ip_limit')),
			created_at TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			removed_at TEXT
		);`,
		`CREATE TABLE IF NOT EXISTS proxy_links (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			protocol TEXT NOT NULL,
			uuid TEXT NOT NULL,
			inbound_tag TEXT NOT NULL,
			link TEXT NOT NULL,
			active INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS traffic_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			node_id INTEGER REFERENCES nodes(id) ON DELETE SET NULL,
			direction TEXT NOT NULL CHECK(direction IN ('inbound','outbound')),
			bytes INTEGER NOT NULL CHECK(bytes >= 0),
			event_at TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT 'manual',
			source_event_id TEXT NOT NULL,
			target_host TEXT,
			target_ip TEXT,
			target_port INTEGER,
			sni TEXT,
			destination TEXT,
			client_ip TEXT,
			inbound_tag TEXT
		);`,
		`CREATE TABLE IF NOT EXISTS usage_event_keys (
			source TEXT NOT NULL,
			source_event_id TEXT NOT NULL,
			user_id INTEGER,
			node_id INTEGER,
			event_at TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY(source, source_event_id)
		);`,
		`CREATE TABLE IF NOT EXISTS traffic_rollups_hourly (
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			node_id INTEGER NOT NULL,
			bucket_start TEXT NOT NULL,
			inbound_bytes INTEGER NOT NULL DEFAULT 0,
			outbound_bytes INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(user_id, node_id, bucket_start)
		);`,
		`CREATE TABLE IF NOT EXISTS traffic_stats (
			user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			inbound_bytes INTEGER NOT NULL DEFAULT 0,
			outbound_bytes INTEGER NOT NULL DEFAULT 0,
			last_seen_at TEXT,
			last_target_host TEXT,
			last_target_ip TEXT,
			last_target_port INTEGER
		);`,
		`CREATE TABLE IF NOT EXISTS quota_windows (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			window_start TEXT NOT NULL,
			window_end TEXT NOT NULL,
			cycle_type TEXT NOT NULL DEFAULT 'month' CHECK(cycle_type IN ('day','week','month')),
			cycle_key TEXT NOT NULL DEFAULT '',
			reset_reason TEXT,
			inbound_bytes INTEGER NOT NULL DEFAULT 0,
			outbound_bytes INTEGER NOT NULL DEFAULT 0,
			effective_bytes INTEGER NOT NULL DEFAULT 0,
			credit_bytes INTEGER NOT NULL DEFAULT 0,
			over_limit_at TEXT,
			alert_80_sent_at TEXT,
			alert_90_sent_at TEXT,
			closed_at TEXT,
			UNIQUE(user_id, window_start)
		);`,
		`CREATE TABLE IF NOT EXISTS enforcement_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			action TEXT NOT NULL,
			reason TEXT NOT NULL,
			detail TEXT,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS xray_state (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS subscription_tokens (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
			token TEXT NOT NULL UNIQUE,
			enabled INTEGER NOT NULL DEFAULT 1,
			expires_at TEXT,
			last_used_at TEXT,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS nodes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			core_type TEXT NOT NULL CHECK(core_type IN ('xray','singbox')),
			protocol TEXT NOT NULL CHECK(protocol IN ('vless_reality','hysteria2','tuic')),
			host TEXT NOT NULL,
			port INTEGER NOT NULL,
			transport TEXT,
			security TEXT,
			flow TEXT,
			sni TEXT,
			fp TEXT,
			public_key TEXT,
			short_id TEXT,
			extra_json TEXT,
			enabled INTEGER NOT NULL DEFAULT 1,
			managed INTEGER NOT NULL DEFAULT 0,
			agent_url TEXT,
			last_seen_at TEXT,
			last_error TEXT,
			desired_users_version TEXT NOT NULL DEFAULT '',
			applied_users_version TEXT NOT NULL DEFAULT '',
			desired_xray_version TEXT NOT NULL DEFAULT '',
			applied_xray_version TEXT NOT NULL DEFAULT '',
			last_job_error_kind TEXT,
			last_job_error_at TEXT,
			users_sync_baseline_schema INTEGER NOT NULL DEFAULT 0,
			users_sync_delta_ready_at TEXT,
			users_sync_baseline_backfill_at TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS node_user_sync_snapshots (
			node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			version TEXT NOT NULL,
			users_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY(node_id, version)
		);`,
		`CREATE TABLE IF NOT EXISTS node_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			kind TEXT NOT NULL CHECK(kind IN ('users_sync','xray_apply','xray_rollback','probe_dns','probe_ping','probe_tcp','probe_http')),
			desired_version TEXT NOT NULL DEFAULT '',
			payload_json TEXT NOT NULL DEFAULT '{}',
			status TEXT NOT NULL CHECK(status IN ('pending','running','succeeded','failed','canceled')),
			attempts INTEGER NOT NULL DEFAULT 0,
			retryable INTEGER NOT NULL DEFAULT 1,
			http_status INTEGER,
			timeout_sec INTEGER,
			correlation_id TEXT,
			last_error TEXT,
			result_json TEXT,
			created_at TEXT NOT NULL,
			started_at TEXT,
			finished_at TEXT
		);`,
		`CREATE TABLE IF NOT EXISTS node_runtime_metrics (
			node_id INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
			cpu_percent REAL NOT NULL DEFAULT 0,
			load1 REAL NOT NULL DEFAULT 0,
			load5 REAL NOT NULL DEFAULT 0,
			load15 REAL NOT NULL DEFAULT 0,
			memory_bytes INTEGER NOT NULL DEFAULT 0,
			memory_total_bytes INTEGER NOT NULL DEFAULT 0,
			memory_available_bytes INTEGER NOT NULL DEFAULT 0,
			swap_used_bytes INTEGER NOT NULL DEFAULT 0,
			swap_total_bytes INTEGER NOT NULL DEFAULT 0,
			inbound_bps REAL NOT NULL DEFAULT 0,
			outbound_bps REAL NOT NULL DEFAULT 0,
			disk_total_bytes INTEGER NOT NULL DEFAULT 0,
			disk_used_bytes INTEGER NOT NULL DEFAULT 0,
			disk_free_bytes INTEGER NOT NULL DEFAULT 0,
			disk_used_percent REAL NOT NULL DEFAULT 0,
			disk_read_bps REAL NOT NULL DEFAULT 0,
			disk_write_bps REAL NOT NULL DEFAULT 0,
			tcp_connections INTEGER NOT NULL DEFAULT 0,
			udp_connections INTEGER NOT NULL DEFAULT 0,
			process_count INTEGER NOT NULL DEFAULT 0,
			uptime_sec INTEGER NOT NULL DEFAULT 0,
			system_uptime_sec INTEGER NOT NULL DEFAULT 0,
			agent_uptime_sec INTEGER NOT NULL DEFAULT 0,
			boot_time TEXT,
			queue_bytes INTEGER NOT NULL DEFAULT 0,
			queue_batches INTEGER NOT NULL DEFAULT 0,
			quarantined_batches INTEGER NOT NULL DEFAULT 0,
			goroutines INTEGER NOT NULL DEFAULT 0,
			agent_version TEXT NOT NULL DEFAULT '',
			xray_version TEXT NOT NULL DEFAULT '',
			xray_config_version TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS node_metric_samples (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			sampled_at TEXT NOT NULL,
			cpu_percent REAL NOT NULL DEFAULT 0,
			load1 REAL NOT NULL DEFAULT 0,
			load5 REAL NOT NULL DEFAULT 0,
			load15 REAL NOT NULL DEFAULT 0,
			memory_used_bytes INTEGER NOT NULL DEFAULT 0,
			memory_total_bytes INTEGER NOT NULL DEFAULT 0,
			memory_available_bytes INTEGER NOT NULL DEFAULT 0,
			swap_used_bytes INTEGER NOT NULL DEFAULT 0,
			swap_total_bytes INTEGER NOT NULL DEFAULT 0,
			disk_total_bytes INTEGER NOT NULL DEFAULT 0,
			disk_used_bytes INTEGER NOT NULL DEFAULT 0,
			disk_free_bytes INTEGER NOT NULL DEFAULT 0,
			disk_used_percent REAL NOT NULL DEFAULT 0,
			disk_read_bps REAL NOT NULL DEFAULT 0,
			disk_write_bps REAL NOT NULL DEFAULT 0,
			inbound_bps REAL NOT NULL DEFAULT 0,
			outbound_bps REAL NOT NULL DEFAULT 0,
			tcp_connections INTEGER NOT NULL DEFAULT 0,
			udp_connections INTEGER NOT NULL DEFAULT 0,
			process_count INTEGER NOT NULL DEFAULT 0,
			system_uptime_sec INTEGER NOT NULL DEFAULT 0,
			agent_uptime_sec INTEGER NOT NULL DEFAULT 0,
			boot_time TEXT,
			queue_bytes INTEGER NOT NULL DEFAULT 0,
			queue_batches INTEGER NOT NULL DEFAULT 0,
			goroutines INTEGER NOT NULL DEFAULT 0
		);`,
		`CREATE TABLE IF NOT EXISTS node_metric_details (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			sampled_at TEXT NOT NULL,
			data_json TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS node_static_facts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			reported_at TEXT NOT NULL,
			data_hash TEXT NOT NULL,
			os_name TEXT NOT NULL DEFAULT '',
			os_version TEXT NOT NULL DEFAULT '',
			kernel TEXT NOT NULL DEFAULT '',
			kernel_version TEXT NOT NULL DEFAULT '',
			arch TEXT NOT NULL DEFAULT '',
			hostname TEXT NOT NULL DEFAULT '',
			virtualization TEXT NOT NULL DEFAULT '',
			cpu_model TEXT NOT NULL DEFAULT '',
			cpu_physical_cores INTEGER NOT NULL DEFAULT 0,
			cpu_logical_cores INTEGER NOT NULL DEFAULT 0,
			agent_version TEXT NOT NULL DEFAULT '',
			xray_version TEXT NOT NULL DEFAULT '',
			facts_json TEXT NOT NULL DEFAULT '{}',
			UNIQUE(node_id, data_hash)
		);`,
		`CREATE TABLE IF NOT EXISTS node_monthly_usage (
			node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			month_key TEXT NOT NULL,
			timezone_name TEXT NOT NULL DEFAULT '',
			rx_bytes INTEGER NOT NULL DEFAULT 0,
			tx_bytes INTEGER NOT NULL DEFAULT 0,
			baseline_rx_total INTEGER NOT NULL DEFAULT 0,
			baseline_tx_total INTEGER NOT NULL DEFAULT 0,
			last_rx_total INTEGER NOT NULL DEFAULT 0,
			last_tx_total INTEGER NOT NULL DEFAULT 0,
			counter_source TEXT NOT NULL DEFAULT '',
			last_reported_at TEXT,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(node_id, month_key)
		);`,
		`CREATE TABLE IF NOT EXISTS node_desired_states (
			node_id INTEGER NOT NULL PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
			kind TEXT NOT NULL CHECK(kind IN ('xray_apply')),
			desired_version TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS node_enroll_codes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			node_id INTEGER NOT NULL UNIQUE REFERENCES nodes(id) ON DELETE CASCADE,
			code TEXT NOT NULL UNIQUE,
			expires_at TEXT NOT NULL,
			used_at TEXT,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS node_cert_pins (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			purpose TEXT NOT NULL CHECK(purpose IN ('agent_client')),
			spki_sha256 TEXT NOT NULL,
			cert_sha256 TEXT NOT NULL,
			serial_hex TEXT NOT NULL,
			subject TEXT NOT NULL,
			issuer TEXT NOT NULL,
			not_before TEXT NOT NULL,
			not_after TEXT NOT NULL,
			created_at TEXT NOT NULL,
			last_seen_at TEXT,
			revoked_at TEXT,
			revoked_reason TEXT,
			UNIQUE(node_id, purpose, cert_sha256)
		);`,
		`CREATE TABLE IF NOT EXISTS user_node_access (
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			created_at TEXT NOT NULL,
			PRIMARY KEY(user_id, node_id)
		);`,
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			actor_type TEXT NOT NULL CHECK(actor_type IN ('admin','apikey','node')),
			actor TEXT NOT NULL,
			action TEXT NOT NULL,
			target_type TEXT NOT NULL,
			target_id TEXT NOT NULL,
			detail_json TEXT,
			client_ip TEXT,
			user_agent TEXT,
			request_id TEXT,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS online_sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			client_ip TEXT NOT NULL,
			node_id INTEGER REFERENCES nodes(id) ON DELETE SET NULL,
			first_seen_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL,
			UNIQUE(user_id, client_ip, node_id)
		);`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			key_hash TEXT NOT NULL UNIQUE,
			scopes TEXT NOT NULL,
			node_id INTEGER REFERENCES nodes(id) ON DELETE SET NULL,
			expires_at TEXT,
			last_used_at TEXT,
			revoked_at TEXT,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS backups (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			file_path TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			sha256 TEXT NOT NULL,
			storage TEXT NOT NULL CHECK(storage IN ('local','s3')),
			object_key TEXT,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS alerts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
			type TEXT NOT NULL CHECK(type IN ('quota','ip','system')),
			threshold TEXT,
			channel TEXT NOT NULL,
			message TEXT NOT NULL,
			dedupe_key TEXT NOT NULL UNIQUE,
			sent_at TEXT,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS telegram_bindings (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
			bind_code TEXT NOT NULL UNIQUE,
			bind_code_expires_at TEXT,
			telegram_chat_id INTEGER UNIQUE,
			telegram_user_id INTEGER,
			telegram_username TEXT,
			verified_at TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS telegram_bind_attempts (
			chat_id INTEGER NOT NULL,
			code TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			window_start TEXT NOT NULL,
			last_attempt_at TEXT NOT NULL,
			PRIMARY KEY(chat_id, code)
		);`,
		`CREATE TABLE IF NOT EXISTS host_net_monthly_usage (
			month_key TEXT PRIMARY KEY,
			rx_bytes INTEGER NOT NULL DEFAULT 0,
			tx_bytes INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS node_probe_results (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			kind TEXT NOT NULL,
			target TEXT NOT NULL,
			success INTEGER NOT NULL DEFAULT 0,
			latency_ms REAL NOT NULL DEFAULT 0,
			status_code INTEGER,
			error TEXT NOT NULL DEFAULT '',
			checked_at TEXT NOT NULL,
			source_job_id INTEGER REFERENCES node_jobs(id) ON DELETE SET NULL
		);`,
		`CREATE TABLE IF NOT EXISTS node_metadata (
			node_id INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
			provider TEXT NOT NULL DEFAULT '',
			region TEXT NOT NULL DEFAULT '',
			country TEXT NOT NULL DEFAULT '',
			city TEXT NOT NULL DEFAULT '',
			latitude REAL,
			longitude REAL,
			tags_json TEXT NOT NULL DEFAULT '[]',
			monthly_cost_cents INTEGER NOT NULL DEFAULT 0,
			currency TEXT NOT NULL DEFAULT 'USD',
			renew_cycle TEXT NOT NULL DEFAULT '',
			renew_at TEXT,
			note TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS ops_alerts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			node_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
			kind TEXT NOT NULL,
			severity TEXT NOT NULL,
			status TEXT NOT NULL CHECK(status IN ('active','resolved')),
			message TEXT NOT NULL DEFAULT '',
			dedupe_key TEXT NOT NULL,
			first_seen_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL,
			resolved_at TEXT
		);`,
		`CREATE INDEX IF NOT EXISTS idx_proxy_links_user_id ON proxy_links(user_id);`,
		`CREATE INDEX IF NOT EXISTS idx_admin_sessions_expires_at ON admin_sessions(expires_at);`,
		`CREATE INDEX IF NOT EXISTS idx_traffic_events_user_id ON traffic_events(user_id);`,
		`CREATE INDEX IF NOT EXISTS idx_traffic_events_event_at ON traffic_events(event_at);`,
		`CREATE INDEX IF NOT EXISTS idx_traffic_events_source_at ON traffic_events(source, event_at);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_traffic_events_source_event ON traffic_events(source, source_event_id);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_event_keys_created_at ON usage_event_keys(created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_event_keys_user_id ON usage_event_keys(user_id);`,
		`CREATE INDEX IF NOT EXISTS idx_traffic_rollups_hourly_bucket ON traffic_rollups_hourly(bucket_start);`,
		`CREATE INDEX IF NOT EXISTS idx_traffic_rollups_hourly_user_bucket ON traffic_rollups_hourly(user_id, bucket_start);`,
		`CREATE INDEX IF NOT EXISTS idx_traffic_rollups_hourly_node_bucket ON traffic_rollups_hourly(node_id, bucket_start);`,
		`CREATE INDEX IF NOT EXISTS idx_quota_windows_user_start ON quota_windows(user_id, window_start DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_enforcement_logs_user_time ON enforcement_logs(user_id, created_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_online_sessions_last_seen ON online_sessions(last_seen_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_online_sessions_user_last_seen ON online_sessions(user_id, last_seen_at);`,
		`CREATE INDEX IF NOT EXISTS idx_backups_created_at ON backups(created_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_node_jobs_pending ON node_jobs(status, created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_node_jobs_node_kind_status ON node_jobs(node_id, kind, status, created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_node_jobs_node_status ON node_jobs(node_id, status, created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_node_monthly_usage_node_reported ON node_monthly_usage(node_id, last_reported_at DESC, month_key DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_node_metric_samples_node_time ON node_metric_samples(node_id, sampled_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_node_metric_samples_time ON node_metric_samples(sampled_at);`,
		`CREATE INDEX IF NOT EXISTS idx_node_metric_details_node_time ON node_metric_details(node_id, sampled_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_node_metric_details_time ON node_metric_details(sampled_at);`,
		`CREATE INDEX IF NOT EXISTS idx_node_static_facts_node_reported ON node_static_facts(node_id, reported_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_node_probe_results_node_time ON node_probe_results(node_id, checked_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_node_probe_results_time ON node_probe_results(checked_at);`,
		`CREATE INDEX IF NOT EXISTS idx_ops_alerts_status_seen ON ops_alerts(status, last_seen_at DESC);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_ops_alerts_active_dedupe ON ops_alerts(dedupe_key) WHERE status = 'active';`,
		`CREATE INDEX IF NOT EXISTS idx_node_enroll_codes_expires ON node_enroll_codes(expires_at, used_at);`,
		`CREATE INDEX IF NOT EXISTS idx_node_cert_pins_node_purpose ON node_cert_pins(node_id, purpose, revoked_at);`,
		`CREATE INDEX IF NOT EXISTS idx_audit_logs_time ON audit_logs(created_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_alerts_sent_at ON alerts(sent_at, created_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_telegram_bindings_chat ON telegram_bindings(telegram_chat_id);`,
		`CREATE INDEX IF NOT EXISTS idx_telegram_bindings_user ON telegram_bindings(user_id);`,
		`CREATE INDEX IF NOT EXISTS idx_telegram_bind_attempts_last ON telegram_bind_attempts(last_attempt_at);`,
		`CREATE INDEX IF NOT EXISTS idx_user_node_access_node ON user_node_access(node_id);`,
	}

	for _, stmt := range stmts {
		if _, err := conn.Exec(stmt); err != nil {
			return err
		}
	}

	if _, err := conn.Exec(`DROP TABLE IF EXISTS block_checks;`); err != nil {
		return err
	}

	compatCols := []struct {
		table string
		col   string
		def   string
	}{
		{"users", "quota_cycle", "TEXT NOT NULL DEFAULT 'month'"},
		{"users", "quota_tz", "TEXT NOT NULL DEFAULT 'Asia/Shanghai'"},
		{"users", "device_limit", "INTEGER NOT NULL DEFAULT 2"},
		{"traffic_events", "source", "TEXT NOT NULL DEFAULT 'manual'"},
		{"traffic_events", "source_event_id", "TEXT"},
		{"traffic_events", "target_host", "TEXT"},
		{"traffic_events", "target_ip", "TEXT"},
		{"traffic_events", "target_port", "INTEGER"},
		{"traffic_events", "sni", "TEXT"},
		{"traffic_events", "destination", "TEXT"},
		{"traffic_events", "client_ip", "TEXT"},
		{"traffic_events", "inbound_tag", "TEXT"},
		{"traffic_events", "node_id", "INTEGER REFERENCES nodes(id) ON DELETE SET NULL"},
		{"traffic_stats", "last_target_host", "TEXT"},
		{"traffic_stats", "last_target_ip", "TEXT"},
		{"traffic_stats", "last_target_port", "INTEGER"},
		{"quota_windows", "cycle_type", "TEXT NOT NULL DEFAULT 'month'"},
		{"quota_windows", "cycle_key", "TEXT NOT NULL DEFAULT ''"},
		{"quota_windows", "reset_reason", "TEXT"},
		{"quota_windows", "credit_bytes", "INTEGER NOT NULL DEFAULT 0"},
		{"quota_windows", "alert_80_sent_at", "TEXT"},
		{"quota_windows", "alert_90_sent_at", "TEXT"},
		{"nodes", "managed", "INTEGER NOT NULL DEFAULT 0"},
		{"nodes", "agent_url", "TEXT"},
		{"nodes", "last_seen_at", "TEXT"},
		{"nodes", "last_error", "TEXT"},
		// observed_ip is best-effort (derived from mTLS connection remote addr / X-Forwarded-For).
		{"nodes", "observed_ip", "TEXT"},
		{"nodes", "desired_users_version", "TEXT NOT NULL DEFAULT ''"},
		{"nodes", "applied_users_version", "TEXT NOT NULL DEFAULT ''"},
		{"nodes", "desired_xray_version", "TEXT NOT NULL DEFAULT ''"},
		{"nodes", "applied_xray_version", "TEXT NOT NULL DEFAULT ''"},
		{"nodes", "last_job_error_kind", "TEXT"},
		{"nodes", "last_job_error_at", "TEXT"},
		// users_sync_baseline_schema=0 forbids delta job generation for the
		// node until a schema-1 full users-sync success has been accepted.
		{"nodes", "users_sync_baseline_schema", "INTEGER NOT NULL DEFAULT 0"},
		{"nodes", "users_sync_delta_ready_at", "TEXT"},
		{"nodes", "users_sync_baseline_backfill_at", "TEXT"},
		{"node_jobs", "desired_version", "TEXT NOT NULL DEFAULT ''"},
		{"node_jobs", "retryable", "INTEGER NOT NULL DEFAULT 1"},
		{"node_jobs", "http_status", "INTEGER"},
		{"node_jobs", "timeout_sec", "INTEGER"},
		{"node_jobs", "correlation_id", "TEXT"},
		{"node_runtime_metrics", "cpu_percent", "REAL NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "load1", "REAL NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "load5", "REAL NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "load15", "REAL NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "memory_bytes", "INTEGER NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "memory_total_bytes", "INTEGER NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "memory_available_bytes", "INTEGER NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "swap_used_bytes", "INTEGER NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "swap_total_bytes", "INTEGER NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "inbound_bps", "REAL NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "outbound_bps", "REAL NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "disk_total_bytes", "INTEGER NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "disk_used_bytes", "INTEGER NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "disk_free_bytes", "INTEGER NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "disk_used_percent", "REAL NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "disk_read_bps", "REAL NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "disk_write_bps", "REAL NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "tcp_connections", "INTEGER NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "udp_connections", "INTEGER NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "process_count", "INTEGER NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "system_uptime_sec", "INTEGER NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "agent_uptime_sec", "INTEGER NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "boot_time", "TEXT"},
		{"node_runtime_metrics", "queue_batches", "INTEGER NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "quarantined_batches", "INTEGER NOT NULL DEFAULT 0"},
		{"node_runtime_metrics", "agent_version", "TEXT NOT NULL DEFAULT ''"},
		{"node_runtime_metrics", "xray_version", "TEXT NOT NULL DEFAULT ''"},
		{"node_runtime_metrics", "xray_config_version", "TEXT NOT NULL DEFAULT ''"},
		{"node_monthly_usage", "baseline_rx_total", "INTEGER NOT NULL DEFAULT 0"},
		{"node_monthly_usage", "baseline_tx_total", "INTEGER NOT NULL DEFAULT 0"},
		{"node_monthly_usage", "last_rx_total", "INTEGER NOT NULL DEFAULT 0"},
		{"node_monthly_usage", "last_tx_total", "INTEGER NOT NULL DEFAULT 0"},
		{"node_monthly_usage", "counter_source", "TEXT NOT NULL DEFAULT ''"},
		{"api_keys", "node_id", "INTEGER REFERENCES nodes(id) ON DELETE SET NULL"},
		{"telegram_bindings", "bind_code_expires_at", "TEXT"},
	}
	for _, c := range compatCols {
		if err := addColumnIfMissing(conn, c.table, c.col, c.def); err != nil {
			return err
		}
	}

	if _, err := conn.Exec(`
INSERT INTO usage_event_keys(source, source_event_id, user_id, node_id, event_at, created_at)
SELECT source, source_event_id, user_id, node_id, event_at, event_at
FROM traffic_events
WHERE source_event_id IS NOT NULL AND TRIM(source_event_id) != ''
ON CONFLICT(source, source_event_id) DO NOTHING;
`); err != nil {
		return err
	}

	if err := ensureOpsAlertsActiveDedupe(conn); err != nil {
		return err
	}

	if err := ensureNodeJobsProbeKinds(conn); err != nil {
		return err
	}
	if err := ensureNodeProbeResultsJobReference(conn); err != nil {
		return err
	}

	postCompatIndexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_traffic_events_target_host ON traffic_events(target_host);`,
		`CREATE INDEX IF NOT EXISTS idx_traffic_events_source_at ON traffic_events(source, event_at);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_traffic_events_source_event ON traffic_events(source, source_event_id);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_event_keys_created_at ON usage_event_keys(created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_event_keys_user_id ON usage_event_keys(user_id);`,
		`CREATE INDEX IF NOT EXISTS idx_traffic_events_node_at ON traffic_events(node_id, event_at);`,
		`CREATE INDEX IF NOT EXISTS idx_traffic_events_user_at ON traffic_events(user_id, event_at);`,
		`CREATE INDEX IF NOT EXISTS idx_online_sessions_node ON online_sessions(node_id, last_seen_at);`,
		`CREATE INDEX IF NOT EXISTS idx_node_metric_samples_node_time ON node_metric_samples(node_id, sampled_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_node_metric_samples_time ON node_metric_samples(sampled_at);`,
		`CREATE INDEX IF NOT EXISTS idx_node_metric_details_node_time ON node_metric_details(node_id, sampled_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_node_metric_details_time ON node_metric_details(sampled_at);`,
		`CREATE INDEX IF NOT EXISTS idx_node_static_facts_node_reported ON node_static_facts(node_id, reported_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_node_probe_results_node_time ON node_probe_results(node_id, checked_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_node_probe_results_time ON node_probe_results(checked_at);`,
		`CREATE INDEX IF NOT EXISTS idx_ops_alerts_status_seen ON ops_alerts(status, last_seen_at DESC);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uniq_ops_alerts_active_dedupe ON ops_alerts(dedupe_key) WHERE status = 'active';`,
		`CREATE INDEX IF NOT EXISTS idx_node_user_sync_snapshots_node_created ON node_user_sync_snapshots(node_id, created_at);`,
	}
	for _, stmt := range postCompatIndexes {
		if _, err := conn.Exec(stmt); err != nil {
			return err
		}
	}

	return nil
}

func ensureNodeJobsProbeKinds(conn *sql.DB) error {
	var ddl string
	if err := conn.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'node_jobs';`).Scan(&ddl); err != nil {
		return err
	}
	if strings.Contains(ddl, "probe_dns") && strings.Contains(ddl, "probe_ping") && strings.Contains(ddl, "probe_tcp") && strings.Contains(ddl, "probe_http") {
		return nil
	}
	if _, err := conn.Exec(`PRAGMA foreign_keys = OFF;`); err != nil {
		return err
	}
	defer conn.Exec(`PRAGMA foreign_keys = ON;`)
	if _, err := conn.Exec(`PRAGMA legacy_alter_table = ON;`); err != nil {
		return err
	}
	defer conn.Exec(`PRAGMA legacy_alter_table = OFF;`)

	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`ALTER TABLE node_jobs RENAME TO node_jobs_old_probe_migration;`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE node_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
		kind TEXT NOT NULL CHECK(kind IN ('users_sync','xray_apply','xray_rollback','probe_dns','probe_ping','probe_tcp','probe_http')),
		desired_version TEXT NOT NULL DEFAULT '',
		payload_json TEXT NOT NULL DEFAULT '{}',
		status TEXT NOT NULL CHECK(status IN ('pending','running','succeeded','failed','canceled')),
		attempts INTEGER NOT NULL DEFAULT 0,
		retryable INTEGER NOT NULL DEFAULT 1,
		http_status INTEGER,
		timeout_sec INTEGER,
		correlation_id TEXT,
		last_error TEXT,
		result_json TEXT,
		created_at TEXT NOT NULL,
		started_at TEXT,
		finished_at TEXT
	);`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
INSERT INTO node_jobs(id, node_id, kind, desired_version, payload_json, status, attempts, retryable, http_status, timeout_sec, correlation_id, last_error, result_json, created_at, started_at, finished_at)
SELECT id, node_id, kind, desired_version, payload_json, status, attempts, retryable, http_status, timeout_sec, correlation_id, last_error, result_json, created_at, started_at, finished_at
FROM node_jobs_old_probe_migration;
`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE node_jobs_old_probe_migration;`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_node_jobs_pending ON node_jobs(status, created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_node_jobs_node_kind_status ON node_jobs(node_id, kind, status, created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_node_jobs_node_status ON node_jobs(node_id, status, created_at);`,
	}
	for _, stmt := range indexes {
		if _, err := conn.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func ensureNodeProbeResultsJobReference(conn *sql.DB) error {
	var ddl string
	if err := conn.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'node_probe_results';`).Scan(&ddl); err != nil {
		return err
	}
	if !strings.Contains(ddl, "node_jobs_old_probe_migration") {
		return nil
	}
	if _, err := conn.Exec(`PRAGMA foreign_keys = OFF;`); err != nil {
		return err
	}
	defer conn.Exec(`PRAGMA foreign_keys = ON;`)

	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`ALTER TABLE node_probe_results RENAME TO node_probe_results_old_job_ref_migration;`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE node_probe_results (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
		kind TEXT NOT NULL,
		target TEXT NOT NULL,
		success INTEGER NOT NULL DEFAULT 0,
		latency_ms REAL NOT NULL DEFAULT 0,
		status_code INTEGER,
		error TEXT NOT NULL DEFAULT '',
		checked_at TEXT NOT NULL,
		source_job_id INTEGER REFERENCES node_jobs(id) ON DELETE SET NULL
	);`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
INSERT INTO node_probe_results(id, node_id, kind, target, success, latency_ms, status_code, error, checked_at, source_job_id)
SELECT id, node_id, kind, target, success, latency_ms, status_code, error, checked_at, source_job_id
FROM node_probe_results_old_job_ref_migration;
`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE node_probe_results_old_job_ref_migration;`); err != nil {
		return err
	}
	return tx.Commit()
}

func ensureOpsAlertsActiveDedupe(conn *sql.DB) error {
	var ddl string
	if err := conn.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'ops_alerts';`).Scan(&ddl); err != nil {
		return err
	}
	if !strings.Contains(ddl, "UNIQUE(dedupe_key, status)") {
		return nil
	}
	if _, err := conn.Exec(`PRAGMA foreign_keys = OFF;`); err != nil {
		return err
	}
	defer conn.Exec(`PRAGMA foreign_keys = ON;`)

	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`ALTER TABLE ops_alerts RENAME TO ops_alerts_old_active_dedupe_migration;`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE ops_alerts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		node_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
		kind TEXT NOT NULL,
		severity TEXT NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('active','resolved')),
		message TEXT NOT NULL DEFAULT '',
		dedupe_key TEXT NOT NULL,
		first_seen_at TEXT NOT NULL,
		last_seen_at TEXT NOT NULL,
		resolved_at TEXT
	);`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
INSERT INTO ops_alerts(id, node_id, kind, severity, status, message, dedupe_key, first_seen_at, last_seen_at, resolved_at)
SELECT id, node_id, kind, severity, status, message, dedupe_key, first_seen_at, last_seen_at, resolved_at
FROM ops_alerts_old_active_dedupe_migration;
`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE ops_alerts_old_active_dedupe_migration;`); err != nil {
		return err
	}
	return tx.Commit()
}

func addColumnIfMissing(conn *sql.DB, table, col, def string) error {
	if !isValidSQLIdent(table) {
		return fmt.Errorf("invalid table identifier: %q", table)
	}
	if !isValidSQLIdent(col) {
		return fmt.Errorf("invalid column identifier: %q", col)
	}
	q := fmt.Sprintf(`PRAGMA table_info(%s);`, table)
	rows, err := conn.Query(q)
	if err != nil {
		return err
	}
	defer rows.Close()

	var (
		cid       int
		name      string
		ctype     string
		notnull   int
		dfltValue sql.NullString
		pk        int
	)
	for rows.Next() {
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return err
		}
		if name == col {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	alter := fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s;`, table, col, def)
	_, err = conn.Exec(alter)
	return err
}
