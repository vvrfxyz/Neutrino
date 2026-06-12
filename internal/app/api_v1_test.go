package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"neutrino/internal/config"
	"neutrino/internal/db"
	"neutrino/internal/repo"
	"neutrino/internal/service"
)

func TestHandleAPIPlanExtendQueuesUsersSync(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "app-test.db")
	conn, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	store := repo.New(conn, config.Config{})
	node, err := store.CreateNode(ctx, repo.CreateNodeInput{
		Name:     "extend-sync-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	user, err := store.CreateUser(ctx, repo.CreateUserInput{
		Username:       "extend-sync-user",
		MonthlyLimitGB: 10,
		CountingMode:   "double",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := store.RawDB().ExecContext(ctx, `
UPDATE users
SET expires_at = ?
WHERE id = ?;
`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), user.ID); err != nil {
		t.Fatalf("expire user: %v", err)
	}
	if _, err := store.SweepExpiredUsers(ctx); err != nil {
		t.Fatalf("sweep expired: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"days": 3})
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/users/%d/plan/extend", user.ID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	a := &App{store: store}
	a.handleAPIUserPlanExtendV1(rr, req, user.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("plan extend got %d body=%s", rr.Code, rr.Body.String())
	}

	jobs, err := store.ListNodeJobs(ctx, node.ID, 10)
	if err != nil {
		t.Fatalf("list node jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Kind != "users_sync" || jobs[0].Status != "pending" {
		t.Fatalf("expected pending users_sync after API plan extend, got %+v", jobs)
	}
}

func TestHandleAPIQuotaResetRejectsInvalidJSONWithoutMutation(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "app-test.db")
	conn, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	store := repo.New(conn, config.Config{})
	user, err := store.CreateUser(ctx, repo.CreateUserInput{
		Username:       "quota-reset-json-user",
		MonthlyLimitGB: 10,
		CountingMode:   "double",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := store.RecordUsage(ctx, repo.UsageInput{
		UserID:        user.ID,
		Direction:     "outbound",
		Bytes:         1024,
		At:            time.Now().UTC(),
		Source:        "test",
		SourceEventID: "quota-reset-json-usage",
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	req := httptest.NewRequest(
		http.MethodPost,
		fmt.Sprintf("/api/v1/users/%d/quota/reset", user.ID),
		bytes.NewBufferString(`{"reason":"bad","unexpected":true}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	a := &App{store: store}
	a.handleAPIUserQuotaResetV1(rr, req, user.ID)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("quota reset invalid json got %d body=%s", rr.Code, rr.Body.String())
	}

	got, err := store.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.WindowEffective != 1024 {
		t.Fatalf("invalid reset request mutated quota window: effective=%d", got.WindowEffective)
	}
}

func TestHandleAPIQuotaResetAllowsEmptyBody(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "app-test.db")
	conn, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	store := repo.New(conn, config.Config{})
	user, err := store.CreateUser(ctx, repo.CreateUserInput{
		Username:       "quota-reset-empty-body-user",
		MonthlyLimitGB: 10,
		CountingMode:   "double",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := store.RecordUsage(ctx, repo.UsageInput{
		UserID:        user.ID,
		Direction:     "outbound",
		Bytes:         1024,
		At:            time.Now().UTC(),
		Source:        "test",
		SourceEventID: "quota-reset-empty-body-usage",
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/users/%d/quota/reset", user.ID), nil)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	a := &App{store: store}
	a.handleAPIUserQuotaResetV1(rr, req, user.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("quota reset empty body got %d body=%s", rr.Code, rr.Body.String())
	}

	got, err := store.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.WindowEffective != 0 {
		t.Fatalf("empty-body reset should clear current quota window, got effective=%d", got.WindowEffective)
	}
}

func TestHandleAPIUserPatchValidatesBeforeStatusMutation(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "app-test.db")
	conn, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	store := repo.New(conn, config.Config{})
	user, err := store.CreateUser(ctx, repo.CreateUserInput{
		Username:       "patch-atomic-user",
		MonthlyLimitGB: 10,
		CountingMode:   "double",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"status": "disabled", "device_limit": 0})
	req := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/v1/users/%d", user.ID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	a := &App{store: store}
	a.handleAPIUserByIDV1(rr, req, user.ID)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("patch invalid device_limit got %d body=%s", rr.Code, rr.Body.String())
	}

	got, err := store.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("invalid patch changed status to %s", got.Status)
	}
	if got.DeviceLimit != user.DeviceLimit {
		t.Fatalf("invalid patch changed device limit to %d", got.DeviceLimit)
	}
	if got.ActiveLink == nil {
		t.Fatalf("invalid patch deactivated active link")
	}
}

func TestListUsersWithOnlineStatsRefreshesExpiredUsers(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "app-test.db")
	conn, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()
	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	store := repo.New(conn, config.Config{})
	user, err := store.CreateUser(ctx, repo.CreateUserInput{
		Username:       "expired-page-user",
		MonthlyLimitGB: 10,
		CountingMode:   "double",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := store.RawDB().ExecContext(ctx, `
UPDATE users
SET expires_at = ?
WHERE id = ?;
`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), user.ID); err != nil {
		t.Fatalf("expire user: %v", err)
	}

	a := &App{cfg: config.Config{OnlineDisplayWindowSec: 120}, store: store}
	a.userService = service.NewUserService(store, a)
	users, err := a.listUsersWithOnlineStats(ctx)
	if err != nil {
		t.Fatalf("list users with online stats: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(users))
	}
	if users[0].Status != "expired" {
		t.Fatalf("expected lifecycle refresh to mark expired, got %s", users[0].Status)
	}
	if users[0].ActiveLink != nil {
		t.Fatalf("expected expired user link to be inactive")
	}
}
