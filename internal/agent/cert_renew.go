package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func loadClientCertNotAfter(certPath string) (notAfter time.Time, cn string, err error) {
	certPath = strings.TrimSpace(certPath)
	if certPath == "" {
		return time.Time{}, "", fmt.Errorf("empty cert path")
	}
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
		cert, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return time.Time{}, "", err
		}
		return cert.NotAfter.UTC(), strings.TrimSpace(cert.Subject.CommonName), nil
	}
	return time.Time{}, "", fmt.Errorf("no certificate block found")
}

func shouldRenew(now time.Time, notAfter time.Time, beforeDays int) bool {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	notAfter = notAfter.UTC()
	if beforeDays <= 0 {
		beforeDays = 7
	}
	threshold := now.Add(time.Duration(beforeDays) * 24 * time.Hour)
	// Renew when cert expires on or before the threshold (inclusive).
	return !notAfter.After(threshold)
}

func (a *Agent) startCertRenewer(ctx context.Context) {
	if a == nil || !a.cfg.CertRenewEnabled {
		return
	}

	checkEvery := time.Duration(a.cfg.CertRenewCheckEveryHours) * time.Hour
	if checkEvery <= 0 {
		checkEvery = 12 * time.Hour
	}
	minRetry := time.Duration(a.cfg.CertRenewMinRetrySec) * time.Second
	if minRetry <= 0 {
		minRetry = 30 * time.Second
	}
	maxRetry := time.Duration(a.cfg.CertRenewMaxRetrySec) * time.Second
	if maxRetry <= 0 {
		maxRetry = 30 * time.Minute
	}
	if maxRetry < minRetry {
		maxRetry = minRetry
	}

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	backoff := minRetry

	// Run once immediately on startup to avoid missing renew windows.
	for {
		if ctx.Err() != nil {
			return
		}
		if err := a.renewCertOnce(ctx); err != nil {
			log.Printf("cert renew check failed: %v", err)
			sleep := jitter(r, backoff, 0.2)
			select {
			case <-ctx.Done():
				return
			case <-time.After(sleep):
			}
			backoff *= 2
			if backoff > maxRetry {
				backoff = maxRetry
			}
			continue
		}
		backoff = minRetry
		select {
		case <-ctx.Done():
			return
		case <-time.After(checkEvery):
		}
	}
}

func jitter(r *rand.Rand, d time.Duration, frac float64) time.Duration {
	if d <= 0 || frac <= 0 || r == nil {
		return d
	}
	if frac > 0.9 {
		frac = 0.9
	}
	maxExtra := int64(float64(d) * frac)
	if maxExtra <= 0 {
		return d
	}
	extra := time.Duration(r.Int63n(maxExtra + 1))
	return d + extra
}

func (a *Agent) renewCertOnce(ctx context.Context) error {
	if a == nil || !a.cfg.CertRenewEnabled {
		return nil
	}

	now := time.Now().UTC()
	wantCN := fmt.Sprintf("node-%d", a.cfg.NodeID)

	notAfter, cn, err := loadClientCertNotAfter(a.cfg.PanelMTLSClientCertPath)
	if err != nil {
		// Attempt bootstrap enroll if cert/key/ca are missing.
		if err2 := ensureEnrolled(a.cfg); err2 != nil {
			return fmt.Errorf("load current cert failed: %w (bootstrap enroll failed: %v)", err, err2)
		}
		notAfter, cn, err = loadClientCertNotAfter(a.cfg.PanelMTLSClientCertPath)
		if err != nil {
			return fmt.Errorf("load current cert failed after bootstrap: %w", err)
		}
	}

	if cn != "" && cn != wantCN {
		log.Printf("warning: node cert CN mismatch: got=%q want=%q", cn, wantCN)
	}

	beforeDays := a.cfg.CertRenewBeforeDays
	if beforeDays <= 0 {
		beforeDays = 7
	}
	if !shouldRenew(now, notAfter, beforeDays) {
		log.Printf("cert renew not needed node_id=%d not_after=%s renew_before_days=%d", a.cfg.NodeID, notAfter.UTC().Format(time.RFC3339), beforeDays)
		return nil
	}

	_, csrPEM, keyPEM, err := generateKeyAndCSR(a.cfg.NodeID)
	if err != nil {
		return err
	}

	pc := a.panelClient()
	if pc == nil {
		return fmt.Errorf("panel client not ready")
	}
	renewCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := pc.RenewCert(renewCtx, csrPEM)
	if err != nil {
		// If the current cert is expired, mTLS renew may be impossible. Optionally fall back to enroll.
		if now.After(notAfter) {
			if strings.TrimSpace(a.cfg.PanelURL) != "" && strings.TrimSpace(a.cfg.EnrollCode) != "" {
				log.Printf("cert renew failed and current cert expired at %s, attempting re-enroll", notAfter.Format(time.RFC3339))
				if err2 := a.reenrollOnce(ctx); err2 == nil {
					return nil
				} else {
					return fmt.Errorf("renew failed: %w; re-enroll failed: %v", err, err2)
				}
			}
			return fmt.Errorf("renew failed: %w (current cert expired at %s; set PANEL_URL and ENROLL_CODE to re-enroll)", err, notAfter.Format(time.RFC3339))
		}
		return err
	}

	newCert, err := parseFirstCert(resp.CertPEM)
	if err != nil {
		return fmt.Errorf("invalid renewed cert: %w", err)
	}
	if strings.TrimSpace(newCert.Subject.CommonName) != wantCN {
		return fmt.Errorf("renewed cert CN mismatch: got=%q want=%q", strings.TrimSpace(newCert.Subject.CommonName), wantCN)
	}
	if !newCert.NotAfter.UTC().After(now.Add(24 * time.Hour)) {
		return fmt.Errorf("renewed cert not_after too soon: %s", newCert.NotAfter.UTC().Format(time.RFC3339))
	}
	if _, err := tls.X509KeyPair([]byte(resp.CertPEM), []byte(keyPEM)); err != nil {
		return fmt.Errorf("renewed cert/key mismatch: %w", err)
	}
	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM([]byte(resp.CABundlePEM)); !ok {
		return fmt.Errorf("invalid renewed ca bundle pem")
	}

	oldNotAfter := notAfter
	newPC, err := a.installMTLSFilesAndReloadPanelClient([]byte(keyPEM), []byte(resp.CertPEM), []byte(resp.CABundlePEM))
	if err != nil {
		return err
	}
	a.setPanelClient(newPC)

	log.Printf("cert renew succeeded node_id=%d old_not_after=%s new_not_after=%s",
		a.cfg.NodeID,
		oldNotAfter.UTC().Format(time.RFC3339),
		newCert.NotAfter.UTC().Format(time.RFC3339),
	)
	return nil
}

func (a *Agent) reenrollOnce(ctx context.Context) error {
	if strings.TrimSpace(a.cfg.PanelURL) == "" {
		return fmt.Errorf("PANEL_URL is required for re-enroll")
	}
	if strings.TrimSpace(a.cfg.EnrollCode) == "" {
		return fmt.Errorf("ENROLL_CODE is required for re-enroll")
	}

	_, csrPEM, keyPEM, err := generateKeyAndCSR(a.cfg.NodeID)
	if err != nil {
		return err
	}
	enrollCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := Enroll(enrollCtx, a.cfg.PanelURL, a.cfg.NodeID, a.cfg.EnrollCode, csrPEM)
	if err != nil {
		return err
	}

	newCert, err := parseFirstCert(resp.CertPEM)
	if err != nil {
		return fmt.Errorf("invalid enroll cert: %w", err)
	}
	wantCN := fmt.Sprintf("node-%d", a.cfg.NodeID)
	if strings.TrimSpace(newCert.Subject.CommonName) != wantCN {
		return fmt.Errorf("enroll cert CN mismatch: got=%q want=%q", strings.TrimSpace(newCert.Subject.CommonName), wantCN)
	}
	if _, err := tls.X509KeyPair([]byte(resp.CertPEM), []byte(keyPEM)); err != nil {
		return fmt.Errorf("enroll cert/key mismatch: %w", err)
	}
	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM([]byte(resp.CABundlePEM)); !ok {
		return fmt.Errorf("invalid enroll ca bundle pem")
	}

	newPC, err := a.installMTLSFilesAndReloadPanelClient([]byte(keyPEM), []byte(resp.CertPEM), []byte(resp.CABundlePEM))
	if err != nil {
		return err
	}
	a.setPanelClient(newPC)

	log.Printf("re-enroll succeeded node_id=%d not_after=%s", a.cfg.NodeID, newCert.NotAfter.UTC().Format(time.RFC3339))
	return nil
}

func parseFirstCert(pemStr string) (*x509.Certificate, error) {
	raw := []byte(strings.TrimSpace(pemStr))
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty pem")
	}
	for {
		b, rest := pem.Decode(raw)
		if b == nil {
			break
		}
		raw = rest
		if b.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	return nil, fmt.Errorf("no certificate block found")
}

type mtlsFileUpdate struct {
	path string
	perm os.FileMode
	body []byte
}

// installMTLSFiles atomically replaces the given files on disk using
// tmp-write + backup-rename + install-rename. On failure it rolls back any
// partial changes itself and returns the error. On success it returns a
// rollback function the caller can invoke to restore the previous files if a
// later step fails; backups (".bak.<ts>") are kept on disk for diagnostics.
func installMTLSFiles(files []mtlsFileUpdate) (rollback func(), err error) {
	ts := time.Now().UTC().Format("20060102T150405Z")
	type upd struct {
		mtlsFileUpdate
		tmp       string
		bak       string
		had       bool // original existed and was moved to bak
		installed bool // new file was renamed from tmp into place at path
	}
	updates := make([]upd, 0, len(files))
	for _, f := range files {
		updates = append(updates, upd{mtlsFileUpdate: f})
	}

	rollback = func() {
		for _, u := range updates {
			if u.tmp != "" {
				_ = os.Remove(u.tmp)
			}
		}
		// Undo installs and restore backups. Never touch u.path unless the
		// new file was installed there: otherwise the original file (or
		// nothing) is still sitting at u.path.
		for _, u := range updates {
			if u.installed {
				_ = os.Remove(u.path)
			}
			if u.had {
				_ = os.Rename(u.bak, u.path)
			}
		}
	}

	// 1) Write temp files first.
	for i := range updates {
		u := &updates[i]
		if err := os.MkdirAll(filepath.Dir(u.path), 0700); err != nil {
			rollback()
			return nil, err
		}
		u.tmp = u.path + ".tmp." + ts
		if err := os.WriteFile(u.tmp, u.body, u.perm); err != nil {
			rollback()
			return nil, err
		}
	}

	// 2) Move current files to backups to support cross-platform atomic replacement.
	for i := range updates {
		u := &updates[i]
		if _, err := os.Stat(u.path); err == nil {
			u.bak = u.path + ".bak." + ts
			if err := os.Rename(u.path, u.bak); err != nil {
				rollback()
				return nil, err
			}
			u.had = true
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			rollback()
			return nil, err
		}
	}

	// 3) Install temps.
	for i := range updates {
		u := &updates[i]
		if err := os.Rename(u.tmp, u.path); err != nil {
			rollback()
			return nil, err
		}
		u.tmp = ""
		u.installed = true
	}

	return rollback, nil
}

func (a *Agent) installMTLSFilesAndReloadPanelClient(keyPEM, certPEM, caPEM []byte) (*PanelClient, error) {
	if a == nil {
		return nil, fmt.Errorf("nil agent")
	}

	keyPath := filepath.Clean(strings.TrimSpace(a.cfg.PanelMTLSClientKeyPath))
	certPath := filepath.Clean(strings.TrimSpace(a.cfg.PanelMTLSClientCertPath))
	caPath := filepath.Clean(strings.TrimSpace(a.cfg.PanelMTLSCACertPath))
	if keyPath == "" || certPath == "" || caPath == "" {
		return nil, fmt.Errorf("panel mTLS file paths are required")
	}

	rollback, err := installMTLSFiles([]mtlsFileUpdate{
		{path: keyPath, perm: 0600, body: keyPEM},
		{path: certPath, perm: 0644, body: certPEM},
		{path: caPath, perm: 0644, body: caPEM},
	})
	if err != nil {
		return nil, err
	}

	// Sanity check keypair.
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		rollback()
		return nil, fmt.Errorf("install cert/key failed sanity check: %w", err)
	}

	// Reload panel client (reads cert/ca from disk).
	newPC, err := NewPanelClient(a.cfg.PanelMTLSURL, a.cfg.NodeID, caPath, certPath, keyPath)
	if err != nil {
		rollback()
		return nil, err
	}

	// Keep backups for diagnostics.
	return newPC, nil
}
