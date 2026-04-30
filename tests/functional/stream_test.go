package functional_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestFunctional_Stream_PublishesOpsSnapshot verifies the /api/v1/stream
// endpoint fully:
//   - upgrade succeeds when authenticated as admin
//   - the ops_snapshot event is broadcast within a few seconds
//   - the payload matches the documented shape
//
// The publisher only runs while StartWorkers is alive (started by
// setupTestEnv via go a.StartWorkers(ctx)) and only emits when there is at
// least one subscriber, so this test exercises both the lazy-publish gate
// and the upgrade path end-to-end.
func TestFunctional_Stream_PublishesOpsSnapshot(t *testing.T) {
	env := setupTestEnv(t)

	wsURL := streamURLFor(t, env.http.URL)
	conn := dialAdminWebSocket(t, env, wsURL)
	defer conn.Close()

	// Wait up to 10s for the first ops_snapshot. Publisher cadence is 5s.
	deadline := time.Now().Add(10 * time.Second)
	_ = conn.SetReadDeadline(deadline)

	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read message: %v", err)
		}
		var ev struct {
			Kind string          `json:"kind"`
			At   time.Time       `json:"at"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(payload, &ev); err != nil {
			t.Fatalf("decode envelope: %v (raw=%s)", err, string(payload))
		}
		if ev.Kind != "ops_snapshot" {
			continue
		}
		var snap struct {
			Host   map[string]any   `json:"host"`
			Online []map[string]any `json:"online"`
			Nodes  []map[string]any `json:"nodes"`
		}
		if err := json.Unmarshal(ev.Data, &snap); err != nil {
			t.Fatalf("decode snapshot: %v (raw=%s)", err, string(ev.Data))
		}
		if snap.Online == nil || snap.Nodes == nil {
			t.Fatalf("snapshot missing arrays: %+v", snap)
		}
		return
	}
}

// TestFunctional_Stream_UnauthRejects ensures the WebSocket route is gated by
// the admin middleware: an unauthenticated upgrade attempt must fail.
func TestFunctional_Stream_UnauthRejects(t *testing.T) {
	env := setupTestEnv(t)

	wsURL := streamURLFor(t, env.http.URL)
	header := http.Header{}
	// Deliberately omit basic auth.
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err == nil {
		t.Fatalf("expected dial error for unauthenticated request")
	}
	if resp == nil {
		// Some failures (e.g. TLS) won't have a response; that's fine — the
		// important part is the upgrade did not succeed.
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatalf("upgrade succeeded without auth")
	}
}

func streamURLFor(t *testing.T, base string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse base url: %v", err)
	}
	scheme := "ws"
	if strings.EqualFold(u.Scheme, "https") {
		scheme = "wss"
	}
	return scheme + "://" + u.Host + "/api/v1/stream"
}

func dialAdminWebSocket(t *testing.T, env *testEnv, wsURL string) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	cred := env.cfg.AdminUser + ":" + env.cfg.AdminPass
	header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(cred)))

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial %s failed: %v (status=%d)", wsURL, err, resp.StatusCode)
		}
		t.Fatalf("dial %s failed: %v", wsURL, err)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	return conn
}
