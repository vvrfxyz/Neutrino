package functional_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"neutrino/internal/agent"
	"neutrino/internal/app"
	"neutrino/internal/certutil"
	"neutrino/internal/config"
	"neutrino/internal/db"
	"neutrino/internal/repo"
)

func TestFunctional_AgentCertRenewHotSwap(t *testing.T) {
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

	// CA used for: mTLS handshake + panel renew signing.
	caCert, caKeyAny, caPool := mustTestCA(t)
	caKey, ok := caKeyAny.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("unexpected ca key type %T", caKeyAny)
	}
	caCertPath := filepath.Join(tmp, "issuing-ca.crt")
	caKeyPath := filepath.Join(tmp, "issuing-ca.key")
	if err := os.WriteFile(caCertPath, []byte(certutil.CertToPEM(caCert)), 0644); err != nil {
		t.Fatalf("write ca cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		t.Fatalf("marshal ca key: %v", err)
	}
	if err := os.WriteFile(caKeyPath, []byte(pemEncode("EC PRIVATE KEY", keyDER)), 0600); err != nil {
		t.Fatalf("write ca key: %v", err)
	}

	cfg := config.Load()
	cfg.DBPath = dbPath
	cfg.AllowBasicAuth = true
	cfg.TelegramBotToken = ""
	cfg.PanelAgentMTLSSigningCACertPath = caCertPath
	cfg.PanelAgentMTLSSigningCAKeyPath = caKeyPath
	cfg.PanelAgentMTLSCACertPaths = caCertPath
	cfg.PanelAgentMTLSSigningRequireIntermediate = false
	cfg.PanelAgentMTLSSigningRequireStrictKeyPerms = true

	store := repo.New(conn, cfg)
	if err := store.UpsertAdminCredential(context.Background(), cfg.AdminUser, cfg.AdminPass); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}
	a := app.New(cfg, store)

	mtlsSrv := newMTLSTestServer(t, a.AgentRoutes(), caCert, caKey, caPool)

	// Create a node (required by mTLS auth middleware).
	n, err := store.CreateNode(context.Background(), repo.CreateNodeInput{
		Name:      "n1",
		CoreType:  "xray",
		Protocol:  "vless_reality",
		Host:      "example.com",
		Port:      443,
		Enabled:   true,
		Managed:   false,
		AgentURL:  "",
		Transport: "",
		Security:  "",
		Flow:      "",
		SNI:       "",
		FP:        "",
		PublicKey: "",
		ShortID:   "",
		ExtraJSON: "",
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	nodeID := n.ID

	// Prepare an initial short-lived node client cert and allowlist it.
	oldKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen node key: %v", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: fmt.Sprintf("node-%d", nodeID)},
	}, oldKey)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		t.Fatalf("parse csr: %v", err)
	}
	oldCert, err := certutil.SignCSR(certutil.SignCSRInput{
		CACert:      caCert,
		CAKey:       caKey,
		CSR:         csr,
		NotBefore:   time.Now().UTC().Add(-5 * time.Minute),
		NotAfter:    time.Now().UTC().Add(2 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		KeyUsage:    x509.KeyUsageDigitalSignature,
	})
	if err != nil {
		t.Fatalf("sign old cert: %v", err)
	}
	if err := store.UpsertNodeCertPin(context.Background(), nodeID, "agent_client", oldCert, time.Now().UTC()); err != nil {
		t.Fatalf("allowlist old cert: %v", err)
	}

	mtlsDir := filepath.Join(tmp, "mtls")
	if err := os.MkdirAll(mtlsDir, 0700); err != nil {
		t.Fatalf("mkdir mtls: %v", err)
	}
	oldCertPath := filepath.Join(mtlsDir, "node.crt")
	oldKeyPath := filepath.Join(mtlsDir, "node.key")
	oldCAPath := filepath.Join(mtlsDir, "ca-bundle.crt")
	if err := os.WriteFile(oldCertPath, []byte(certutil.CertToPEM(oldCert)), 0644); err != nil {
		t.Fatalf("write old cert: %v", err)
	}
	oldKeyDER, err := x509.MarshalECPrivateKey(oldKey)
	if err != nil {
		t.Fatalf("marshal old key: %v", err)
	}
	if err := os.WriteFile(oldKeyPath, []byte(pemEncode("EC PRIVATE KEY", oldKeyDER)), 0600); err != nil {
		t.Fatalf("write old key: %v", err)
	}
	if err := os.WriteFile(oldCAPath, []byte(certutil.CertToPEM(caCert)), 0644); err != nil {
		t.Fatalf("write ca bundle: %v", err)
	}

	// Keep a copy of the old files at different paths so we can prove they become forbidden.
	oldCopyDir := filepath.Join(tmp, "oldcopy")
	if err := os.MkdirAll(oldCopyDir, 0700); err != nil {
		t.Fatalf("mkdir old copy: %v", err)
	}
	oldCopyCertPath := filepath.Join(oldCopyDir, "node.crt")
	oldCopyKeyPath := filepath.Join(oldCopyDir, "node.key")
	oldCopyCAPath := filepath.Join(oldCopyDir, "ca-bundle.crt")
	_ = os.WriteFile(oldCopyCertPath, []byte(certutil.CertToPEM(oldCert)), 0644)
	_ = os.WriteFile(oldCopyKeyPath, []byte(pemEncode("EC PRIVATE KEY", oldKeyDER)), 0600)
	_ = os.WriteFile(oldCopyCAPath, []byte(certutil.CertToPEM(caCert)), 0644)

	ag, err := agent.New(agent.Config{
		NodeID:                   nodeID,
		PanelMTLSURL:             mtlsSrv.URL,
		PanelMTLSCACertPath:      oldCAPath,
		PanelMTLSClientCertPath:  oldCertPath,
		PanelMTLSClientKeyPath:   oldKeyPath,
		CertRenewEnabled:         true,
		CertRenewBeforeDays:      7,
		CertRenewCheckEveryHours: 12,
		CertRenewMinRetrySec:     1,
		CertRenewMaxRetrySec:     10,
		// State/queue in temp dir to keep the agent happy.
		StatePath: filepath.Join(tmp, "agent", "state.json"),
		QueueDir:  filepath.Join(tmp, "agent", "queue"),
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ag.Run(ctx) }()

	// Wait until cert is renewed (panel renew issues a 30-day cert).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for cert renew")
		}
		na, _, err := agentLoadCertNotAfter(oldCertPath)
		if err == nil && na.After(time.Now().UTC().Add(20*24*time.Hour)) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Old cert (stored separately) should now be forbidden due to tight allowlist rotation.
	oldPC, err := agent.NewPanelClient(mtlsSrv.URL, nodeID, oldCopyCAPath, oldCopyCertPath, oldCopyKeyPath)
	if err != nil {
		t.Fatalf("new old panel client: %v", err)
	}
	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()
	if err := oldPC.Heartbeat(reqCtx); err == nil {
		t.Fatalf("expected old client heartbeat to fail after renew rotation")
	}

	// New cert should succeed.
	newPC, err := agent.NewPanelClient(mtlsSrv.URL, nodeID, oldCAPath, oldCertPath, oldKeyPath)
	if err != nil {
		t.Fatalf("new new panel client: %v", err)
	}
	reqCtx2, reqCancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel2()
	if err := newPC.Heartbeat(reqCtx2); err != nil {
		t.Fatalf("expected new client heartbeat to succeed: %v", err)
	}
}

func agentLoadCertNotAfter(certPath string) (time.Time, string, error) {
	raw, err := os.ReadFile(certPath)
	if err != nil {
		return time.Time{}, "", err
	}
	for {
		var b *pem.Block
		b, raw = pem.Decode(raw)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return time.Time{}, "", err
		}
		return c.NotAfter.UTC(), c.Subject.CommonName, nil
	}
	return time.Time{}, "", fmt.Errorf("no cert")
}
