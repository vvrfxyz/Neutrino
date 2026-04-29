package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenEnablesForeignKeysOnNewConnections(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	conn, err := Open(filepath.Join(t.TempDir(), "fk.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(2)

	if err := Migrate(conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	held, err := conn.Conn(ctx)
	if err != nil {
		t.Fatalf("reserve conn: %v", err)
	}
	defer held.Close()
	if _, err := held.ExecContext(ctx, `SELECT 1;`); err != nil {
		t.Fatalf("touch reserved conn: %v", err)
	}

	_, err = conn.ExecContext(ctx, `
INSERT INTO proxy_links(user_id, protocol, uuid, inbound_tag, link, active, created_at)
VALUES (987654321, 'vless-reality-vision', 'uuid', 'tag', 'link', 1, '2026-01-01T00:00:00Z');
`)
	if err == nil {
		t.Fatalf("expected foreign key violation on a non-migration connection")
	}
}
