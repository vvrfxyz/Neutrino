package functional_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestFunctional_APIKeyLifecycleHTTP(t *testing.T) {
	env := setupTestEnv(t)

	// Create via admin Basic Auth.
	createResp := env.request(t, http.MethodPost, "/api/v1/apikeys", map[string]any{
		"name":   "ops-reader",
		"scopes": []string{"nodes:read", "traffic:read"},
	}, true, "")
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createResp.Body)
		t.Fatalf("create status=%d body=%s", createResp.StatusCode, body)
	}
	var created struct {
		Key  string `json:"key"`
		Item struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Scopes string `json:"scopes"`
		} `json:"item"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if !strings.HasPrefix(created.Key, "nk_") || created.Item.ID == 0 {
		t.Fatalf("unexpected create payload: %+v", created)
	}
	if created.Item.Scopes != "nodes:read,traffic:read" {
		t.Fatalf("scopes = %q", created.Item.Scopes)
	}

	// The minted key works for its scope...
	okResp := env.request(t, http.MethodGet, "/api/v1/ops/nodes", nil, false, created.Key)
	defer okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("scoped request status=%d", okResp.StatusCode)
	}
	// ...but must NOT be able to manage keys (no privilege escalation).
	escResp := env.request(t, http.MethodPost, "/api/v1/apikeys", map[string]any{
		"name":   "evil",
		"scopes": []string{"admin"},
	}, false, created.Key)
	defer escResp.Body.Close()
	if escResp.StatusCode != http.StatusForbidden {
		t.Fatalf("narrow key creating keys: status=%d, want 403", escResp.StatusCode)
	}
	listEscResp := env.request(t, http.MethodGet, "/api/v1/apikeys", nil, false, created.Key)
	defer listEscResp.Body.Close()
	if listEscResp.StatusCode != http.StatusForbidden {
		t.Fatalf("narrow key listing keys: status=%d, want 403", listEscResp.StatusCode)
	}

	// List via admin: metadata only, never plaintext or hash.
	listResp := env.request(t, http.MethodGet, "/api/v1/apikeys", nil, true, "")
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d", listResp.StatusCode)
	}
	listBody, _ := io.ReadAll(listResp.Body)
	if strings.Contains(string(listBody), created.Key) {
		t.Fatalf("list leaked plaintext key")
	}
	var list struct {
		Count int `json:"count"`
		Items []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(listBody, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Count != 1 || list.Items[0].ID != created.Item.ID {
		t.Fatalf("list = %+v", list)
	}

	// Get by id.
	getResp := env.request(t, http.MethodGet, "/api/v1/apikeys/"+strconv.FormatInt(created.Item.ID, 10), nil, true, "")
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get status=%d", getResp.StatusCode)
	}

	// Revoke; the key stops authenticating immediately.
	revokeResp := env.request(t, http.MethodDelete, "/api/v1/apikeys/"+strconv.FormatInt(created.Item.ID, 10), nil, true, "")
	defer revokeResp.Body.Close()
	if revokeResp.StatusCode != http.StatusOK {
		t.Fatalf("revoke status=%d", revokeResp.StatusCode)
	}
	deadResp := env.request(t, http.MethodGet, "/api/v1/ops/nodes", nil, false, created.Key)
	defer deadResp.Body.Close()
	if deadResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked key status=%d, want 401", deadResp.StatusCode)
	}

	// Audit entries recorded for create + revoke.
	var count int
	if err := env.store.RawDB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM audit_logs WHERE action IN ('apikey.create','apikey.revoke')`).Scan(&count); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if count != 2 {
		t.Fatalf("audit rows = %d, want 2", count)
	}

	// Invalid scope rejected.
	badResp := env.request(t, http.MethodPost, "/api/v1/apikeys", map[string]any{
		"name":   "typo",
		"scopes": []string{"users:reed"},
	}, true, "")
	defer badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid scope status=%d, want 400", badResp.StatusCode)
	}

	// Unknown id revoke → 404.
	missingResp := env.request(t, http.MethodDelete, "/api/v1/apikeys/999999", nil, true, "")
	defer missingResp.Body.Close()
	if missingResp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing id status=%d, want 404", missingResp.StatusCode)
	}
}

func TestFunctional_APIKeysAdminPage(t *testing.T) {
	env := setupTestEnv(t)

	// Page renders for an admin with the create form and scope checkboxes.
	pageResp := env.request(t, http.MethodGet, "/apikeys", nil, true, "")
	defer pageResp.Body.Close()
	if pageResp.StatusCode != http.StatusOK {
		t.Fatalf("page status=%d", pageResp.StatusCode)
	}
	body := decodeBodyText(t, pageResp)
	for _, want := range []string{`action="/apikeys"`, `name="scopes"`, "users:read", "暂无密钥"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing %q", want)
		}
	}

	// Create via the SSR form; the plaintext key renders exactly once.
	form := url.Values{}
	form.Set("name", "panel-made")
	form.Add("scopes", "nodes:read")
	createResp := env.requestForm(t, http.MethodPost, "/apikeys", form, true)
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusOK {
		t.Fatalf("form create status=%d", createResp.StatusCode)
	}
	createBody := decodeBodyText(t, createResp)
	if !strings.Contains(createBody, "nk_") || !strings.Contains(createBody, "panel-made") {
		t.Fatalf("create page missing key or name")
	}

	// Subsequent page loads must NOT re-show the plaintext.
	again := env.request(t, http.MethodGet, "/apikeys", nil, true, "")
	defer again.Body.Close()
	if b := decodeBodyText(t, again); strings.Contains(b, "nk_") {
		t.Fatalf("plaintext key persisted across page loads")
	}

	// Revoke via the SSR route redirects back to the page.
	var keyID int64
	if err := env.store.RawDB().QueryRowContext(context.Background(),
		`SELECT id FROM api_keys WHERE name='panel-made'`).Scan(&keyID); err != nil {
		t.Fatalf("lookup key id: %v", err)
	}
	revokeResp := env.requestForm(t, http.MethodPost, "/apikeys/"+strconv.FormatInt(keyID, 10)+"/revoke", url.Values{}, true)
	defer revokeResp.Body.Close()
	if revokeResp.StatusCode != http.StatusOK && revokeResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke status=%d", revokeResp.StatusCode)
	}
	var revokedAt string
	if err := env.store.RawDB().QueryRowContext(context.Background(),
		`SELECT COALESCE(revoked_at,'') FROM api_keys WHERE id=?`, keyID).Scan(&revokedAt); err != nil {
		t.Fatalf("read revoked_at: %v", err)
	}
	if revokedAt == "" {
		t.Fatalf("revoked_at not set after SSR revoke")
	}
}
