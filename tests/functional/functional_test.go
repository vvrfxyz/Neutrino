package functional_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"neutrino/internal/app"
	"neutrino/internal/certutil"
	"neutrino/internal/config"
	"neutrino/internal/db"
	"neutrino/internal/repo"
)

func TestMain(m *testing.M) {
	_, file, _, ok := runtime.Caller(0)
	if ok {
		root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
		_ = os.Chdir(root)
	}
	os.Exit(m.Run())
}

type testEnv struct {
	cfg    config.Config
	store  *repo.Store
	http   *httptest.Server
	client *http.Client
	mtls   *httptest.Server
	caCert *x509.Certificate
	caKey  any
	caPool *x509.CertPool
	cancel context.CancelFunc
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")

	conn, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if err := db.Migrate(conn); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	cfg := config.Load()
	cfg.DBPath = dbPath
	cfg.AllowBasicAuth = true
	cfg.AdminUser = "test-admin"
	cfg.AdminPass = "test-admin-pass"
	cfg.SubBaseURL = "http://127.0.0.1:18080"
	cfg.ProxyHost = "example.com"
	cfg.RealityPublicK = "REPLACE_WITH_REALITY_PUBLIC_KEY"
	cfg.RealityShortID = "REPLACE_WITH_SHORT_ID"
	cfg.TelegramBotToken = ""

	store := repo.New(conn, cfg)
	if err := store.UpsertAdminCredential(context.Background(), cfg.AdminUser, cfg.AdminPass); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	a := app.New(cfg, store)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.StartWorkers(ctx)

	ts := httptest.NewServer(a.Routes())
	t.Cleanup(ts.Close)

	caCert, caKey, caPool := mustTestCA(t)
	mtlsSrv := newMTLSTestServer(t, a.AgentRoutes(), caCert, caKey, caPool)

	return &testEnv{
		cfg:    cfg,
		store:  store,
		http:   ts,
		client: ts.Client(),
		mtls:   mtlsSrv,
		caCert: caCert,
		caKey:  caKey,
		caPool: caPool,
		cancel: cancel,
	}
}

func (e *testEnv) request(t *testing.T, method, path string, body any, basicAuth bool, apiKey string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.http.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if basicAuth {
		req.SetBasicAuth(e.cfg.AdminUser, e.cfg.AdminPass)
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("do request %s %s: %v", method, path, err)
	}
	return resp
}

func (e *testEnv) mtlsClientForNode(t *testing.T, nodeID int64) *http.Client {
	t.Helper()

	if nodeID <= 0 {
		t.Fatalf("invalid node id")
	}

	// Create a short-lived node client cert (CN=node-<id>) and allowlist it.
	cert, tlsCert := mustNodeClientCert(t, e.caCert, e.caKey, nodeID)
	if err := e.store.UpsertNodeCertPin(context.Background(), nodeID, "agent_client", cert, time.Now().UTC()); err != nil {
		t.Fatalf("upsert node cert pin: %v", err)
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			RootCAs:      e.caPool,
			Certificates: []tls.Certificate{tlsCert},
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 15 * time.Second}
}

func (e *testEnv) requestMTLS(t *testing.T, nodeID int64, method, path string, body any) *http.Response {
	t.Helper()
	c := e.mtlsClientForNode(t, nodeID)
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.mtls.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do mtls request %s %s: %v", method, path, err)
	}
	return resp
}

func (e *testEnv) requestForm(t *testing.T, method, path string, form url.Values, basicAuth bool) *http.Response {
	t.Helper()
	var reader io.Reader
	if form != nil {
		reader = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, e.http.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basicAuth {
		req.SetBasicAuth(e.cfg.AdminUser, e.cfg.AdminPass)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("do request %s %s: %v", method, path, err)
	}
	return resp
}

func decodeBodyMap(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal map: %v body=%s", err, string(b))
	}
	return out
}

func decodeBodyText(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body text: %v", err)
	}
	return string(b)
}

func mustStatus(t *testing.T, resp *http.Response, expected int) {
	t.Helper()
	if resp.StatusCode != expected {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("unexpected status got=%d want=%d body=%s", resp.StatusCode, expected, string(b))
	}
}

func extractUserID(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	uv, ok := payload["user"]
	if !ok {
		t.Fatalf("missing user in payload")
	}
	um, ok := uv.(map[string]any)
	if !ok {
		t.Fatalf("invalid user payload type")
	}
	idRaw, ok := um["ID"]
	if !ok {
		t.Fatalf("missing user.ID")
	}
	switch v := idRaw.(type) {
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	default:
		t.Fatalf("unsupported id type %T", idRaw)
	}
	return 0
}

func createUserViaAPI(t *testing.T, e *testEnv) (int64, string) {
	t.Helper()
	username := fmt.Sprintf("u-%d", time.Now().UnixNano())
	resp := e.request(t, http.MethodPost, "/api/v1/users", map[string]any{
		"username":         username,
		"monthly_limit_gb": 20,
		"counting_mode":    "double",
		"quota_cycle":      "day",
		"quota_tz":         "Asia/Shanghai",
		"device_limit":     2,
		"plan_days":        7,
	}, true, "")
	mustStatus(t, resp, http.StatusCreated)
	payload := decodeBodyMap(t, resp)
	return extractUserID(t, payload), username
}

func createNodeViaBasicAuth(t *testing.T, e *testEnv, name string) int64 {
	t.Helper()
	resp := e.request(t, http.MethodPost, "/api/v1/nodes", map[string]any{
		"name":      name,
		"core_type": "xray",
		"protocol":  "vless_reality",
		"host":      "example.com",
		"port":      443,
		"enabled":   true,
	}, true, "")
	mustStatus(t, resp, http.StatusCreated)
	payload := decodeBodyMap(t, resp)
	idNum, _ := payload["id"].(float64)
	if int64(idNum) <= 0 {
		t.Fatalf("invalid node id payload=%v", payload)
	}
	return int64(idNum)
}

func postOnlineSnapshot(t *testing.T, e *testEnv, nodeID int64, observedAt time.Time, items []map[string]any) {
	t.Helper()
	resp := e.requestMTLS(t, nodeID, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/report", nodeID), map[string]any{
		"reported_at": observedAt.Format(time.RFC3339),
		"online_snapshot": map[string]any{
			"observed_at": observedAt.Format(time.RFC3339),
			"items":       items,
		},
	})
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)
}

func createUserDirect(t *testing.T, e *testEnv) repo.User {
	t.Helper()
	u, err := e.store.CreateUser(context.Background(), repo.CreateUserInput{
		Username:       fmt.Sprintf("direct-u-%d", time.Now().UnixNano()),
		MonthlyLimitGB: 20,
		CountingMode:   "double",
		QuotaCycle:     "day",
		QuotaTZ:        "Asia/Shanghai",
		DeviceLimit:    2,
		PlanDays:       7,
	})
	if err != nil {
		t.Fatalf("create user direct: %v", err)
	}
	return u
}

func createManagedXrayNodeDirect(t *testing.T, e *testEnv, name string) repo.Node {
	t.Helper()
	n, err := e.store.CreateNode(context.Background(), repo.CreateNodeInput{
		Name:     name,
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
		Managed:  true,
	})
	if err != nil {
		t.Fatalf("create managed node direct: %v", err)
	}
	return n
}

func clearNodeJobs(t *testing.T, e *testEnv, nodeID int64) {
	t.Helper()
	if _, err := e.store.RawDB().ExecContext(context.Background(), `DELETE FROM node_jobs WHERE node_id = ?`, nodeID); err != nil {
		t.Fatalf("clear node jobs: %v", err)
	}
}

func expireUserDirect(t *testing.T, e *testEnv, userID int64) {
	t.Helper()
	expiredAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := e.store.RawDB().ExecContext(context.Background(), `
UPDATE users
SET status = 'active', expires_at = ?, removed_at = NULL
WHERE id = ?;
`, expiredAt, userID); err != nil {
		t.Fatalf("expire user direct: %v", err)
	}
}

func mustTestCA(t *testing.T) (*x509.Certificate, any, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen ca key: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	now := time.Now().UTC()
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "functional-test-ca"},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return cert, key, pool
}

func newMTLSTestServer(t *testing.T, handler http.Handler, caCert *x509.Certificate, caKey any, caPool *x509.CertPool) *httptest.Server {
	t.Helper()
	if handler == nil || caCert == nil || caKey == nil || caPool == nil {
		t.Fatalf("invalid mtls server inputs")
	}

	// Server cert for 127.0.0.1 and localhost.
	sKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen server key: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	now := time.Now().UTC()
	sTpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "panel-agent-mtls"},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, sTpl, caCert, &sKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("sign server cert: %v", err)
	}
	sCert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse server cert: %v", err)
	}

	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		Certificates: []tls.Certificate{mustTLSCert(t, sCert, sKey)},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func mustTLSCert(t *testing.T, cert *x509.Certificate, key *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()
	certPEM := []byte(certutil.CertToPEM(cert))
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := []byte(pemEncode("EC PRIVATE KEY", keyDER))
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("x509 key pair: %v", err)
	}
	return tlsCert
}

func pemEncode(typ string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
}

func mustNodeClientCert(t *testing.T, caCert *x509.Certificate, caKey any, nodeID int64) (*x509.Certificate, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen node key: %v", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: fmt.Sprintf("node-%d", nodeID)},
	}, key)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		t.Fatalf("parse csr: %v", err)
	}
	cert, err := certutil.SignCSR(certutil.SignCSRInput{
		CACert:      caCert,
		CAKey:       caKey,
		CSR:         csr,
		NotBefore:   time.Now().UTC().Add(-1 * time.Hour),
		NotAfter:    time.Now().UTC().Add(24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		KeyUsage:    x509.KeyUsageDigitalSignature,
	})
	if err != nil {
		t.Fatalf("sign node cert: %v", err)
	}
	tlsCert := mustTLSCert(t, cert, key)
	return cert, tlsCert
}

func TestFunctional_AdminUserLifecycle(t *testing.T) {
	env := setupTestEnv(t)

	userID, _ := createUserViaAPI(t, env)
	if userID <= 0 {
		t.Fatalf("invalid user id")
	}

	resp := env.request(t, http.MethodGet, fmt.Sprintf("/api/v1/users/%d", userID), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	resp = env.request(t, http.MethodPost, fmt.Sprintf("/api/v1/users/%d/quota/credit", userID), map[string]any{"bytes": 1024}, true, "")
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	resp = env.request(t, http.MethodPost, fmt.Sprintf("/api/v1/users/%d/quota/reset", userID), map[string]any{"reason": "functional"}, true, "")
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	resp = env.request(t, http.MethodPost, fmt.Sprintf("/api/v1/users/%d/plan/extend", userID), map[string]any{"days": 3}, true, "")
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	resp = env.request(t, http.MethodGet, "/api/v1/users", nil, false, "")
	mustStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()
}

func TestFunctional_UINavAndTrafficFilters(t *testing.T) {
	env := setupTestEnv(t)

	userID1, username1 := createUserViaAPI(t, env)
	userID2, _ := createUserViaAPI(t, env)

	nodeName1 := fmt.Sprintf("node-a-%d", time.Now().UnixNano())
	node1 := createNodeViaBasicAuth(t, env, nodeName1)
	nodeName2 := fmt.Sprintf("node-b-%d", time.Now().UnixNano())
	node2 := createNodeViaBasicAuth(t, env, nodeName2)

	now := time.Now().UTC()
	resp := env.requestMTLS(t, node1, http.MethodPost, "/api/v1/usage", map[string]any{
		"events": []map[string]any{
			{
				"user_id":         userID1,
				"direction":       "outbound",
				"bytes":           111,
				"source":          "functional",
				"source_event_id": fmt.Sprintf("evt-ui-1-%d", now.UnixNano()),
				"at":              now.Format(time.RFC3339),
			},
		},
	})
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)
	resp = env.requestMTLS(t, node2, http.MethodPost, "/api/v1/usage", map[string]any{
		"events": []map[string]any{
			{
				"user_id":         userID2,
				"direction":       "outbound",
				"bytes":           222,
				"source":          "functional",
				"source_event_id": fmt.Sprintf("evt-ui-2-%d", now.UnixNano()),
				"at":              now.Add(2 * time.Second).Format(time.RFC3339),
			},
		},
	})
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	resp = env.request(t, http.MethodGet, "/users", nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	usersHTML := decodeBodyText(t, resp)
	if strings.Contains(usersHTML, "API Keys") || strings.Contains(usersHTML, "Alerts") {
		t.Fatalf("users page still contains removed tabs")
	}

	resp = env.request(t, http.MethodGet, "/traffic", nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	trafficHTML := decodeBodyText(t, resp)
	if strings.Contains(trafficHTML, "API Keys") || strings.Contains(trafficHTML, "Alerts") {
		t.Fatalf("traffic page still contains removed tabs")
	}
	if !strings.Contains(trafficHTML, fmt.Sprintf(`<option value="%d">%s</option>`, userID1, username1)) {
		t.Fatalf("traffic page missing user option for id=%d username=%s", userID1, username1)
	}
	if !strings.Contains(trafficHTML, fmt.Sprintf(`<option value="%d">%s</option>`, node1, nodeName1)) {
		t.Fatalf("traffic page missing node option for id=%d name=%s", node1, nodeName1)
	}

	resp = env.request(t, http.MethodGet, "/apikeys", nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()

	resp = env.request(t, http.MethodGet, "/alerts", nil, true, "")
	mustStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()

	resp = env.request(t, http.MethodGet, fmt.Sprintf("/api/v1/traffic/summary?range=1h&user_id=%d", userID1), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	summaryByUser := decodeBodyMap(t, resp)
	kpiByUser, ok := summaryByUser["kpi"].(map[string]any)
	if !ok {
		t.Fatalf("missing kpi in user summary payload=%v", summaryByUser)
	}
	if got := int64(kpiByUser["total_out"].(float64)); got != 111 {
		t.Fatalf("unexpected filtered user total_out=%d payload=%v", got, summaryByUser)
	}

	resp = env.request(t, http.MethodGet, fmt.Sprintf("/api/v1/traffic/summary?range=1h&node_id=%d", node1), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	summaryByNode := decodeBodyMap(t, resp)
	kpiByNode, ok := summaryByNode["kpi"].(map[string]any)
	if !ok {
		t.Fatalf("missing kpi in node summary payload=%v", summaryByNode)
	}
	if got := int64(kpiByNode["total_out"].(float64)); got != 111 {
		t.Fatalf("unexpected filtered node total_out=%d payload=%v", got, summaryByNode)
	}
}

func TestFunctional_UserDetailTrafficAPIAndOnlineSessions(t *testing.T) {
	env := setupTestEnv(t)

	userID, username := createUserViaAPI(t, env)
	node1 := createNodeViaBasicAuth(t, env, fmt.Sprintf("detail-node-a-%d", time.Now().UnixNano()))
	node2 := createNodeViaBasicAuth(t, env, fmt.Sprintf("detail-node-b-%d", time.Now().UnixNano()))

	now := time.Now().UTC().Truncate(time.Minute)
	eventsNode1 := []map[string]any{
		{
			"user_id":         userID,
			"direction":       "inbound",
			"bytes":           512,
			"source":          "xray-stats",
			"source_event_id": fmt.Sprintf("evt-detail-stats-in-%d", now.UnixNano()),
			"at":              now.Format(time.RFC3339),
		},
		{
			"user_id":         userID,
			"direction":       "outbound",
			"bytes":           1024,
			"source":          "xray-stats",
			"source_event_id": fmt.Sprintf("evt-detail-stats-out-%d", now.UnixNano()),
			"at":              now.Add(2 * time.Second).Format(time.RFC3339),
		},
		{
			"user_id":         userID,
			"direction":       "outbound",
			"bytes":           0,
			"source":          "xray-access",
			"source_event_id": fmt.Sprintf("evt-detail-access-a-%d", now.UnixNano()),
			"at":              now.Add(3 * time.Second).Format(time.RFC3339),
			"target_host":     "example.com",
			"sni":             "example.com",
			"destination":     "tcp:example.com:443",
			"client_ip":       "10.0.0.1",
			"inbound_tag":     "vless-main",
		},
	}
	eventsNode2 := []map[string]any{
		{
			"user_id":         userID,
			"direction":       "outbound",
			"bytes":           0,
			"source":          "xray-access",
			"source_event_id": fmt.Sprintf("evt-detail-access-b-%d", now.UnixNano()),
			"at":              now.Add(4 * time.Second).Format(time.RFC3339),
			"target_host":     "api.example.com",
			"sni":             "api.example.com",
			"destination":     "tcp:api.example.com:443",
			"client_ip":       "10.0.0.1",
			"inbound_tag":     "vless-backup",
		},
	}
	for i := 0; i < 320; i++ {
		eventsNode1 = append(eventsNode1, map[string]any{
			"user_id":         userID,
			"direction":       "outbound",
			"bytes":           0,
			"source":          "manual",
			"source_event_id": fmt.Sprintf("evt-detail-hidden-%d-%d", now.UnixNano(), i),
			"at":              now.Add(time.Duration(5+i) * time.Second).Format(time.RFC3339),
			"target_host":     fmt.Sprintf("hidden-%03d.example.com", i),
			"destination":     fmt.Sprintf("tcp:hidden-%03d.example.com:443", i),
		})
	}
	resp := env.requestMTLS(t, node1, http.MethodPost, "/api/v1/usage", map[string]any{
		"events": eventsNode1,
	})
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)
	resp = env.requestMTLS(t, node2, http.MethodPost, "/api/v1/usage", map[string]any{
		"events": eventsNode2,
	})
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)
	postOnlineSnapshot(t, env, node1, now.Add(3*time.Second), []map[string]any{
		{
			"user_id":      userID,
			"client_ip":    "10.0.0.1",
			"last_seen_at": now.Add(3 * time.Second).Format(time.RFC3339),
		},
	})
	postOnlineSnapshot(t, env, node2, now.Add(4*time.Second), []map[string]any{
		{
			"user_id":      userID,
			"client_ip":    "10.0.0.1",
			"last_seen_at": now.Add(4 * time.Second).Format(time.RFC3339),
		},
	})

	resp = env.request(t, http.MethodGet, fmt.Sprintf("/api/v1/users/%d/traffic", userID), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	payload := decodeBodyMap(t, resp)
	if got, _ := payload["period"].(string); got != "hourly" {
		t.Fatalf("unexpected default period got=%q payload=%v", got, payload)
	}
	series, ok := payload["series"].([]any)
	if !ok {
		t.Fatalf("missing series payload=%v", payload)
	}
	if len(series) != 24 {
		t.Fatalf("unexpected series length for default period got=%d want=%d", len(series), 24)
	}
	foundTraffic := false
	for _, item := range series {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("invalid default-period series row type=%T row=%v", item, item)
		}
		if int64(row["inbound_bytes"].(float64)) == 512 && int64(row["outbound_bytes"].(float64)) == 1024 {
			foundTraffic = true
			break
		}
	}
	if !foundTraffic {
		t.Fatalf("expected traffic bucket in default-period payload=%v", payload)
	}

	for period, wantLen := range map[string]int{"hourly": 24, "daily": 30, "monthly": 12} {
		resp = env.request(t, http.MethodGet, fmt.Sprintf("/api/v1/users/%d/traffic?period=%s", userID, period), nil, true, "")
		mustStatus(t, resp, http.StatusOK)
		payload = decodeBodyMap(t, resp)
		if got, _ := payload["period"].(string); got != period {
			t.Fatalf("unexpected period got=%q want=%q payload=%v", got, period, payload)
		}
		series, ok = payload["series"].([]any)
		if !ok {
			t.Fatalf("missing series payload=%v", payload)
		}
		if len(series) != wantLen {
			t.Fatalf("unexpected series length for %s got=%d want=%d", period, len(series), wantLen)
		}
		foundTraffic = false
		for _, item := range series {
			row, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("invalid series row type=%T row=%v", item, item)
			}
			if int64(row["inbound_bytes"].(float64)) == 512 && int64(row["outbound_bytes"].(float64)) == 1024 {
				foundTraffic = true
				break
			}
		}
		if !foundTraffic {
			t.Fatalf("expected traffic bucket in %s payload=%v", period, payload)
		}
	}

	resp = env.request(t, http.MethodGet, fmt.Sprintf("/api/v1/users/%d/traffic?period=invalid", userID), nil, true, "")
	mustStatus(t, resp, http.StatusBadRequest)
	payload = decodeBodyMap(t, resp)
	if got, _ := payload["error"].(string); !strings.Contains(got, "unsupported period") {
		t.Fatalf("unexpected invalid-period error payload=%v", payload)
	}

	resp = env.request(t, http.MethodGet, "/api/v1/users/not-a-number/traffic", nil, true, "")
	mustStatus(t, resp, http.StatusBadRequest)
	if body := strings.TrimSpace(decodeBodyText(t, resp)); body != "bad user id" {
		t.Fatalf("unexpected bad-user-id body=%q", body)
	}

	resp = env.request(t, http.MethodGet, "/api/v1/users/999999999/traffic", nil, true, "")
	mustStatus(t, resp, http.StatusNotFound)
	if body := strings.TrimSpace(decodeBodyText(t, resp)); body != "not found" {
		t.Fatalf("unexpected missing-user body=%q", body)
	}

	resp = env.request(t, http.MethodGet, fmt.Sprintf("/users/%d", userID), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	body := decodeBodyText(t, resp)
	if !strings.Contains(body, username) {
		t.Fatalf("user detail page missing username body=%q", body)
	}
	if !strings.Contains(body, "在线会话") || !strings.Contains(body, "活跃会话") || !strings.Contains(body, "活跃不同 IP") {
		t.Fatalf("user detail page missing online session summary body=%q", body)
	}
	if !strings.Contains(body, "活跃会话</div>\n          <div class=\"text-lg font-semibold text-slate-900 mt-0.5\">2</div>") {
		t.Fatalf("user detail page missing session count body=%q", body)
	}
	if !strings.Contains(body, "活跃不同 IP</div>\n          <div class=\"text-lg font-semibold text-slate-900 mt-0.5\">1</div>") {
		t.Fatalf("user detail page missing IP count body=%q", body)
	}
	if !strings.Contains(body, "10.0.0.1") {
		t.Fatalf("user detail page missing client ip body=%q", body)
	}
	if !strings.Contains(body, fmt.Sprintf("#%d", node1)) || !strings.Contains(body, fmt.Sprintf("#%d", node2)) {
		t.Fatalf("user detail page missing node ids body=%q", body)
	}
	if !strings.Contains(body, "api.example.com") || !strings.Contains(body, "example.com") {
		t.Fatalf("user detail page missing access events body=%q", body)
	}
	if strings.Contains(body, "hidden-000.example.com") || strings.Contains(body, "hidden-319.example.com") {
		t.Fatalf("user detail page should filter newer non-xray-access events body=%q", body)
	}
	if !strings.Contains(body, "x-on:click=\"fetchSeries()\"") || !strings.Contains(body, ">重试</button>") {
		t.Fatalf("user detail page missing retry affordance body=%q", body)
	}
	if !strings.Contains(body, "readErrorMessage(resp)") {
		t.Fatalf("user detail page missing readErrorMessage handler body=%q", body)
	}
	if !strings.Contains(body, "payload.error") {
		t.Fatalf("user detail page missing JSON error parsing body=%q", body)
	}
	if !strings.Contains(body, fmt.Sprintf("trafficChart({ userId: %d })", userID)) {
		t.Fatalf("user detail page missing async traffic bootstrap body=%q", body)
	}
	if !strings.Contains(body, "let chart = null;") {
		t.Fatalf("user detail page should keep chart instance out of Alpine reactive state body=%q", body)
	}
	if strings.Contains(body, "chart: null,") {
		t.Fatalf("user detail page should not store chart instance in Alpine reactive state body=%q", body)
	}
	if !strings.Contains(body, "clearChart()") {
		t.Fatalf("user detail page missing chart teardown helper body=%q", body)
	}
}

func TestFunctional_UsersPageShowsBatchedOnlineStats(t *testing.T) {
	env := setupTestEnv(t)

	userID1, username1 := createUserViaAPI(t, env)
	userID2, username2 := createUserViaAPI(t, env)
	node1 := createNodeViaBasicAuth(t, env, fmt.Sprintf("users-node-a-%d", time.Now().UnixNano()))
	node2 := createNodeViaBasicAuth(t, env, fmt.Sprintf("users-node-b-%d", time.Now().UnixNano()))

	now := time.Now().UTC().Truncate(time.Minute)
	resp := env.requestMTLS(t, node1, http.MethodPost, "/api/v1/usage", map[string]any{
		"events": []map[string]any{
			{
				"user_id":         userID1,
				"direction":       "outbound",
				"bytes":           0,
				"source":          "xray-access",
				"source_event_id": fmt.Sprintf("evt-users-a-%d", now.UnixNano()),
				"at":              now.Format(time.RFC3339),
				"client_ip":       "10.8.0.1",
			},
			{
				"user_id":         userID1,
				"direction":       "outbound",
				"bytes":           0,
				"source":          "xray-access",
				"source_event_id": fmt.Sprintf("evt-users-c-%d", now.UnixNano()),
				"at":              now.Add(2 * time.Second).Format(time.RFC3339),
				"client_ip":       "10.8.0.2",
			},
			{
				"user_id":         userID2,
				"direction":       "outbound",
				"bytes":           0,
				"source":          "xray-access",
				"source_event_id": fmt.Sprintf("evt-users-d-%d", now.UnixNano()),
				"at":              now.Add(3 * time.Second).Format(time.RFC3339),
				"client_ip":       "10.8.1.1",
			},
		},
	})
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)
	resp = env.requestMTLS(t, node2, http.MethodPost, "/api/v1/usage", map[string]any{
		"events": []map[string]any{
			{
				"user_id":         userID1,
				"direction":       "outbound",
				"bytes":           0,
				"source":          "xray-access",
				"source_event_id": fmt.Sprintf("evt-users-b-%d", now.UnixNano()),
				"at":              now.Add(time.Second).Format(time.RFC3339),
				"client_ip":       "10.8.0.1",
			},
		},
	})
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)
	postOnlineSnapshot(t, env, node1, now.Add(3*time.Second), []map[string]any{
		{
			"user_id":      userID1,
			"client_ip":    "10.8.0.1",
			"last_seen_at": now.Format(time.RFC3339),
		},
		{
			"user_id":      userID1,
			"client_ip":    "10.8.0.2",
			"last_seen_at": now.Add(2 * time.Second).Format(time.RFC3339),
		},
		{
			"user_id":      userID2,
			"client_ip":    "10.8.1.1",
			"last_seen_at": now.Add(3 * time.Second).Format(time.RFC3339),
		},
	})
	postOnlineSnapshot(t, env, node2, now.Add(3*time.Second), []map[string]any{
		{
			"user_id":      userID1,
			"client_ip":    "10.8.0.1",
			"last_seen_at": now.Add(time.Second).Format(time.RFC3339),
		},
	})

	resp = env.request(t, http.MethodGet, "/users", nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	body := decodeBodyText(t, resp)
	if !strings.Contains(body, "在线") {
		t.Fatalf("users page missing online column body=%q", body)
	}
	if !strings.Contains(body, username1) || !strings.Contains(body, username2) {
		t.Fatalf("users page missing usernames body=%q", body)
	}
	if !strings.Contains(body, ">会话 <span class=\"font-medium text-slate-800\">3</span></div>") {
		t.Fatalf("users page missing session count for user1 body=%q", body)
	}
	if !strings.Contains(body, ">不同 IP <span class=\"font-medium text-slate-800\">2</span></div>") {
		t.Fatalf("users page missing IP count for user1 body=%q", body)
	}
	if !strings.Contains(body, ">会话 <span class=\"font-medium text-slate-800\">1</span></div>") {
		t.Fatalf("users page missing session count for user2 body=%q", body)
	}
	if !strings.Contains(body, ">不同 IP <span class=\"font-medium text-slate-800\">1</span></div>") {
		t.Fatalf("users page missing IP count for user2 body=%q", body)
	}
}

func TestFunctional_OpsAndNodesCleanup(t *testing.T) {
	env := setupTestEnv(t)

	userID, _ := createUserViaAPI(t, env)
	node1 := createNodeViaBasicAuth(t, env, fmt.Sprintf("ops-node-a-%d", time.Now().UnixNano()))
	node2 := createNodeViaBasicAuth(t, env, fmt.Sprintf("ops-node-b-%d", time.Now().UnixNano()))

	now := time.Now().UTC().Truncate(time.Minute)
	resp := env.requestMTLS(t, node1, http.MethodPost, "/api/v1/usage", map[string]any{
		"events": []map[string]any{
			{
				"user_id":         userID,
				"direction":       "outbound",
				"bytes":           0,
				"source":          "xray-access",
				"source_event_id": fmt.Sprintf("evt-ops-access-a-%d", now.UnixNano()),
				"at":              now.Format(time.RFC3339),
				"target_host":     "example.com",
				"sni":             "example.com",
				"destination":     "tcp:example.com:443",
				"client_ip":       "10.9.0.1",
			},
		},
	})
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)
	resp = env.requestMTLS(t, node2, http.MethodPost, "/api/v1/usage", map[string]any{
		"events": []map[string]any{
			{
				"user_id":         userID,
				"direction":       "outbound",
				"bytes":           0,
				"source":          "xray-access",
				"source_event_id": fmt.Sprintf("evt-ops-access-b-%d", now.UnixNano()),
				"at":              now.Add(2 * time.Second).Format(time.RFC3339),
				"target_host":     "api.example.com",
				"sni":             "api.example.com",
				"destination":     "tcp:api.example.com:443",
				"client_ip":       "10.9.0.1",
			},
		},
	})
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)
	postOnlineSnapshot(t, env, node1, now.Add(3*time.Second), []map[string]any{
		{
			"user_id":      userID,
			"client_ip":    "10.9.0.1",
			"last_seen_at": now.Format(time.RFC3339),
		},
	})
	postOnlineSnapshot(t, env, node2, now.Add(3*time.Second), []map[string]any{
		{
			"user_id":      userID,
			"client_ip":    "10.9.0.1",
			"last_seen_at": now.Add(2 * time.Second).Format(time.RFC3339),
		},
	})

	resp = env.requestMTLS(t, node1, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/report", node1), map[string]any{
		"reported_at": now.Add(-4 * time.Second).Format(time.RFC3339),
		"metrics": map[string]any{
			"month_key":          "2026-03",
			"month_timezone":     "Asia/Shanghai",
			"net_counter_source": "proc:/host/proc/1/net",
			"net_rx_total_bytes": 100000,
			"net_tx_total_bytes": 200000,
		},
	})
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	resp = env.requestMTLS(t, node1, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/report", node1), map[string]any{
		"reported_at": now.Add(-3 * time.Second).Format(time.RFC3339),
		"metrics": map[string]any{
			"cpu_percent":        12.5,
			"memory_bytes":       2048,
			"inbound_bps":        4096,
			"outbound_bps":       8192,
			"month_key":          "2026-03",
			"month_timezone":     "Asia/Shanghai",
			"net_counter_source": "proc:/host/proc/1/net",
			"net_rx_total_bytes": 223456,
			"net_tx_total_bytes": 854321,
			"disk_used_percent":  62.5,
			"uptime_sec":         3661,
			"queue_bytes":        512,
			"queue_batches":      3,
			"goroutines":         321,
		},
	})
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	resp = env.request(t, http.MethodGet, "/api/v1/online-users", nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	onlinePayload := decodeBodyMap(t, resp)
	items, ok := onlinePayload["items"].([]any)
	if !ok {
		t.Fatalf("missing online items payload=%v", onlinePayload)
	}
	seenNode1 := false
	seenNode2 := false
	for _, raw := range items {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("invalid online row type=%T row=%v", raw, raw)
		}
		if row["client_ip"] != "10.9.0.1" {
			continue
		}
		nodeID, _ := row["node_id"].(float64)
		switch int64(nodeID) {
		case node1:
			seenNode1 = true
		case node2:
			seenNode2 = true
		}
	}
	if !seenNode1 || !seenNode2 {
		t.Fatalf("expected online users from both nodes payload=%v", onlinePayload)
	}

	resp = env.request(t, http.MethodGet, "/api/v1/ops/nodes", nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	nodesPayload := decodeBodyMap(t, resp)
	nodeItems, ok := nodesPayload["items"].([]any)
	if !ok {
		t.Fatalf("missing ops nodes items payload=%v", nodesPayload)
	}
	foundRuntime := false
	for _, raw := range nodeItems {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("invalid node row type=%T row=%v", raw, raw)
		}
		id, _ := row["id"].(float64)
		if int64(id) != node1 {
			continue
		}
		metrics, ok := row["agent_metrics"].(map[string]any)
		if !ok {
			t.Fatalf("missing agent_metrics row=%v", row)
		}
		if got := int(metrics["goroutines"].(float64)); got != 321 {
			t.Fatalf("unexpected goroutines=%d row=%v", got, row)
		}
		if got, _ := row["agent_metrics_at"].(string); strings.TrimSpace(got) == "" {
			t.Fatalf("missing agent_metrics_at row=%v", row)
		} else if got != now.Add(-3*time.Second).Format(time.RFC3339) {
			t.Fatalf("expected agent_metrics_at to follow node reported_at got=%q want=%q row=%v", got, now.Add(-3*time.Second).Format(time.RFC3339), row)
		}
		if got, _ := row["last_seen_at"].(string); strings.TrimSpace(got) == "" {
			t.Fatalf("missing last_seen_at row=%v", row)
		} else if got == now.Add(-3*time.Second).Format(time.RFC3339) {
			t.Fatalf("expected last_seen_at to reflect panel receive time, not node reported_at row=%v", row)
		}
		monthUsage, ok := row["month_usage"].(map[string]any)
		if !ok {
			t.Fatalf("missing month_usage row=%v", row)
		}
		monthLoc, err := time.LoadLocation("Asia/Shanghai")
		if err != nil {
			t.Fatalf("load month location: %v", err)
		}
		wantMonthKey := now.Add(-3 * time.Second).In(monthLoc).Format("2006-01")
		if got, _ := monthUsage["month_key"].(string); got != wantMonthKey {
			t.Fatalf("unexpected month key=%q want=%q row=%v", got, wantMonthKey, row)
		}
		if got := int64(monthUsage["rx_bytes"].(float64)); got != 123456 {
			t.Fatalf("unexpected month rx=%d row=%v", got, row)
		}
		if got := int64(monthUsage["tx_bytes"].(float64)); got != 654321 {
			t.Fatalf("unexpected month tx=%d row=%v", got, row)
		}
		if got, _ := monthUsage["last_reported_at"].(string); got != now.Add(-3*time.Second).Format(time.RFC3339) {
			t.Fatalf("expected month last_reported_at to follow node reported_at got=%q want=%q row=%v", got, now.Add(-3*time.Second).Format(time.RFC3339), row)
		}
		foundRuntime = true
	}
	if !foundRuntime {
		t.Fatalf("expected ops nodes runtime for node=%d payload=%v", node1, nodesPayload)
	}

	resp = env.request(t, http.MethodGet, "/ops", nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	opsBody := decodeBodyText(t, resp)
	if !strings.Contains(opsBody, "item.user_id + '-' + item.client_ip + '-' + (item.node_id || 0)") {
		t.Fatalf("ops page missing node-aware online row key body=%q", opsBody)
	}
	if !strings.Contains(opsBody, "fmtPct(n.agent_metrics.cpu_percent)") {
		t.Fatalf("ops page missing structured runtime rendering body=%q", opsBody)
	}
	if !strings.Contains(opsBody, "fmtTimeWithAgo(") {
		t.Fatalf("ops page missing relative time formatter body=%q", opsBody)
	}
	if strings.Contains(opsBody, "runtimeText(n)") {
		t.Fatalf("ops page still references runtimeText helper body=%q", opsBody)
	}
	for _, marker := range []string{"控制面任务", "配置版本", "运行指标", "最近错误", "自然月累计", "面板本月流量"} {
		if !strings.Contains(opsBody, marker) {
			t.Fatalf("ops page missing redesigned node section marker=%q body=%q", marker, opsBody)
		}
	}

	resp = env.request(t, http.MethodGet, "/nodes", nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	nodesBody := decodeBodyText(t, resp)
	if !strings.Contains(nodesBody, fmt.Sprintf("/nodes/%d/deploy", node1)) {
		t.Fatalf("nodes page missing deploy action body=%q", nodesBody)
	}
	if !strings.Contains(nodesBody, fmt.Sprintf("/api/v1/nodes/%d/disable", node1)) {
		t.Fatalf("nodes page missing enable/disable action body=%q", nodesBody)
	}
	if strings.Contains(nodesBody, "go 321") || strings.Contains(nodesBody, "cpu 12.5%") {
		t.Fatalf("nodes page should not render runtime metrics body=%q", nodesBody)
	}
}

func TestFunctional_UsageIdempotencyAndNodeIDOverride(t *testing.T) {
	env := setupTestEnv(t)

	node1 := createNodeViaBasicAuth(t, env, "n1")
	node2 := createNodeViaBasicAuth(t, env, "n2")

	userID, _ := createUserViaAPI(t, env)

	evtID := fmt.Sprintf("evt-%d", time.Now().UnixNano())
	body := map[string]any{
		"events": []map[string]any{{
			"user_id":         userID,
			"node_id":         node2, // should be overridden to node1 by mTLS node identity
			"direction":       "outbound",
			"bytes":           123,
			"source":          "functional",
			"source_event_id": evtID,
			"at":              time.Now().UTC().Format(time.RFC3339),
		}},
	}

	resp := env.requestMTLS(t, node1, http.MethodPost, "/api/v1/usage", body)
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	resp = env.requestMTLS(t, node1, http.MethodPost, "/api/v1/usage", body)
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	resp = env.request(t, http.MethodGet, fmt.Sprintf("/api/v1/traffic/summary?range=1h&node_id=%d", node1), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	s1 := decodeBodyMap(t, resp)
	kpi1 := s1["kpi"].(map[string]any)
	totalOut1 := int64(kpi1["total_out"].(float64))
	if totalOut1 != 123 {
		t.Fatalf("unexpected node1 total_out=%d payload=%v", totalOut1, s1)
	}

	resp = env.request(t, http.MethodGet, fmt.Sprintf("/api/v1/traffic/summary?range=1h&node_id=%d", node2), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	s2 := decodeBodyMap(t, resp)
	kpi2 := s2["kpi"].(map[string]any)
	totalOut2 := int64(kpi2["total_out"].(float64))
	if totalOut2 != 0 {
		t.Fatalf("unexpected node2 total_out=%d payload=%v", totalOut2, s2)
	}

	resp = env.request(t, http.MethodGet, fmt.Sprintf("/api/v1/traffic/summary?range=1h&user_id=%d", userID), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	s3 := decodeBodyMap(t, resp)
	kpi3 := s3["kpi"].(map[string]any)
	totalOut3 := int64(kpi3["total_out"].(float64))
	if totalOut3 != 123 {
		t.Fatalf("unexpected user total_out=%d payload=%v", totalOut3, s3)
	}
}

func TestFunctional_UsageRejectsUserOutsideNodeAccess(t *testing.T) {
	env := setupTestEnv(t)

	node1 := createNodeViaBasicAuth(t, env, "restricted-writer-node")
	node2 := createNodeViaBasicAuth(t, env, "restricted-target-node")

	resp := env.request(t, http.MethodPost, "/api/v1/users", map[string]any{
		"username":         fmt.Sprintf("restricted-usage-%d", time.Now().UnixNano()),
		"monthly_limit_gb": 20,
		"counting_mode":    "double",
		"quota_cycle":      "day",
		"quota_tz":         "Asia/Shanghai",
		"device_limit":     2,
		"plan_days":        7,
		"node_ids":         []int64{node2},
	}, true, "")
	mustStatus(t, resp, http.StatusCreated)
	userID := extractUserID(t, decodeBodyMap(t, resp))

	resp = env.requestMTLS(t, node1, http.MethodPost, "/api/v1/usage", map[string]any{
		"events": []map[string]any{{
			"user_id":         userID,
			"direction":       "outbound",
			"bytes":           64,
			"source":          "functional",
			"source_event_id": fmt.Sprintf("wrong-node-%d", time.Now().UnixNano()),
			"at":              time.Now().UTC().Format(time.RFC3339),
		}},
	})
	mustStatus(t, resp, http.StatusOK)
	payload := decodeBodyMap(t, resp)
	results, ok := payload["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("unexpected usage results payload=%v", payload)
	}
	result, ok := results[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected result shape=%T payload=%v", results[0], payload)
	}
	if got := result["error"]; got != "user not allowed on node" {
		t.Fatalf("expected node access error, got %v payload=%v", got, payload)
	}
	if got := result["code"]; got != "user_not_allowed_on_node" {
		t.Fatalf("expected structured code, got %v payload=%v", got, payload)
	}
	if got, _ := result["permanent"].(bool); !got {
		t.Fatalf("expected permanent=true, got %v payload=%v", result["permanent"], payload)
	}

	resp = env.request(t, http.MethodGet, fmt.Sprintf("/api/v1/traffic/summary?range=1h&user_id=%d", userID), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	summary := decodeBodyMap(t, resp)
	kpi := summary["kpi"].(map[string]any)
	if totalOut := int64(kpi["total_out"].(float64)); totalOut != 0 {
		t.Fatalf("expected blocked write to keep total_out=0, got %d payload=%v", totalOut, summary)
	}
}

func TestFunctional_APIUsersCreateRejectsMissingNodeIDsWithValidationError(t *testing.T) {
	env := setupTestEnv(t)

	resp := env.request(t, http.MethodPost, "/api/v1/users", map[string]any{
		"username":         fmt.Sprintf("missing-node-%d", time.Now().UnixNano()),
		"monthly_limit_gb": 20,
		"counting_mode":    "double",
		"quota_cycle":      "day",
		"quota_tz":         "Asia/Shanghai",
		"device_limit":     2,
		"plan_days":        7,
		"node_ids":         []int64{999999, 999999},
	}, true, "")
	mustStatus(t, resp, http.StatusBadRequest)
	if body := decodeBodyText(t, resp); !strings.Contains(body, "node not found") {
		t.Fatalf("expected node validation error, got %q", body)
	}
}

func TestFunctional_NodeReportEnforcesBoundNodeID(t *testing.T) {
	env := setupTestEnv(t)

	node1 := createNodeViaBasicAuth(t, env, "n1")
	node2 := createNodeViaBasicAuth(t, env, "n2")

	// node1 mTLS client must not be able to act as node2.
	resp := env.requestMTLS(t, node1, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/report", node2), nil)
	mustStatus(t, resp, http.StatusForbidden)
	_ = resp.Body.Close()

	resp = env.requestMTLS(t, node1, http.MethodGet, fmt.Sprintf("/api/v1/nodes/%d/agent/users", node2), nil)
	mustStatus(t, resp, http.StatusForbidden)
	_ = resp.Body.Close()

	// node1 mTLS client should succeed on its own node endpoints.
	resp = env.requestMTLS(t, node1, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/report", node1), nil)
	mustStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()
}

func TestFunctional_DisabledNodeCanDrainUsersSyncButCannotReportOrWriteUsage(t *testing.T) {
	env := setupTestEnv(t)

	nodeID := createNodeViaBasicAuth(t, env, "disable-drain-node")
	userID, _ := createUserViaAPI(t, env)

	resp := env.request(t, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/disable", nodeID), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	resp = env.requestMTLS(t, nodeID, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/jobs/claim?wait=0", nodeID), nil)
	mustStatus(t, resp, http.StatusOK)
	claim := decodeBodyMap(t, resp)
	job, ok := claim["job"].(map[string]any)
	if !ok {
		t.Fatalf("expected claimed job payload, got %v", claim)
	}
	if kind, _ := job["kind"].(string); kind != "users_sync" {
		t.Fatalf("expected disabled node cleanup users_sync, got %v", claim)
	}
	jobID := int64(job["id"].(float64))
	jobAttempt := int(job["attempts"].(float64))
	appliedVersion, _ := job["desired_version"].(string)

	resp = env.requestMTLS(t, nodeID, http.MethodGet, fmt.Sprintf("/api/v1/nodes/%d/agent/users", nodeID), nil)
	mustStatus(t, resp, http.StatusOK)
	usersPayload := decodeBodyMap(t, resp)
	users, ok := usersPayload["users"].([]any)
	if !ok {
		t.Fatalf("unexpected users payload=%v", usersPayload)
	}
	if len(users) != 0 {
		t.Fatalf("disabled node should receive empty user list, got %v", usersPayload)
	}

	resp = env.requestMTLS(t, nodeID, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/jobs/%d/finish", nodeID, jobID), map[string]any{
		"status":          "succeeded",
		"retryable":       false,
		"applied_version": appliedVersion,
		"attempt":         jobAttempt,
		"result_json":     map[string]any{"ok": true},
	})
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	resp = env.requestMTLS(t, nodeID, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/report", nodeID), nil)
	mustStatus(t, resp, http.StatusForbidden)
	_ = resp.Body.Close()

	resp = env.requestMTLS(t, nodeID, http.MethodPost, "/api/v1/usage", map[string]any{
		"events": []map[string]any{{
			"user_id":         userID,
			"direction":       "outbound",
			"bytes":           32,
			"source":          "functional",
			"source_event_id": fmt.Sprintf("disabled-node-%d", time.Now().UnixNano()),
			"at":              time.Now().UTC().Format(time.RFC3339),
		}},
	})
	mustStatus(t, resp, http.StatusForbidden)
	_ = resp.Body.Close()
}

func TestFunctional_DisabledNodeClaimSkipsNonUsersSyncJobs(t *testing.T) {
	env := setupTestEnv(t)
	_ = createUserDirect(t, env)
	node := createManagedXrayNodeDirect(t, env, fmt.Sprintf("disabled-claim-%d", time.Now().UnixNano()))
	clearNodeJobs(t, env, node.ID)

	xrayJobID, enqueued, err := env.store.EnqueueNodeJob(context.Background(), node.ID, "xray_apply", "xray-v1", `{}`, 120, "xray")
	if err != nil {
		t.Fatalf("enqueue xray_apply: %v", err)
	}
	if !enqueued || xrayJobID <= 0 {
		t.Fatalf("expected xray_apply job to enqueue, id=%d enqueued=%v", xrayJobID, enqueued)
	}

	resp := env.request(t, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/disable", node.ID), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	resp = env.requestMTLS(t, node.ID, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/jobs/claim?wait=0", node.ID), nil)
	mustStatus(t, resp, http.StatusOK)
	claim := decodeBodyMap(t, resp)
	job, ok := claim["job"].(map[string]any)
	if !ok {
		t.Fatalf("expected claimed job payload, got %v", claim)
	}
	if kind, _ := job["kind"].(string); kind != "users_sync" {
		t.Fatalf("expected disabled node to skip xray_apply and claim users_sync, got %v", claim)
	}

	claimedJobID := int64(job["id"].(float64))
	claimedAttempt := int(job["attempts"].(float64))
	appliedVersion, _ := job["desired_version"].(string)
	resp = env.requestMTLS(t, node.ID, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/jobs/%d/finish", node.ID, claimedJobID), map[string]any{
		"status":          "succeeded",
		"retryable":       false,
		"applied_version": appliedVersion,
		"attempt":         claimedAttempt,
		"result_json":     map[string]any{"ok": true},
	})
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	resp = env.requestMTLS(t, node.ID, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/jobs/claim?wait=0", node.ID), nil)
	mustStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()

	xrayJob, ok, err := env.store.GetNodeJob(context.Background(), xrayJobID)
	if err != nil || !ok {
		t.Fatalf("get xray_apply job ok=%v err=%v", ok, err)
	}
	if xrayJob.Status != "pending" {
		t.Fatalf("expected xray_apply to remain pending for disabled node, got %+v", xrayJob)
	}
}

func TestFunctional_UsageAPIRejectsNodeBoundAPIKeys(t *testing.T) {
	env := setupTestEnv(t)
	user := createUserDirect(t, env)

	cases := []struct {
		name       string
		mutate     func(t *testing.T, nodeID int64)
		wantStatus int
	}{
		{
			name: "disabled",
			mutate: func(t *testing.T, nodeID int64) {
				t.Helper()
				if err := env.store.SetNodeEnabled(context.Background(), nodeID, false); err != nil {
					t.Fatalf("disable node: %v", err)
				}
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "deleted",
			mutate: func(t *testing.T, nodeID int64) {
				t.Helper()
				if err := env.store.DeleteNode(context.Background(), nodeID); err != nil {
					t.Fatalf("delete node: %v", err)
				}
			},
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nodeID := createNodeViaBasicAuth(t, env, fmt.Sprintf("api-key-node-%s-%d", tc.name, time.Now().UnixNano()))
			apiKey, _, err := env.store.CreateAPIKey(context.Background(), fmt.Sprintf("usage-key-%s", tc.name), "usage:write", &nodeID, nil)
			if err != nil {
				t.Fatalf("create api key: %v", err)
			}

			tc.mutate(t, nodeID)

			resp := env.request(t, http.MethodPost, "/api/v1/usage", map[string]any{
				"events": []map[string]any{{
					"user_id":         user.ID,
					"direction":       "outbound",
					"bytes":           64,
					"source":          "functional",
					"source_event_id": fmt.Sprintf("bound-key-%s-%d", tc.name, time.Now().UnixNano()),
					"at":              time.Now().UTC().Format(time.RFC3339),
				}},
			}, false, apiKey)
			mustStatus(t, resp, tc.wantStatus)
			_ = resp.Body.Close()
		})
	}
}

func TestFunctional_NodeReportRejectsInvalidJSON(t *testing.T) {
	env := setupTestEnv(t)
	nodeID := createNodeViaBasicAuth(t, env, "report-invalid-json")
	client := env.mtlsClientForNode(t, nodeID)

	req, err := http.NewRequest(http.MethodPost, env.mtls.URL+fmt.Sprintf("/api/v1/nodes/%d/report", nodeID), strings.NewReader(`{"metrics":`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	mustStatus(t, resp, http.StatusBadRequest)
	body := decodeBodyText(t, resp)
	if !strings.Contains(body, "invalid json") {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestFunctional_NodeDeployPageGETIsReadOnlyAndPOSTRotatesEnrollCode(t *testing.T) {
	env := setupTestEnv(t)
	nodeID := createNodeViaBasicAuth(t, env, "deploy-page-node")
	ctx := context.Background()

	if code, ok, err := env.store.GetNodeEnrollCode(ctx, nodeID); err != nil {
		t.Fatalf("get initial enroll code: %v", err)
	} else if ok || strings.TrimSpace(code.Code) != "" {
		t.Fatalf("expected no initial enroll code, got %+v", code)
	}

	resp := env.request(t, http.MethodGet, fmt.Sprintf("/nodes/%d/deploy", nodeID), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	body := decodeBodyText(t, resp)
	if !strings.Contains(body, "未生成") {
		t.Fatalf("expected empty enroll-code state, body=%q", body)
	}
	if !strings.Contains(body, "当前没有可用的 Enroll Code") {
		t.Fatalf("expected no-usable-enroll-code hint, body=%q", body)
	}

	if code, ok, err := env.store.GetNodeEnrollCode(ctx, nodeID); err != nil {
		t.Fatalf("get enroll code after GET: %v", err)
	} else if ok || strings.TrimSpace(code.Code) != "" {
		t.Fatalf("GET should not create enroll code, got %+v", code)
	}

	resp = env.requestForm(t, http.MethodPost, fmt.Sprintf("/nodes/%d/deploy", nodeID), url.Values{}, true)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusSeeOther {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("unexpected POST status got=%d body=%s", resp.StatusCode, string(b))
	}
	_ = resp.Body.Close()

	first, ok, err := env.store.GetNodeEnrollCode(ctx, nodeID)
	if err != nil {
		t.Fatalf("get enroll code after POST: %v", err)
	}
	if !ok || strings.TrimSpace(first.Code) == "" {
		t.Fatalf("expected enroll code after POST, got %+v", first)
	}

	resp = env.request(t, http.MethodGet, fmt.Sprintf("/nodes/%d/deploy", nodeID), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	body = decodeBodyText(t, resp)
	if !strings.Contains(body, first.Code) {
		t.Fatalf("expected deploy page to show enroll code, body=%q", body)
	}
	if !strings.Contains(body, "id=\"deploy-script\"") {
		t.Fatalf("expected deploy script after enroll code generation, body=%q", body)
	}

	resp = env.requestForm(t, http.MethodPost, fmt.Sprintf("/nodes/%d/deploy", nodeID), url.Values{}, true)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusSeeOther {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("unexpected rotate POST status got=%d body=%s", resp.StatusCode, string(b))
	}
	_ = resp.Body.Close()

	second, ok, err := env.store.GetNodeEnrollCode(ctx, nodeID)
	if err != nil {
		t.Fatalf("get enroll code after rotate: %v", err)
	}
	if !ok || strings.TrimSpace(second.Code) == "" {
		t.Fatalf("expected rotated enroll code, got %+v", second)
	}
	if first.Code == second.Code {
		t.Fatalf("expected POST to rotate enroll code")
	}
}

func TestFunctional_NodeDeployPageHidesExpiredOrUsedEnrollCode(t *testing.T) {
	env := setupTestEnv(t)
	nodeID := createNodeViaBasicAuth(t, env, "deploy-page-stale-code-node")
	ctx := context.Background()

	usedAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	expiresAt := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	createdAt := time.Now().UTC().Add(-3 * time.Minute).Format(time.RFC3339)
	if _, err := env.store.RawDB().ExecContext(ctx, `
INSERT INTO node_enroll_codes(node_id, code, expires_at, used_at, created_at)
VALUES (?, 'stale-enroll-code', ?, ?, ?)
ON CONFLICT(node_id) DO UPDATE SET
	code = excluded.code,
	expires_at = excluded.expires_at,
	used_at = excluded.used_at,
	created_at = excluded.created_at;
`, nodeID, expiresAt, usedAt, createdAt); err != nil {
		t.Fatalf("seed stale enroll code: %v", err)
	}

	resp := env.request(t, http.MethodGet, fmt.Sprintf("/nodes/%d/deploy", nodeID), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	body := decodeBodyText(t, resp)
	if strings.Contains(body, "stale-enroll-code") {
		t.Fatalf("stale enroll code should not be shown, body=%q", body)
	}
	if strings.Contains(body, "id=\"deploy-script\"") {
		t.Fatalf("deploy script should not be shown for stale enroll code, body=%q", body)
	}
	if !strings.Contains(body, "当前没有可用的 Enroll Code") {
		t.Fatalf("expected no-usable-enroll-code hint, body=%q", body)
	}
}

func TestFunctional_NodeEnrollInvalidCSRDoesNotConsumeCode(t *testing.T) {
	env := setupTestEnv(t)
	nodeID := createNodeViaBasicAuth(t, env, "enroll-invalid-csr-node")
	ctx := context.Background()

	code, err := env.store.CreateNodeEnrollCode(ctx, nodeID, time.Minute)
	if err != nil {
		t.Fatalf("create enroll code: %v", err)
	}

	resp := env.request(t, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/enroll", nodeID), map[string]any{
		"enroll_code": code.Code,
		"csr_pem":     "not a csr",
	}, false, "")
	mustStatus(t, resp, http.StatusBadRequest)
	_ = decodeBodyText(t, resp)

	got, ok, err := env.store.GetNodeEnrollCode(ctx, nodeID)
	if err != nil {
		t.Fatalf("get enroll code: %v", err)
	}
	if !ok {
		t.Fatalf("expected enroll code to remain")
	}
	if got.UsedAt != nil {
		t.Fatalf("enroll code was consumed after invalid CSR: %+v", *got.UsedAt)
	}
}

func TestFunctional_DeviceLimitAndSSRAdminActions(t *testing.T) {
	env := setupTestEnv(t)
	userID, _ := createUserViaAPI(t, env)

	// API PATCH: set device_limit
	resp := env.request(t, http.MethodPatch, fmt.Sprintf("/api/v1/users/%d", userID), map[string]any{"device_limit": 4}, true, "")
	mustStatus(t, resp, http.StatusOK)
	u := decodeBodyMap(t, resp)
	if got, _ := u["DeviceLimit"].(float64); int(got) != 4 {
		t.Fatalf("unexpected DeviceLimit got=%v payload=%v", u["DeviceLimit"], u)
	}

	// SSR actions should exist and redirect back to detail page.
	resp = env.requestForm(t, http.MethodPost, fmt.Sprintf("/users/%d/device_limit", userID), url.Values{"device_limit": {"3"}}, true)
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("unexpected status for device_limit SSR got=%d body=%s", resp.StatusCode, string(b))
	}
	_ = resp.Body.Close()

	resp = env.requestForm(t, http.MethodPost, fmt.Sprintf("/users/%d/quota_reset", userID), url.Values{"reason": {"functional"}}, true)
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("unexpected status for quota_reset SSR got=%d body=%s", resp.StatusCode, string(b))
	}
	_ = resp.Body.Close()

	resp = env.requestForm(t, http.MethodPost, fmt.Sprintf("/users/%d/quota_credit", userID), url.Values{"credit_gb": {"1"}}, true)
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("unexpected status for quota_credit SSR got=%d body=%s", resp.StatusCode, string(b))
	}
	_ = resp.Body.Close()

	resp = env.requestForm(t, http.MethodPost, fmt.Sprintf("/users/%d/plan_extend", userID), url.Values{"extend_days": {"2"}}, true)
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("unexpected status for plan_extend SSR got=%d body=%s", resp.StatusCode, string(b))
	}
	_ = resp.Body.Close()

	// Metrics host should include month field (even if 0 usage).
	resp = env.request(t, http.MethodGet, "/api/v1/metrics/host?range=1h", nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	m := decodeBodyMap(t, resp)
	mon, ok := m["month"].(map[string]any)
	if !ok {
		t.Fatalf("missing month in metrics payload=%v", m)
	}
	if strings.TrimSpace(fmt.Sprintf("%v", mon["month_key"])) == "" {
		t.Fatalf("missing month_key in month payload=%v", mon)
	}
}

func TestFunctional_SubscriptionAndTelegramBindEndpoints(t *testing.T) {
	env := setupTestEnv(t)
	userID, _ := createUserViaAPI(t, env)

	if _, err := env.store.RawDB().ExecContext(context.Background(), `DELETE FROM subscription_tokens WHERE user_id = ?`, userID); err != nil {
		t.Fatalf("delete subscription token: %v", err)
	}
	if _, err := env.store.RawDB().ExecContext(context.Background(), `DELETE FROM telegram_bindings WHERE user_id = ?`, userID); err != nil {
		t.Fatalf("delete telegram binding: %v", err)
	}

	resp := env.request(t, http.MethodGet, fmt.Sprintf("/api/v1/users/%d/subscription", userID), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	sub := decodeBodyMap(t, resp)
	if token, _ := sub["token"].(string); token != "" {
		t.Fatalf("subscription GET unexpectedly created token: %q", token)
	}

	resp = env.request(t, http.MethodGet, fmt.Sprintf("/api/v1/users/%d/telegram-bind", userID), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	bind := decodeBodyMap(t, resp)
	if code, _ := bind["bind_code"].(string); code != "" {
		t.Fatalf("telegram bind GET unexpectedly created bind code: %q", code)
	}

	resp = env.request(t, http.MethodPost, fmt.Sprintf("/api/v1/users/%d/subscription/rotate", userID), map[string]any{}, true, "")
	mustStatus(t, resp, http.StatusOK)
	sub = decodeBodyMap(t, resp)
	token, _ := sub["token"].(string)
	if token == "" {
		t.Fatalf("subscription rotate returned empty token")
	}
	createNodeViaBasicAuth(t, env, fmt.Sprintf("sub-node-%d", time.Now().UnixNano()))

	resp = env.request(t, http.MethodGet, "/sub/"+token+"?target=v2rayn", nil, false, "")
	mustStatus(t, resp, http.StatusOK)
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if len(bytes.TrimSpace(b)) == 0 {
		t.Fatalf("subscription response empty")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err == nil {
		if strings.Contains(string(decoded), "00000000-0000-0000-0000-000000000000") {
			t.Fatalf("subscription contains placeholder uuid: %s", string(decoded))
		}
	}

	resp = env.request(t, http.MethodPost, fmt.Sprintf("/api/v1/users/%d/telegram-bind/rotate", userID), map[string]any{}, true, "")
	mustStatus(t, resp, http.StatusOK)
	bind2 := decodeBodyMap(t, resp)
	code2, _ := bind2["bind_code"].(string)
	if code2 == "" {
		t.Fatalf("bind code rotate failed: empty code")
	}

	resp = env.request(t, http.MethodGet, fmt.Sprintf("/api/v1/users/%d/telegram-bind", userID), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	bind3 := decodeBodyMap(t, resp)
	code3, _ := bind3["bind_code"].(string)
	if code3 == "" || code3 != code2 {
		t.Fatalf("telegram bind GET returned wrong code got=%q want=%q", code3, code2)
	}
}

func TestFunctional_SubscriptionPublicEndpointFailsWithoutActiveLink(t *testing.T) {
	env := setupTestEnv(t)
	userID, _ := createUserViaAPI(t, env)

	resp := env.request(t, http.MethodPost, fmt.Sprintf("/api/v1/users/%d/subscription/rotate", userID), map[string]any{}, true, "")
	mustStatus(t, resp, http.StatusOK)
	sub := decodeBodyMap(t, resp)
	token, _ := sub["token"].(string)
	if token == "" {
		t.Fatalf("subscription rotate returned empty token")
	}
	if _, err := env.store.RawDB().ExecContext(context.Background(), `DELETE FROM proxy_links WHERE user_id = ?`, userID); err != nil {
		t.Fatalf("delete proxy links: %v", err)
	}

	resp = env.request(t, http.MethodGet, "/sub/"+token+"?target=v2rayn", nil, false, "")
	mustStatus(t, resp, http.StatusConflict)
	if body := decodeBodyText(t, resp); !strings.Contains(body, "subscription unavailable: no active link") {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestFunctional_SubscriptionPublicEndpointFailsWithoutEnabledNodes(t *testing.T) {
	env := setupTestEnv(t)
	userID, _ := createUserViaAPI(t, env)

	resp := env.request(t, http.MethodPost, fmt.Sprintf("/api/v1/users/%d/subscription/rotate", userID), map[string]any{}, true, "")
	mustStatus(t, resp, http.StatusOK)
	sub := decodeBodyMap(t, resp)
	token, _ := sub["token"].(string)
	if token == "" {
		t.Fatalf("subscription rotate returned empty token")
	}

	resp = env.request(t, http.MethodGet, "/sub/"+token+"?target=v2rayn", nil, false, "")
	mustStatus(t, resp, http.StatusServiceUnavailable)
	if body := decodeBodyText(t, resp); !strings.Contains(body, "subscription unavailable: no enabled nodes") {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestFunctional_SubscriptionPublicEndpointFailsWithoutPermittedEnabledNodes(t *testing.T) {
	env := setupTestEnv(t)
	nodeID := createNodeViaBasicAuth(t, env, fmt.Sprintf("restricted-node-%d", time.Now().UnixNano()))

	resp := env.request(t, http.MethodPost, "/api/v1/users", map[string]any{
		"username":         fmt.Sprintf("restricted-user-%d", time.Now().UnixNano()),
		"monthly_limit_gb": 20,
		"counting_mode":    "double",
		"quota_cycle":      "day",
		"quota_tz":         "Asia/Shanghai",
		"device_limit":     2,
		"plan_days":        7,
		"node_ids":         []int64{nodeID},
	}, true, "")
	mustStatus(t, resp, http.StatusCreated)
	payload := decodeBodyMap(t, resp)
	userID := extractUserID(t, payload)

	resp = env.request(t, http.MethodPost, fmt.Sprintf("/api/v1/users/%d/subscription/rotate", userID), map[string]any{}, true, "")
	mustStatus(t, resp, http.StatusOK)
	sub := decodeBodyMap(t, resp)
	token, _ := sub["token"].(string)
	if token == "" {
		t.Fatalf("subscription rotate returned empty token")
	}

	resp = env.request(t, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/disable", nodeID), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	resp = env.request(t, http.MethodGet, "/sub/"+token+"?target=v2rayn", nil, false, "")
	mustStatus(t, resp, http.StatusServiceUnavailable)
	if body := decodeBodyText(t, resp); !strings.Contains(body, "subscription unavailable: no permitted nodes enabled") {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestFunctional_SubscriptionRestrictedUserDoesNotFallbackToGlobalLink(t *testing.T) {
	env := setupTestEnv(t)
	nodeID := createNodeViaBasicAuth(t, env, fmt.Sprintf("restricted-unrenderable-node-%d", time.Now().UnixNano()))

	resp := env.request(t, http.MethodPost, "/api/v1/users", map[string]any{
		"username":         fmt.Sprintf("restricted-unrenderable-user-%d", time.Now().UnixNano()),
		"monthly_limit_gb": 20,
		"counting_mode":    "double",
		"quota_cycle":      "day",
		"quota_tz":         "Asia/Shanghai",
		"device_limit":     2,
		"plan_days":        7,
		"node_ids":         []int64{nodeID},
	}, true, "")
	mustStatus(t, resp, http.StatusCreated)
	userID := extractUserID(t, decodeBodyMap(t, resp))

	resp = env.request(t, http.MethodPost, fmt.Sprintf("/api/v1/users/%d/subscription/rotate", userID), map[string]any{}, true, "")
	mustStatus(t, resp, http.StatusOK)
	sub := decodeBodyMap(t, resp)
	token, _ := sub["token"].(string)
	if token == "" {
		t.Fatalf("subscription rotate returned empty token")
	}

	// The node is enabled but lacks REALITY pbk/sid, so it cannot render yet.
	// Restricted users must not fall back to the global/default active link.
	resp = env.request(t, http.MethodGet, "/sub/"+token+"?target=shadowrocket", nil, false, "")
	mustStatus(t, resp, http.StatusServiceUnavailable)
	if body := decodeBodyText(t, resp); !strings.Contains(body, "subscription unavailable: no permitted nodes renderable") {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestFunctional_NodeDeleteBlockedWhenItWouldWidenRestrictedUserAccess(t *testing.T) {
	env := setupTestEnv(t)
	nodeID := createNodeViaBasicAuth(t, env, fmt.Sprintf("delete-guard-node-%d", time.Now().UnixNano()))

	if _, err := env.store.CreateUser(context.Background(), repo.CreateUserInput{
		Username:       fmt.Sprintf("guarded-user-%d", time.Now().UnixNano()),
		MonthlyLimitGB: 20,
		CountingMode:   "double",
		QuotaCycle:     "day",
		QuotaTZ:        "Asia/Shanghai",
		DeviceLimit:    2,
		PlanDays:       7,
		NodeIDs:        []int64{nodeID},
	}); err != nil {
		t.Fatalf("create restricted user: %v", err)
	}

	resp := env.request(t, http.MethodDelete, fmt.Sprintf("/api/v1/nodes/%d", nodeID), nil, true, "")
	mustStatus(t, resp, http.StatusConflict)
	if body := decodeBodyText(t, resp); !strings.Contains(body, "restricted users would implicitly gain access") {
		t.Fatalf("unexpected body: %s", body)
	}
	if strings.Contains(decodeBodyText(t, env.request(t, http.MethodDelete, fmt.Sprintf("/api/v1/nodes/%d", nodeID), nil, true, "")), "guarded-user-") {
		t.Fatalf("delete response should not leak usernames")
	}

	if _, err := env.store.GetNode(context.Background(), nodeID); err != nil {
		t.Fatalf("node should still exist after blocked delete: %v", err)
	}
}

func TestFunctional_HTMXUserCreateOnlyClosesDrawerOnSuccess(t *testing.T) {
	env := setupTestEnv(t)
	username := fmt.Sprintf("htmx-user-%d", time.Now().UnixNano())
	form := url.Values{
		"username":         {username},
		"monthly_limit_gb": {"20"},
		"counting_mode":    {"double"},
		"quota_cycle":      {"day"},
		"quota_tz":         {"Asia/Shanghai"},
		"device_limit":     {"2"},
		"plan_days":        {"7"},
	}

	doHXForm := func(values url.Values) *http.Response {
		req, err := http.NewRequest(http.MethodPost, env.http.URL+"/users", strings.NewReader(values.Encode()))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.SetBasicAuth(env.cfg.AdminUser, env.cfg.AdminPass)
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		return resp
	}

	resp := doHXForm(form)
	mustStatus(t, resp, http.StatusOK)
	if trigger := resp.Header.Get("HX-Trigger"); !strings.Contains(trigger, `"user-created":true`) {
		t.Fatalf("expected user-created trigger on success, got %q", trigger)
	}
	_ = decodeBodyText(t, resp)

	resp = doHXForm(form)
	mustStatus(t, resp, http.StatusOK)
	trigger := resp.Header.Get("HX-Trigger")
	if strings.Contains(trigger, `"user-created":true`) {
		t.Fatalf("did not expect user-created trigger on validation failure, got %q", trigger)
	}
	if !strings.Contains(trigger, `"toast"`) {
		t.Fatalf("expected error toast trigger on validation failure, got %q", trigger)
	}
	_ = decodeBodyText(t, resp)
}

func TestFunctional_NodesOpsAndMiscPages(t *testing.T) {
	env := setupTestEnv(t)
	_, _ = createUserViaAPI(t, env)

	resp := env.request(t, http.MethodPost, "/api/v1/nodes", map[string]any{
		"name":      fmt.Sprintf("n-%d", time.Now().UnixNano()),
		"core_type": "singbox",
		"protocol":  "hysteria2",
		"host":      "127.0.0.1",
		"port":      8443,
		"enabled":   true,
	}, true, "")
	mustStatus(t, resp, http.StatusCreated)
	_ = decodeBodyMap(t, resp)

	resp = env.request(t, http.MethodGet, "/api/v1/nodes", nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	resp = env.request(t, http.MethodGet, "/api/v1/metrics/host", nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	resp = env.request(t, http.MethodGet, "/api/v1/online-users", nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	// SSR /users should render.
	resp = env.request(t, http.MethodGet, "/users", nil, true, "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("unexpected status for /users got=%d body=%s", resp.StatusCode, string(b))
	}
	_ = resp.Body.Close()
}

func TestFunctional_UserDisableEnqueuesManagedRestartAndUsersSync(t *testing.T) {
	env := setupTestEnv(t)
	user := createUserDirect(t, env)
	node := createManagedXrayNodeDirect(t, env, fmt.Sprintf("managed-%d", time.Now().UnixNano()))
	clearNodeJobs(t, env, node.ID)

	status := "disabled"
	resp := env.request(t, http.MethodPatch, fmt.Sprintf("/api/v1/users/%d", user.ID), map[string]any{"status": status}, true, "")
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	jobs, err := env.store.ListNodeJobs(context.Background(), node.ID, 10)
	if err != nil {
		t.Fatalf("list node jobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 node jobs, got %d: %+v", len(jobs), jobs)
	}
	if jobs[0].Kind != "users_sync" || jobs[1].Kind != "xray_apply" {
		t.Fatalf("expected users_sync after xray_apply, got %+v", jobs)
	}
}

func TestFunctional_UserEnableOnlyEnqueuesUsersSync(t *testing.T) {
	env := setupTestEnv(t)
	user := createUserDirect(t, env)
	node := createManagedXrayNodeDirect(t, env, fmt.Sprintf("managed-%d", time.Now().UnixNano()))

	if _, err := env.store.SetUserStatus(context.Background(), user.ID, "disabled"); err != nil {
		t.Fatalf("disable user setup: %v", err)
	}
	clearNodeJobs(t, env, node.ID)

	status := "active"
	resp := env.request(t, http.MethodPatch, fmt.Sprintf("/api/v1/users/%d", user.ID), map[string]any{"status": status}, true, "")
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	jobs, err := env.store.ListNodeJobs(context.Background(), node.ID, 10)
	if err != nil {
		t.Fatalf("list node jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 node job, got %d: %+v", len(jobs), jobs)
	}
	if jobs[0].Kind != "users_sync" {
		t.Fatalf("expected users_sync only, got %+v", jobs[0])
	}
}

func TestFunctional_UserDeleteEnqueuesManagedRestartAndUsersSync(t *testing.T) {
	env := setupTestEnv(t)
	user := createUserDirect(t, env)
	node := createManagedXrayNodeDirect(t, env, fmt.Sprintf("managed-%d", time.Now().UnixNano()))
	clearNodeJobs(t, env, node.ID)

	resp := env.request(t, http.MethodDelete, fmt.Sprintf("/api/v1/users/%d", user.ID), nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	jobs, err := env.store.ListNodeJobs(context.Background(), node.ID, 10)
	if err != nil {
		t.Fatalf("list node jobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 node jobs, got %d: %+v", len(jobs), jobs)
	}
	if jobs[0].Kind != "users_sync" || jobs[1].Kind != "xray_apply" {
		t.Fatalf("expected users_sync after xray_apply, got %+v", jobs)
	}
}

func TestFunctional_XrayApplyFinishEnqueuesUsersSync(t *testing.T) {
	env := setupTestEnv(t)
	_ = createUserDirect(t, env)
	node := createManagedXrayNodeDirect(t, env, fmt.Sprintf("managed-%d", time.Now().UnixNano()))
	clearNodeJobs(t, env, node.ID)

	jobID, enqueued, err := env.store.EnqueueNodeJob(context.Background(), node.ID, "xray_apply", "ver-1", `{}`, 120, "test")
	if err != nil {
		t.Fatalf("enqueue xray_apply: %v", err)
	}
	if !enqueued || jobID <= 0 {
		t.Fatalf("expected xray_apply to be enqueued, jobID=%d enqueued=%v", jobID, enqueued)
	}
	claimed, ok, err := env.store.ClaimNextNodeJobForNode(context.Background(), node.ID)
	if err != nil || !ok {
		t.Fatalf("claim xray_apply: ok=%v err=%v", ok, err)
	}

	resp := env.requestMTLS(t, node.ID, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/jobs/%d/finish", node.ID, jobID), map[string]any{
		"status":          "succeeded",
		"retryable":       false,
		"applied_version": "ver-1",
		"attempt":         claimed.Attempts,
		"result_json":     map[string]any{"ok": true},
	})
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	jobs, err := env.store.ListNodeJobs(context.Background(), node.ID, 10)
	if err != nil {
		t.Fatalf("list node jobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected xray_apply + users_sync jobs, got %d: %+v", len(jobs), jobs)
	}
	if jobs[0].Kind != "users_sync" || jobs[0].Status != "pending" {
		t.Fatalf("expected pending users_sync after xray_apply finish, got %+v", jobs[0])
	}
	if jobs[1].Kind != "xray_apply" || jobs[1].Status != "succeeded" {
		t.Fatalf("expected succeeded xray_apply job, got %+v", jobs[1])
	}
}

func TestFunctional_APIUsersFreshensExpiredStatus(t *testing.T) {
	env := setupTestEnv(t)
	user := createUserDirect(t, env)
	expireUserDirect(t, env, user.ID)

	resp := env.request(t, http.MethodGet, "/api/v1/users", nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	body := decodeBodyMap(t, resp)

	items, ok := body["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("expected users in response, got %+v", body)
	}

	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("expected first user map, got %#v", items[0])
	}
	if got := first["Status"]; got != "expired" {
		t.Fatalf("expected expired status after fresh list, got %#v", got)
	}
}

func TestFunctional_UsageSweepsExpiredUsersBeforeIngest(t *testing.T) {
	env := setupTestEnv(t)
	user := createUserDirect(t, env)
	nodeID := createNodeViaBasicAuth(t, env, fmt.Sprintf("usage-expired-node-%d", time.Now().UnixNano()))

	expiredAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := env.store.RawDB().ExecContext(context.Background(), `
UPDATE users
SET expires_at = ?, status = 'active', removed_at = NULL
WHERE id = ?;
`, expiredAt, user.ID); err != nil {
		t.Fatalf("expire user: %v", err)
	}

	resp := env.requestMTLS(t, nodeID, http.MethodPost, "/api/v1/usage", map[string]any{
		"events": []map[string]any{{
			"user_id":         user.ID,
			"direction":       "outbound",
			"bytes":           16,
			"source":          "functional",
			"source_event_id": fmt.Sprintf("expired-usage-%d", time.Now().UnixNano()),
			"at":              time.Now().UTC().Format(time.RFC3339),
		}},
	})
	mustStatus(t, resp, http.StatusOK)
	body := decodeBodyMap(t, resp)
	results := body["results"].([]any)
	result := results[0].(map[string]any)
	if got := result["error"]; got != "user not active" {
		t.Fatalf("expected expired user rejection, got %v payload=%v", got, body)
	}

	after, err := env.store.GetUser(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("get user after usage sweep: %v", err)
	}
	if after.Status != "expired" {
		t.Fatalf("expected expired status after usage sweep, got %s", after.Status)
	}
	if after.ActiveLink != nil {
		t.Fatalf("expected active link to be deactivated after usage sweep")
	}
}

func TestFunctional_UsageSweepExpiredUsersEnqueuesUsersSync(t *testing.T) {
	env := setupTestEnv(t)
	user := createUserDirect(t, env)
	nodeID := createNodeViaBasicAuth(t, env, fmt.Sprintf("usage-sweep-node-%d", time.Now().UnixNano()))
	clearNodeJobs(t, env, nodeID)

	expireUserDirect(t, env, user.ID)

	resp := env.requestMTLS(t, nodeID, http.MethodPost, "/api/v1/usage", map[string]any{
		"events": []map[string]any{{
			"user_id":         user.ID,
			"direction":       "outbound",
			"bytes":           16,
			"source":          "functional",
			"source_event_id": fmt.Sprintf("expired-sync-%d", time.Now().UnixNano()),
			"at":              time.Now().UTC().Format(time.RFC3339),
		}},
	})
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	jobs, err := env.store.ListNodeJobs(context.Background(), nodeID, 10)
	if err != nil {
		t.Fatalf("list node jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 node job, got %d: %+v", len(jobs), jobs)
	}
	if jobs[0].Kind != "users_sync" || jobs[0].Status != "pending" {
		t.Fatalf("expected pending users_sync after expiry sweep, got %+v", jobs[0])
	}
}

func TestFunctional_UsageSweepsQuotaWindowsBeforeIngest(t *testing.T) {
	env := setupTestEnv(t)
	user := createUserDirect(t, env)
	nodeID := createNodeViaBasicAuth(t, env, fmt.Sprintf("usage-quota-node-%d", time.Now().UnixNano()))

	now := time.Now().UTC()
	if _, err := env.store.RawDB().ExecContext(context.Background(), `
UPDATE users
SET status = 'over_limit', removed_at = ?
WHERE id = ?;
`, now.Format(time.RFC3339), user.ID); err != nil {
		t.Fatalf("mark user over_limit: %v", err)
	}
	if _, err := env.store.RawDB().ExecContext(context.Background(), `
UPDATE proxy_links
SET active = 0
WHERE user_id = ?;
`, user.ID); err != nil {
		t.Fatalf("deactivate proxy links: %v", err)
	}
	if _, err := env.store.RawDB().ExecContext(context.Background(), `
UPDATE quota_windows
SET window_start = ?, window_end = ?, closed_at = ?
WHERE user_id = ?;
`, now.Add(-48*time.Hour).Format(time.RFC3339), now.Add(-24*time.Hour).Format(time.RFC3339), now.Add(-24*time.Hour).Format(time.RFC3339), user.ID); err != nil {
		t.Fatalf("backdate quota window: %v", err)
	}

	resp := env.requestMTLS(t, nodeID, http.MethodPost, "/api/v1/usage", map[string]any{
		"events": []map[string]any{{
			"user_id":         user.ID,
			"direction":       "outbound",
			"bytes":           24,
			"source":          "functional",
			"source_event_id": fmt.Sprintf("quota-cycle-%d", time.Now().UnixNano()),
			"at":              now.Format(time.RFC3339),
		}},
	})
	mustStatus(t, resp, http.StatusOK)
	body := decodeBodyMap(t, resp)
	results := body["results"].([]any)
	result := results[0].(map[string]any)
	if got := result["status"]; got != "active" {
		t.Fatalf("expected quota sweep to reactivate user, got %v payload=%v", got, body)
	}

	after, err := env.store.GetUser(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("get user after quota sweep: %v", err)
	}
	if after.Status != "active" {
		t.Fatalf("expected active status after quota sweep, got %s", after.Status)
	}
	if after.ActiveLink == nil {
		t.Fatalf("expected active link to be restored after quota sweep")
	}
	if after.WindowEffective == 0 {
		t.Fatalf("expected usage to be recorded in new quota window, got %+v", after)
	}
}
