package functional_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fetchSubscription is a small helper that mirrors env.request but lets the
// caller override the User-Agent (which env.request does not expose).
func (e *testEnv) fetchSubscription(t *testing.T, path string, ua string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.http.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func setupSubTestUser(t *testing.T, env *testEnv) string {
	t.Helper()
	userID, _ := createUserViaAPI(t, env)
	if _, err := env.store.RawDB().ExecContext(context.Background(),
		`DELETE FROM subscription_tokens WHERE user_id = ?`, userID); err != nil {
		t.Fatalf("clear sub tokens: %v", err)
	}
	resp := env.request(t, http.MethodPost,
		fmt.Sprintf("/api/v1/users/%d/subscription/rotate", userID), map[string]any{}, true, "")
	mustStatus(t, resp, http.StatusOK)
	sub := decodeBodyMap(t, resp)
	token, _ := sub["token"].(string)
	if token == "" {
		t.Fatalf("rotate returned empty token")
	}
	createNodeViaBasicAuth(t, env, fmt.Sprintf("sub-hdr-node-%d", time.Now().UnixNano()))
	return token
}

func TestFunctional_Subscription_HeadersIncludeUserinfoAndUpdateInterval(t *testing.T) {
	env := setupTestEnv(t)
	token := setupSubTestUser(t, env)

	resp := env.fetchSubscription(t, "/sub/"+token+"?target=v2rayn", "")
	defer resp.Body.Close()
	mustStatus(t, resp, http.StatusOK)

	got := resp.Header.Get("Subscription-Userinfo")
	if got == "" {
		t.Fatalf("Subscription-Userinfo header is missing")
	}
	for _, key := range []string{"upload=", "download=", "total=", "expire="} {
		if !strings.Contains(got, key) {
			t.Errorf("Subscription-Userinfo missing %q field; got %q", key, got)
		}
	}

	if iv := resp.Header.Get("Profile-Update-Interval"); iv != "24" {
		t.Errorf("Profile-Update-Interval = %q, want 24", iv)
	}

	// Drain the body so the connection can be reused; we don't assert on
	// payload here because TestFunctional_SubscriptionAndTelegramBindEndpoints
	// already covers that.
	_, _ = io.Copy(io.Discard, resp.Body)
}

func TestFunctional_Subscription_UserAgentPicksTarget(t *testing.T) {
	env := setupTestEnv(t)
	token := setupSubTestUser(t, env)

	tests := []struct {
		name        string
		ua          string
		wantContent string
	}{
		{name: "clash UA → yaml", ua: "ClashforWindows/0.20.39", wantContent: "text/yaml"},
		{name: "singbox UA → json", ua: "sing-box/1.8.0", wantContent: "application/json"},
		{name: "shadowrocket UA → plain", ua: "Shadowrocket/2155 CFNetwork/1240", wantContent: "text/plain"},
		{name: "unknown UA → v2rayn (text/plain)", ua: "Mozilla/5.0", wantContent: "text/plain"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			resp := env.fetchSubscription(t, "/sub/"+token, tc.ua)
			defer resp.Body.Close()
			mustStatus(t, resp, http.StatusOK)
			ct := resp.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, tc.wantContent) {
				t.Fatalf("UA %q: Content-Type %q, want prefix %q", tc.ua, ct, tc.wantContent)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
		})
	}
}

func TestFunctional_Subscription_ExplicitTargetOverridesUA(t *testing.T) {
	env := setupTestEnv(t)
	token := setupSubTestUser(t, env)

	// Clash UA but explicit target=v2rayn → must serve v2rayn (base64).
	resp := env.fetchSubscription(t, "/sub/"+token+"?target=v2rayn", "ClashforWindows/0.20.39")
	defer resp.Body.Close()
	mustStatus(t, resp, http.StatusOK)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("explicit target should win; Content-Type=%q", ct)
	}
}
