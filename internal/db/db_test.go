package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenEnablesForeignKeysPerConnection(t *testing.T) {
	t.Parallel()

	conn, err := Open(filepath.Join(t.TempDir(), "foreign-keys.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	conn.SetMaxOpenConns(2)
	ctx := context.Background()
	sqlConns := make([]*sql.Conn, 0, 2)
	for i := 0; i < 2; i++ {
		c, err := conn.Conn(ctx)
		if err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
		defer c.Close()
		sqlConns = append(sqlConns, c)
	}

	for i, raw := range sqlConns {
		c := raw
		var enabled int
		if err := c.QueryRowContext(ctx, `PRAGMA foreign_keys;`).Scan(&enabled); err != nil {
			t.Fatalf("pragma foreign_keys conn %d: %v", i, err)
		}
		if enabled != 1 {
			t.Fatalf("expected foreign_keys=1 on conn %d, got %d", i, enabled)
		}
	}
}

func TestAddColumnIfMissing_RejectsInvalidIdents(t *testing.T) {
	t.Parallel()

	conn, err := Open(filepath.Join(t.TempDir(), "ident-check.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	if err := Migrate(conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Row count before the abuse attempt.
	ctx := context.Background()
	var before int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='users';`).Scan(&before); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if before != 1 {
		t.Fatalf("expected users table to exist, got count=%d", before)
	}

	cases := []struct {
		table string
		col   string
	}{
		{"users; DROP TABLE users", "extra"},
		{"users", "bad name"},
		{"", "col"},
		{"users", ""},
	}
	for _, c := range cases {
		if err := addColumnIfMissing(conn, c.table, c.col, "TEXT"); err == nil {
			t.Fatalf("expected rejection for table=%q col=%q, got nil", c.table, c.col)
		}
	}

	// Table must still be present.
	var after int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='users';`).Scan(&after); err != nil {
		t.Fatalf("count tables after: %v", err)
	}
	if after != 1 {
		t.Fatalf("users table gone after invalid-ident attempts (before=%d after=%d)", before, after)
	}
}

func TestMigrateOldTrafficEventsWithoutSourceColumns(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "old-traffic-events.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	// Simulate a pre-source-tracking database: traffic_events exists but has
	// no source / source_event_id columns (they are compat-added by Migrate).
	if _, err := conn.Exec(`
PRAGMA foreign_keys = ON;
CREATE TABLE users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	username TEXT NOT NULL UNIQUE,
	monthly_limit_bytes INTEGER NOT NULL,
	counting_mode TEXT NOT NULL CHECK(counting_mode IN ('single','double')),
	status TEXT NOT NULL DEFAULT 'active',
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);
CREATE TABLE traffic_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	direction TEXT NOT NULL CHECK(direction IN ('inbound','outbound')),
	bytes INTEGER NOT NULL CHECK(bytes >= 0),
	event_at TEXT NOT NULL
);
INSERT INTO users(username, monthly_limit_bytes, counting_mode, created_at, expires_at)
VALUES ('old-user', 1000, 'double', '2026-01-01T00:00:00Z', '2026-12-31T00:00:00Z');
INSERT INTO traffic_events(user_id, direction, bytes, event_at)
VALUES (1, 'inbound', 100, '2026-01-01T00:00:00Z');
`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if err := Migrate(conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, idx := range []string{"idx_traffic_events_source_at", "uniq_traffic_events_source_event"} {
		var count int
		if err := conn.QueryRow(`SELECT COUNT(1) FROM sqlite_master WHERE type = 'index' AND name = ?;`, idx).Scan(&count); err != nil {
			t.Fatalf("query index %s: %v", idx, err)
		}
		if count != 1 {
			t.Fatalf("index %s missing after migrate", idx)
		}
	}
	var source string
	if err := conn.QueryRow(`SELECT source FROM traffic_events WHERE id = 1;`).Scan(&source); err != nil {
		t.Fatalf("read compat source column: %v", err)
	}
	if source != "manual" {
		t.Fatalf("expected compat default source 'manual', got %q", source)
	}
}

func TestMigrateFreshDBCreatesSourceIndexes(t *testing.T) {
	t.Parallel()

	conn, err := Open(filepath.Join(t.TempDir(), "fresh-indexes.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	if err := Migrate(conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, idx := range []string{"idx_traffic_events_source_at", "uniq_traffic_events_source_event"} {
		var count int
		if err := conn.QueryRow(`SELECT COUNT(1) FROM sqlite_master WHERE type = 'index' AND name = ?;`, idx).Scan(&count); err != nil {
			t.Fatalf("query index %s: %v", idx, err)
		}
		if count != 1 {
			t.Fatalf("index %s missing on fresh db", idx)
		}
	}
}

func TestMigrateUpgradesNodeJobsProbeKinds(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "old-node-jobs.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Exec(`
PRAGMA foreign_keys = ON;
CREATE TABLE nodes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	core_type TEXT NOT NULL CHECK(core_type IN ('xray','singbox')),
	protocol TEXT NOT NULL CHECK(protocol IN ('vless_reality','hysteria2','tuic')),
	host TEXT NOT NULL,
	port INTEGER NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE node_jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK(kind IN ('users_sync','xray_apply','xray_rollback')),
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
);
CREATE TABLE node_probe_results (
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
);
INSERT INTO nodes(name, core_type, protocol, host, port, created_at, updated_at)
VALUES ('old-node', 'xray', 'vless_reality', 'example.com', 443, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
INSERT INTO node_jobs(node_id, kind, status, created_at)
VALUES (1, 'users_sync', 'pending', '2026-01-01T00:00:00Z');
INSERT INTO node_probe_results(node_id, kind, target, success, checked_at, source_job_id)
VALUES (1, 'probe_tcp', 'example.com:443', 1, '2026-01-01T00:00:00Z', 1);
`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if err := Migrate(conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := conn.Exec(`INSERT INTO node_jobs(node_id, kind, status, created_at) VALUES (1, 'probe_tcp', 'pending', '2026-01-01T00:00:01Z');`); err != nil {
		t.Fatalf("probe kind insert after migrate: %v", err)
	}
	if _, err := conn.Exec(`INSERT INTO node_jobs(node_id, kind, status, created_at) VALUES (1, 'probe_dns', 'pending', '2026-01-01T00:00:02Z');`); err != nil {
		t.Fatalf("probe_dns kind insert after migrate: %v", err)
	}
	var count int
	if err := conn.QueryRow(`SELECT COUNT(1) FROM node_jobs WHERE kind = 'users_sync';`).Scan(&count); err != nil {
		t.Fatalf("count old job: %v", err)
	}
	if count != 1 {
		t.Fatalf("old jobs not preserved, count=%d", count)
	}
	var probeDDL string
	if err := conn.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'node_probe_results';`).Scan(&probeDDL); err != nil {
		t.Fatalf("query probe ddl: %v", err)
	}
	if strings.Contains(probeDDL, "node_jobs_old_probe_migration") || !strings.Contains(probeDDL, "REFERENCES node_jobs") {
		t.Fatalf("node_probe_results has broken job reference: %s", probeDDL)
	}
	if _, err := conn.Exec(`DELETE FROM node_probe_results WHERE id = 1;`); err != nil {
		t.Fatalf("delete probe result after migrate: %v", err)
	}
}
