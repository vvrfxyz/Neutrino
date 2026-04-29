package repo

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"neutrino/internal/config"
	"neutrino/internal/db"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "neutrino-test.db")
	conn, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})

	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	return New(conn, config.Config{})
}

func newActiveUser(t *testing.T, s *Store) User {
	t.Helper()

	username := fmt.Sprintf("user-%d", time.Now().UnixNano())
	u, err := s.CreateUser(context.Background(), CreateUserInput{
		Username:       username,
		MonthlyLimitGB: 500,
		CountingMode:   "double",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}
