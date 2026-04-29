package repo

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"neutrino/internal/certutil"
)

func TestRenewNodeCertPinRotatesAllowlist(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "cert-renew-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	caCert, caKey := mustRepoTestCA(t)
	oldCert := mustRepoNodeCert(t, caCert, caKey, node.ID)
	newCert := mustRepoNodeCert(t, caCert, caKey, node.ID)

	if err := s.UpsertNodeCertPin(ctx, node.ID, "agent_client", oldCert, time.Now().UTC()); err != nil {
		t.Fatalf("upsert old cert pin: %v", err)
	}
	if err := s.ValidateNodeMTLSCert(ctx, node.ID, oldCert, time.Now().UTC()); err != nil {
		t.Fatalf("old cert should validate before renew: %v", err)
	}

	revoked, err := s.RenewNodeCertPin(ctx, node.ID, "agent_client", newCert, time.Now().UTC())
	if err != nil {
		t.Fatalf("renew cert pin: %v", err)
	}
	if revoked != 1 {
		t.Fatalf("revoked=%d, want 1", revoked)
	}
	if err := s.ValidateNodeMTLSCert(ctx, node.ID, oldCert, time.Now().UTC()); err == nil {
		t.Fatalf("old cert should be revoked after renew")
	}
	if err := s.ValidateNodeMTLSCert(ctx, node.ID, newCert, time.Now().UTC()); err != nil {
		t.Fatalf("new cert should validate after renew: %v", err)
	}
}

func mustRepoTestCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ca key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate ca serial: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "repo-test-ca"},
		NotBefore:             time.Now().UTC().Add(-time.Hour),
		NotAfter:              time.Now().UTC().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca cert: %v", err)
	}
	return cert, key
}

func mustRepoNodeCert(t *testing.T, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, nodeID int64) *x509.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate node key: %v", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "node-" + big.NewInt(nodeID).String()},
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
		NotBefore:   time.Now().UTC().Add(-5 * time.Minute),
		NotAfter:    time.Now().UTC().Add(24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		KeyUsage:    x509.KeyUsageDigitalSignature,
	})
	if err != nil {
		t.Fatalf("sign node cert: %v", err)
	}
	return cert
}
