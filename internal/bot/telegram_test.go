package bot

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"neutrino/internal/config"
	"neutrino/internal/db"
	"neutrino/internal/repo"
	"neutrino/internal/service"
)

// recordingSync counts sync requests so tests can assert that admin commands
// actually propagate to nodes (the original bug: /disable wrote the DB but
// never triggered users-sync, leaving the user connected).
type recordingSync struct {
	usersSync        int
	usersSyncNow     int
	xrayReload       int
	usersSyncForNode int
}

func (r *recordingSync) RequestUsersSync(context.Context)                  { r.usersSync++ }
func (r *recordingSync) RequestUsersSyncNow(context.Context)               { r.usersSyncNow++ }
func (r *recordingSync) RequestUsersSyncForNodeNow(context.Context, int64) { r.usersSyncForNode++ }
func (r *recordingSync) RequestManagedXrayReload(context.Context)          { r.xrayReload++ }

func newTestBot(t *testing.T) (*TelegramBot, *repo.Store, *recordingSync) {
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
	sync := &recordingSync{}
	users := service.NewUserService(store, sync)
	cfg := config.Config{
		SubBaseURL:           "https://panel.example.com",
		TelegramAdminChatIDs: "424242",
	}
	return New(cfg, store, users), store, sync
}

func TestHandleCommandRequiresBindingForSelfServiceCommands(t *testing.T) {
	ctx := context.Background()
	b, store, _ := newTestBot(t)

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
	b, store, _ := newTestBot(t)

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

func TestAdminDisableEnableRoutesThroughUserService(t *testing.T) {
	ctx := context.Background()
	b, store, sync := newTestBot(t)

	u, err := store.CreateUser(ctx, repo.CreateUserInput{
		Username:       "target",
		MonthlyLimitGB: 1,
		CountingMode:   "double",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	resp := b.handleCommand(ctx, 424242, 1, "admin", "/disable target")
	if resp != "ok" {
		t.Fatalf("expected ok, got %q", resp)
	}
	got, err := store.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Status != "disabled" {
		t.Fatalf("expected status disabled, got %q", got.Status)
	}
	// The original bug: /disable wrote the DB directly and never requested a
	// users-sync, so the user stayed connected on every node.
	if sync.usersSyncNow == 0 {
		t.Fatalf("expected /disable to request an immediate users-sync")
	}
	if sync.xrayReload == 0 {
		t.Fatalf("expected /disable to request a managed-xray reload")
	}

	syncBefore := sync.usersSyncNow
	resp = b.handleCommand(ctx, 424242, 1, "admin", "/enable target")
	if resp != "ok" {
		t.Fatalf("expected ok, got %q", resp)
	}
	got, err = store.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("expected status active, got %q", got.Status)
	}
	if sync.usersSyncNow <= syncBefore {
		t.Fatalf("expected /enable to request an immediate users-sync")
	}
}

func TestAdminQuotaResetRoutesThroughUserService(t *testing.T) {
	ctx := context.Background()
	b, store, sync := newTestBot(t)

	if _, err := store.CreateUser(ctx, repo.CreateUserInput{
		Username:       "quota-user",
		MonthlyLimitGB: 1,
		CountingMode:   "double",
		PlanDays:       30,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	resp := b.handleCommand(ctx, 424242, 1, "admin", "/quota_reset quota-user")
	if resp != "ok" {
		t.Fatalf("expected ok, got %q", resp)
	}
	if sync.usersSync == 0 {
		t.Fatalf("expected /quota_reset to request a users-sync")
	}
}

func TestAdminCommandsRejectedFromNonAdminChat(t *testing.T) {
	ctx := context.Background()
	b, store, sync := newTestBot(t)

	u, err := store.CreateUser(ctx, repo.CreateUserInput{
		Username:       "bystander",
		MonthlyLimitGB: 1,
		CountingMode:   "double",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	resp := b.handleCommand(ctx, 999999, 1, "rando", "/disable bystander")
	if resp == "ok" {
		t.Fatalf("non-admin chat must not disable users")
	}
	got, err := store.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("expected user untouched, got status %q", got.Status)
	}
	if sync.usersSyncNow != 0 || sync.xrayReload != 0 {
		t.Fatalf("expected no sync requests from rejected command")
	}
}
