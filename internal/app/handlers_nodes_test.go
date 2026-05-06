package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"neutrino/internal/config"
	"neutrino/internal/db"
	"neutrino/internal/repo"
	"neutrino/internal/service"
)

func TestHandleAPINodeAgentUsersReturns500WhenNodeLookupFails(t *testing.T) {
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
	node, err := store.CreateNode(context.Background(), repo.CreateNodeInput{
		Name:     "agent-users-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/nodes/%d/agent/users", node.ID), nil)
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{
			Subject: pkix.Name{CommonName: fmt.Sprintf("node-%d", node.ID)},
		}},
	}
	rr := httptest.NewRecorder()

	a := &App{store: store}
	a.userService = service.NewUserService(store, a)
	a.handleAPINodeAgentUsers(rr, req, node.ID)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on node lookup failure, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleAPINodeEnrollRejectsOversizedBodyBeforeStore(t *testing.T) {
	body := `{"enroll_code":"x","csr_pem":"` + strings.Repeat("A", 70*1024) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/1/enroll", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	a := &App{}
	a.handleAPINodeEnroll(rr, req, 1)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized enroll body, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleAPINodeAgentUsersRefreshesExpiredUsers(t *testing.T) {
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
		Name:     "agent-users-expired-node",
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
		Username:       "agent-expired-user",
		MonthlyLimitGB: 10,
		CountingMode:   "double",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := store.RawDB().ExecContext(ctx, `
	UPDATE users
	SET expires_at = ?, status = 'active', removed_at = NULL
	WHERE id = ?;
	`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), user.ID); err != nil {
		t.Fatalf("expire user: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/nodes/%d/agent/users", node.ID), nil)
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{
			Subject: pkix.Name{CommonName: fmt.Sprintf("node-%d", node.ID)},
		}},
	}
	rr := httptest.NewRecorder()

	a := &App{store: store}
	a.userService = service.NewUserService(store, a)
	a.handleAPINodeAgentUsers(rr, req, node.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var payload struct {
		Users []map[string]any `json:"users"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(payload.Users) != 1 {
		t.Fatalf("expected 1 user, got %+v", payload.Users)
	}
	if got := payload.Users[0]["status"]; got != "expired" {
		t.Fatalf("expected expired user in sync payload, got %v payload=%+v", got, payload.Users[0])
	}
	if _, ok := payload.Users[0]["uuid"]; ok {
		t.Fatalf("expired user should not include active uuid: %+v", payload.Users[0])
	}

	after, err := store.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if after.Status != "expired" || after.ActiveLink != nil {
		t.Fatalf("expected expired user with inactive link, got %+v", after)
	}
}

func TestHandleAPINodeJobFinishRejectsWrongAttemptWithoutApplyingVersion(t *testing.T) {
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
	node, err := store.CreateNode(context.Background(), repo.CreateNodeInput{
		Name:     "finish-attempt-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
		Managed:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	jobID, _, err := store.EnqueueNodeJob(context.Background(), node.ID, "xray_apply", "ver-1", `{}`, 120, "test")
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	claimed, ok, err := store.ClaimNextNodeJobForNode(context.Background(), node.ID)
	if err != nil || !ok {
		t.Fatalf("claim job ok=%v err=%v", ok, err)
	}

	body, err := json.Marshal(map[string]any{
		"status":          "succeeded",
		"retryable":       false,
		"applied_version": "ver-1",
		"attempt":         claimed.Attempts + 1,
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/jobs/%d/finish", node.ID, jobID), bytes.NewReader(body))
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{
			Subject: pkix.Name{CommonName: fmt.Sprintf("node-%d", node.ID)},
		}},
	}
	rr := httptest.NewRecorder()

	a := &App{store: store, cfg: config.Config{NodeJobMaxAttempts: 5}}
	a.handleAPINodeJobFinish(rr, req, node.ID, jobID)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for wrong attempt, got %d body=%s", rr.Code, rr.Body.String())
	}
	after, err := store.GetNode(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("get node after finish: %v", err)
	}
	if after.AppliedXrayVersion != "" {
		t.Fatalf("wrong attempt should not update applied xray version, got %q", after.AppliedXrayVersion)
	}
}

func TestHandleAPINodeDeleteDisablesAndWaitsForEmptyUsersSync(t *testing.T) {
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
	node, err := store.CreateNode(context.Background(), repo.CreateNodeInput{
		Name:     "delete-sync-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	a := &App{store: store}

	req := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/nodes/%d", node.ID), nil)
	rr := httptest.NewRecorder()
	a.handleAPINodeByIDV1(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("first delete got %d body=%s, want 202", rr.Code, rr.Body.String())
	}
	got, err := store.GetNode(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("node should remain for cleanup sync: %v", err)
	}
	if got.Enabled {
		t.Fatalf("node should be disabled before hard delete")
	}
	jobs, err := store.ListNodeJobs(context.Background(), node.ID, 10)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Kind != "users_sync" {
		t.Fatalf("expected pending users_sync cleanup job, got %+v", jobs)
	}

	req = httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/nodes/%d", node.ID), nil)
	rr = httptest.NewRecorder()
	a.handleAPINodeByIDV1(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("second delete before applied got %d body=%s, want 202", rr.Code, rr.Body.String())
	}
	if _, err := store.GetNode(context.Background(), node.ID); err != nil {
		t.Fatalf("node should still exist until empty users sync is applied: %v", err)
	}

	emptyVersion := usersDesiredVersion([]repo.User{})
	if err := store.SetNodeAppliedUsersVersion(context.Background(), node.ID, emptyVersion); err != nil {
		t.Fatalf("mark empty users sync applied: %v", err)
	}
	req = httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/nodes/%d", node.ID), nil)
	rr = httptest.NewRecorder()
	a.handleAPINodeByIDV1(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete after applied got %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	if _, err := store.GetNode(context.Background(), node.ID); err == nil {
		t.Fatalf("node should be hard-deleted after empty users sync is applied")
	}
}

func TestHandleAPINodeDeleteMissingReturnsNotFound(t *testing.T) {
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
	a := &App{store: store}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/nodes/404", nil)
	rr := httptest.NewRecorder()

	a.handleAPINodeByIDV1(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("delete missing node got %d body=%s, want 404", rr.Code, rr.Body.String())
	}
}

func TestHandleAPINodePatchPreservesOmittedFieldsAndQueuesDisableSync(t *testing.T) {
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
	node, err := store.CreateNode(context.Background(), repo.CreateNodeInput{
		Name:     "patch-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "old.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	a := &App{store: store}

	body, _ := json.Marshal(map[string]any{"host": "new.example.com"})
	req := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", node.ID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	a.handleAPINodeByIDV1(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch host got %d body=%s", rr.Code, rr.Body.String())
	}
	got, err := store.GetNode(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if !got.Enabled || got.Host != "new.example.com" || got.Port != 443 {
		t.Fatalf("patch should preserve omitted fields, got %+v", got)
	}

	body, _ = json.Marshal(map[string]any{"enabled": false})
	req = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", node.ID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	a.handleAPINodeByIDV1(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch disable got %d body=%s", rr.Code, rr.Body.String())
	}
	got, err = store.GetNode(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("get disabled node: %v", err)
	}
	if got.Enabled {
		t.Fatalf("node should be disabled after patch")
	}
	jobs, err := store.ListNodeJobs(context.Background(), node.ID, 10)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Kind != "users_sync" {
		t.Fatalf("expected users_sync after patch disable, got %+v", jobs)
	}
}

func TestHandleManagedXrayRollbackRejectsInvalidJSON(t *testing.T) {
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
	node, err := store.CreateNode(context.Background(), repo.CreateNodeInput{
		Name:     "rollback-json-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
		Managed:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	a := &App{store: store}

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/managed/xray/rollback", node.ID), strings.NewReader("{"))
	rr := httptest.NewRecorder()
	a.handleAPINodeByIDV1Extended(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid json rollback got %d body=%s, want 400", rr.Code, rr.Body.String())
	}
	jobs, err := store.ListNodeJobs(context.Background(), node.ID, 10)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("invalid json should not enqueue rollback job: %+v", jobs)
	}
}

func TestHandleManagedXrayDeployMissingNodeReturnsNotFound(t *testing.T) {
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
	a := &App{store: store}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/404/managed/xray/deploy", nil)
	rr := httptest.NewRecorder()

	a.handleAPINodeByIDV1Extended(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("deploy missing node got %d body=%s, want 404", rr.Code, rr.Body.String())
	}
}

func TestHandleManagedXrayDeployInvalidExtraJSONReturnsBadRequest(t *testing.T) {
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
		Name:     "deploy-invalid-extra-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
		Managed:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := store.RawDB().ExecContext(ctx, `UPDATE nodes SET extra_json = ? WHERE id = ?`, "{", node.ID); err != nil {
		t.Fatalf("poison extra_json: %v", err)
	}

	a := &App{store: store}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/managed/xray/deploy", node.ID), nil)
	rr := httptest.NewRecorder()

	a.handleAPINodeByIDV1Extended(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("deploy invalid extra_json got %d body=%s, want 400", rr.Code, rr.Body.String())
	}
}
