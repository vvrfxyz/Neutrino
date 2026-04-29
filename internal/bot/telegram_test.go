package bot

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"neutrino/internal/config"
	"neutrino/internal/db"
	"neutrino/internal/repo"
)

func newTestBot(t *testing.T) (*TelegramBot, *repo.Store) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "telegram-test.db")
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

	store := repo.New(conn, config.Config{SubBaseURL: "https://panel.example.com"})
	return New(config.Config{SubBaseURL: "https://panel.example.com"}, store), store
}

func TestHandleCommandRequiresBindingForSelfServiceCommands(t *testing.T) {
	ctx := context.Background()
	b, store := newTestBot(t)

	if _, err := store.CreateUser(ctx, repo.CreateUserInput{
		Username:       "victim",
		MonthlyLimitGB: 1,
		CountingMode:   "double",
		PlanDays:       30,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	resp := b.handleCommand(ctx, 12345, 67890, "victim", "/sub")
	if !strings.Contains(resp, "user not bound") {
		t.Fatalf("expected unbound warning, got %q", resp)
	}
}

func TestHandleCommandAllowsBoundUserSelfService(t *testing.T) {
	ctx := context.Background()
	b, store := newTestBot(t)

	u, err := store.CreateUser(ctx, repo.CreateUserInput{
		Username:       "bound-user",
		MonthlyLimitGB: 1,
		CountingMode:   "double",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	binding, err := store.EnsureTelegramBinding(ctx, u.ID)
	if err != nil {
		t.Fatalf("ensure binding: %v", err)
	}
	if _, _, err := store.BindTelegramChatByCode(ctx, binding.BindCode, 223344, 556677, "telegram-user"); err != nil {
		t.Fatalf("bind chat: %v", err)
	}

	resp := b.handleCommand(ctx, 223344, 556677, "telegram-user", "/sub")
	if !strings.Contains(resp, "https://panel.example.com/sub/") {
		t.Fatalf("expected subscription url, got %q", resp)
	}
}
