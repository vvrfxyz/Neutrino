package db

import (
	"context"
	"database/sql"
	"path/filepath"
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
