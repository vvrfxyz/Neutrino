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

func writeOriginalMTLSFiles(t *testing.T, dir string) (keyPath, certPath, caPath string) {
	t.Helper()
	keyPath = filepath.Join(dir, "node.key")
	certPath = filepath.Join(dir, "node.crt")
	caPath = filepath.Join(dir, "ca-bundle.crt")
	if err := os.WriteFile(keyPath, []byte("old-key"), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if err := os.WriteFile(certPath, []byte("old-cert"), 0644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(caPath, []byte("old-ca"), 0644); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	return keyPath, certPath, caPath
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("unexpected content at %s: got=%q want=%q", path, got, want)
	}
}

func assertNoLeftovers(t *testing.T, dir, pattern string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("unexpected leftover files matching %s: %v", pattern, matches)
	}
}

func TestInstallMTLSFiles_TmpWriteFailure_PreservesOriginals(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only dir is not enforced for root")
	}
	dir := t.TempDir()
	keyPath, certPath, caPath := writeOriginalMTLSFiles(t, dir)

	// Put the CA file in a read-only directory so its tmp write fails after
	// the first two tmp files were written successfully.
	roDir := filepath.Join(dir, "ro")
	if err := os.Mkdir(roDir, 0500); err != nil {
		t.Fatalf("mkdir ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0700) })
	roCAPath := filepath.Join(roDir, "ca-bundle.crt")

	_, err := installMTLSFiles([]mtlsFileUpdate{
		{path: keyPath, perm: 0600, body: []byte("new-key")},
		{path: certPath, perm: 0644, body: []byte("new-cert")},
		{path: roCAPath, perm: 0644, body: []byte("new-ca")},
	})
	if err == nil {
		t.Fatalf("expected error")
	}

	// Originals must remain untouched; no backups existed yet at this step.
	assertFileContent(t, keyPath, "old-key")
	assertFileContent(t, certPath, "old-cert")
	assertFileContent(t, caPath, "old-ca")
	assertNoLeftovers(t, dir, "*.tmp.*")
	assertNoLeftovers(t, dir, "*.bak.*")
}

func TestInstallMTLSFiles_BackupFailure_RestoresOriginals(t *testing.T) {
	dir := t.TempDir()
	keyPath, certPath, caPath := writeOriginalMTLSFiles(t, dir)

	// Replace the cert path with a self-referencing symlink: tmp writes
	// succeed, but Stat in the backup step fails with ELOOP (non-NotExist),
	// after the key file was already moved to its backup.
	if err := os.Remove(certPath); err != nil {
		t.Fatalf("remove cert: %v", err)
	}
	if err := os.Symlink(certPath, certPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := installMTLSFiles([]mtlsFileUpdate{
		{path: keyPath, perm: 0600, body: []byte("new-key")},
		{path: certPath, perm: 0644, body: []byte("new-cert")},
		{path: caPath, perm: 0644, body: []byte("new-ca")},
	})
	if err == nil {
		t.Fatalf("expected error")
	}

	// Key (already backed up) must be restored; CA (not yet processed) must
	// not have been deleted.
	assertFileContent(t, keyPath, "old-key")
	assertFileContent(t, caPath, "old-ca")
	assertNoLeftovers(t, dir, "*.tmp.*")
}

func TestInstallMTLSFiles_Success_InstallsAndKeepsBackups(t *testing.T) {
	dir := t.TempDir()
	keyPath, certPath, caPath := writeOriginalMTLSFiles(t, dir)

	rollback, err := installMTLSFiles([]mtlsFileUpdate{
		{path: keyPath, perm: 0600, body: []byte("new-key")},
		{path: certPath, perm: 0644, body: []byte("new-cert")},
		{path: caPath, perm: 0644, body: []byte("new-ca")},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if rollback == nil {
		t.Fatalf("expected rollback func")
	}

	assertFileContent(t, keyPath, "new-key")
	assertFileContent(t, certPath, "new-cert")
	assertFileContent(t, caPath, "new-ca")
	assertNoLeftovers(t, dir, "*.tmp.*")

	// Backups must contain the old contents.
	for _, want := range []struct{ glob, body string }{
		{"node.key.bak.*", "old-key"},
		{"node.crt.bak.*", "old-cert"},
		{"ca-bundle.crt.bak.*", "old-ca"},
	} {
		matches, err := filepath.Glob(filepath.Join(dir, want.glob))
		if err != nil || len(matches) != 1 {
			t.Fatalf("expected one backup for %s, got %v (err=%v)", want.glob, matches, err)
		}
		assertFileContent(t, matches[0], want.body)
	}
}

func TestInstallMTLSFiles_RollbackAfterInstall_RestoresOriginals(t *testing.T) {
	dir := t.TempDir()
	keyPath, certPath, caPath := writeOriginalMTLSFiles(t, dir)

	rollback, err := installMTLSFiles([]mtlsFileUpdate{
		{path: keyPath, perm: 0600, body: []byte("new-key")},
		{path: certPath, perm: 0644, body: []byte("new-cert")},
		{path: caPath, perm: 0644, body: []byte("new-ca")},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	rollback()

	assertFileContent(t, keyPath, "old-key")
	assertFileContent(t, certPath, "old-cert")
	assertFileContent(t, caPath, "old-ca")
	assertNoLeftovers(t, dir, "*.tmp.*")
}

func TestInstallMTLSFiles_RollbackFreshInstall_RemovesNewFiles(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "node.key")

	rollback, err := installMTLSFiles([]mtlsFileUpdate{
		{path: keyPath, perm: 0600, body: []byte("new-key")},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	rollback()

	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("expected fresh-installed file removed, stat err=%v", err)
	}
}

func TestInstallMTLSFilesAndReloadPanelClient_SanityCheckFailure_RestoresOriginals(t *testing.T) {
	dir := t.TempDir()
	keyPath, certPath, caPath := writeOriginalMTLSFiles(t, dir)

	a := &Agent{cfg: Config{
		NodeID:                  1,
		PanelMTLSURL:            "https://panel.example:8443",
		PanelMTLSClientKeyPath:  keyPath,
		PanelMTLSClientCertPath: certPath,
		PanelMTLSCACertPath:     caPath,
	}}

	// Garbage PEM bodies install fine as files but fail the keypair sanity
	// check, which must roll the originals back.
	_, err := a.installMTLSFilesAndReloadPanelClient([]byte("bad-key"), []byte("bad-cert"), []byte("bad-ca"))
	if err == nil {
		t.Fatalf("expected error")
	}

	assertFileContent(t, keyPath, "old-key")
	assertFileContent(t, certPath, "old-cert")
	assertFileContent(t, caPath, "old-ca")
	assertNoLeftovers(t, dir, "*.tmp.*")
}

func TestInstallMTLSFilesAndReloadPanelClient_PanelClientFailure_RestoresOriginals(t *testing.T) {
	dir := t.TempDir()
	keyPath, certPath, caPath := writeOriginalMTLSFiles(t, dir)

	// Empty PanelMTLSURL makes NewPanelClient fail after a fully successful
	// file install + keypair sanity check.
	a := &Agent{cfg: Config{
		NodeID:                  1,
		PanelMTLSURL:            "",
		PanelMTLSClientKeyPath:  keyPath,
		PanelMTLSClientCertPath: certPath,
		PanelMTLSCACertPath:     caPath,
	}}

	certPEM, keyPEM := genSelfSignedPair(t)
	_, err := a.installMTLSFilesAndReloadPanelClient(keyPEM, certPEM, certPEM)
	if err == nil {
		t.Fatalf("expected error")
	}

	assertFileContent(t, keyPath, "old-key")
	assertFileContent(t, certPath, "old-cert")
	assertFileContent(t, caPath, "old-ca")
	assertNoLeftovers(t, dir, "*.tmp.*")
}

func genSelfSignedPair(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	now := time.Now().UTC()
	tpl := &x509.Certificate{
		SerialNumber:          mustSerial(t),
		Subject:               pkix.Name{CommonName: "node-1"},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}
