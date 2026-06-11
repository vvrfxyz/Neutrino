package agent

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
	"strings"
	"time"
)

func ensureEnrolled(cfg Config) error {
	// If cert+key+ca already exist, do nothing.
	if fileExists(cfg.PanelMTLSClientCertPath) && fileExists(cfg.PanelMTLSClientKeyPath) && fileExists(cfg.PanelMTLSCACertPath) {
		return nil
	}
	if strings.TrimSpace(cfg.PanelURL) == "" {
		return fmt.Errorf("PANEL_URL is required for initial enroll")
	}
	if strings.TrimSpace(cfg.EnrollCode) == "" {
		return fmt.Errorf("ENROLL_CODE is required for initial enroll")
	}

	key, csrPEM, keyPEM, err := generateKeyAndCSR(cfg.NodeID)
	if err != nil {
		return err
	}
	_ = key // only used for sanity

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := Enroll(ctx, cfg.PanelURL, cfg.NodeID, cfg.EnrollCode, csrPEM)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(cfg.PanelMTLSClientCertPath), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.PanelMTLSClientKeyPath), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.PanelMTLSCACertPath), 0700); err != nil {
		return err
	}

	if err := writeFileAtomic(cfg.PanelMTLSClientKeyPath, []byte(keyPEM), 0600); err != nil {
		return err
	}
	if err := writeFileAtomic(cfg.PanelMTLSClientCertPath, []byte(resp.CertPEM), 0644); err != nil {
		return err
	}
	if err := writeFileAtomic(cfg.PanelMTLSCACertPath, []byte(resp.CABundlePEM), 0644); err != nil {
		return err
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func generateKeyAndCSR(nodeID int64) (*ecdsa.PrivateKey, string, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", "", err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkixNameForNode(nodeID),
	}, key)
	if err != nil {
		return nil, "", "", err
	}
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, "", "", err
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return key, csrPEM, keyPEM, nil
}

func pkixNameForNode(nodeID int64) pkix.Name {
	return pkix.Name{CommonName: fmt.Sprintf("node-%d", nodeID)}
}
