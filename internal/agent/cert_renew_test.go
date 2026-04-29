package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestShouldRenew_BeforeWindow(t *testing.T) {
	now := time.Date(2026, 2, 8, 0, 0, 0, 0, time.UTC)

	if shouldRenew(now, now.Add(8*24*time.Hour), 7) {
		t.Fatalf("expected false when not_after is beyond window")
	}
	if !shouldRenew(now, now.Add(7*24*time.Hour), 7) {
		t.Fatalf("expected true on boundary")
	}
	if !shouldRenew(now, now.Add(6*24*time.Hour), 7) {
		t.Fatalf("expected true within window")
	}
	if !shouldRenew(now, now.Add(-1*time.Hour), 7) {
		t.Fatalf("expected true when already expired")
	}
}

func TestLoadClientCertNotAfter_ParsePEM(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "node.crt")

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	now := time.Now().UTC()
	notAfter := now.Add(10 * time.Hour).UTC().Truncate(time.Second)
	tpl := &x509.Certificate{
		SerialNumber:          mustSerial(t),
		Subject:               pkix.Name{CommonName: "node-1"},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		t.Fatalf("write cert: %v", err)
	}

	na, cn, err := loadClientCertNotAfter(certPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cn != "node-1" {
		t.Fatalf("unexpected cn: %q", cn)
	}
	// Allow for truncation differences; compare in seconds.
	if na.UTC().Truncate(time.Second) != notAfter.UTC().Truncate(time.Second) {
		t.Fatalf("unexpected not_after got=%s want=%s", na.UTC().Format(time.RFC3339), notAfter.UTC().Format(time.RFC3339))
	}
}

func mustSerial(t *testing.T) *big.Int {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("gen serial: %v", err)
	}
	return serial
}
