package repo

import (
	"context"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"neutrino/internal/certutil"
)

type NodeCertPin struct {
	ID            int64
	NodeID        int64
	Purpose       string
	SPKISHA256    string
	CertSHA256    string
	SerialHex     string
	Subject       string
	Issuer        string
	NotBefore     time.Time
	NotAfter      time.Time
	CreatedAt     time.Time
	LastSeenAt    *time.Time
	RevokedAt     *time.Time
	RevokedReason string
}

func (s *Store) UpsertNodeCertPin(ctx context.Context, nodeID int64, purpose string, cert *x509.Certificate, now time.Time) error {
	if nodeID <= 0 || cert == nil {
		return fmt.Errorf("invalid pin input")
	}
	purpose = strings.TrimSpace(purpose)
	if purpose == "" {
		purpose = "agent_client"
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	nowStr := now.UTC().Format(time.RFC3339)
	nb := cert.NotBefore.UTC().Format(time.RFC3339)
	na := cert.NotAfter.UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO node_cert_pins(node_id, purpose, spki_sha256, cert_sha256, serial_hex, subject, issuer, not_before, not_after, created_at, last_seen_at, revoked_at, revoked_reason)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL)
ON CONFLICT(node_id, purpose, cert_sha256) DO UPDATE SET
	last_seen_at = excluded.last_seen_at,
	revoked_at = NULL,
	revoked_reason = NULL;
`, nodeID, purpose, certutil.SPKISHA256Hex(cert), certutil.CertSHA256Hex(cert), certutil.SerialHex(cert), cert.Subject.String(), cert.Issuer.String(), nb, na, nowStr, nowStr)
	return err
}

// CompleteNodeEnroll consumes the one-time enroll code and installs the signed
// client certificate in a single transaction. If any DB write fails, the code
// remains usable because the transaction rolls back.
func (s *Store) CompleteNodeEnroll(ctx context.Context, nodeID int64, code string, cert *x509.Certificate, now time.Time) (int64, error) {
	if nodeID <= 0 || cert == nil {
		return 0, fmt.Errorf("invalid enroll input")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if err := consumeNodeEnrollCodeTx(ctx, tx, nodeID, code, now); err != nil {
		return 0, err
	}

	nowStr := now.Format(time.RFC3339)
	nb := cert.NotBefore.UTC().Format(time.RFC3339)
	na := cert.NotAfter.UTC().Format(time.RFC3339)
	certSHA := certutil.CertSHA256Hex(cert)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO node_cert_pins(node_id, purpose, spki_sha256, cert_sha256, serial_hex, subject, issuer, not_before, not_after, created_at, last_seen_at, revoked_at, revoked_reason)
VALUES (?, 'agent_client', ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL)
ON CONFLICT(node_id, purpose, cert_sha256) DO UPDATE SET
	last_seen_at = excluded.last_seen_at,
	revoked_at = NULL,
	revoked_reason = NULL;
`, nodeID, certutil.SPKISHA256Hex(cert), certSHA, certutil.SerialHex(cert), cert.Subject.String(), cert.Issuer.String(), nb, na, nowStr, nowStr); err != nil {
		return 0, err
	}

	res, err := tx.ExecContext(ctx, `
UPDATE node_cert_pins
SET revoked_at = ?, revoked_reason = 'auto_rotate'
WHERE node_id = ? AND purpose = 'agent_client' AND revoked_at IS NULL AND cert_sha256 != ?;
`, nowStr, nodeID, certSHA)
	if err != nil {
		return 0, err
	}
	revoked, _ := res.RowsAffected()

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return revoked, nil
}

// RenewNodeCertPin installs a freshly renewed client certificate and revokes
// previously active certs for the same node in one transaction.
func (s *Store) RenewNodeCertPin(ctx context.Context, nodeID int64, purpose string, cert *x509.Certificate, now time.Time) (int64, error) {
	if nodeID <= 0 || cert == nil {
		return 0, fmt.Errorf("invalid renew input")
	}
	purpose = strings.TrimSpace(purpose)
	if purpose == "" {
		purpose = "agent_client"
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	nowStr := now.Format(time.RFC3339)
	nb := cert.NotBefore.UTC().Format(time.RFC3339)
	na := cert.NotAfter.UTC().Format(time.RFC3339)
	certSHA := certutil.CertSHA256Hex(cert)
	if _, err := tx.ExecContext(ctx, `
	INSERT INTO node_cert_pins(node_id, purpose, spki_sha256, cert_sha256, serial_hex, subject, issuer, not_before, not_after, created_at, last_seen_at, revoked_at, revoked_reason)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL)
	ON CONFLICT(node_id, purpose, cert_sha256) DO UPDATE SET
		last_seen_at = excluded.last_seen_at,
		revoked_at = NULL,
		revoked_reason = NULL;
	`, nodeID, purpose, certutil.SPKISHA256Hex(cert), certSHA, certutil.SerialHex(cert), cert.Subject.String(), cert.Issuer.String(), nb, na, nowStr, nowStr); err != nil {
		return 0, err
	}

	res, err := tx.ExecContext(ctx, `
	UPDATE node_cert_pins
	SET revoked_at = ?, revoked_reason = 'auto_rotate'
	WHERE node_id = ? AND purpose = ? AND revoked_at IS NULL AND cert_sha256 != ?;
	`, nowStr, nodeID, purpose, certSHA)
	if err != nil {
		return 0, err
	}
	revoked, _ := res.RowsAffected()

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return revoked, nil
}

// ValidateNodeMTLSCert enforces allowlist+revocation. It updates last_seen_at on success.
func (s *Store) ValidateNodeMTLSCert(ctx context.Context, nodeID int64, cert *x509.Certificate, now time.Time) error {
	if nodeID <= 0 || cert == nil {
		return fmt.Errorf("invalid node/cert")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	spki := certutil.SPKISHA256Hex(cert)
	csha := certutil.CertSHA256Hex(cert)
	if spki == "" || csha == "" {
		return fmt.Errorf("invalid cert fingerprint")
	}

	var revokedAt sql.NullString
	var revokedReason sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT revoked_at, revoked_reason
FROM node_cert_pins
WHERE node_id = ? AND purpose = 'agent_client' AND spki_sha256 = ? AND cert_sha256 = ?
ORDER BY id DESC
LIMIT 1;
`, nodeID, spki, csha).Scan(&revokedAt, &revokedReason)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("node cert not allowed")
		}
		return err
	}
	if revokedAt.Valid && strings.TrimSpace(revokedAt.String) != "" {
		msg := "node cert revoked"
		if revokedReason.Valid && strings.TrimSpace(revokedReason.String) != "" {
			msg = msg + ": " + revokedReason.String
		}
		return fmt.Errorf("%s", msg)
	}
	// Touch last_seen_at on success.
	_, _ = s.db.ExecContext(ctx, `
UPDATE node_cert_pins
SET last_seen_at = ?
WHERE node_id = ? AND purpose = 'agent_client' AND spki_sha256 = ? AND cert_sha256 = ?;
`, now.UTC().Format(time.RFC3339), nodeID, spki, csha)
	return nil
}

func (s *Store) RevokeNodeCertPin(ctx context.Context, nodeID int64, certSHA256 string, reason string) (int64, error) {
	if nodeID <= 0 {
		return 0, fmt.Errorf("invalid node id")
	}
	certSHA256 = strings.TrimSpace(strings.ToLower(certSHA256))
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "manual"
	}
	now := nowRFC3339()
	var res sql.Result
	var err error
	if certSHA256 == "" {
		res, err = s.db.ExecContext(ctx, `
UPDATE node_cert_pins
SET revoked_at = ?, revoked_reason = ?
WHERE node_id = ? AND purpose = 'agent_client' AND revoked_at IS NULL;
`, now, reason, nodeID)
	} else {
		res, err = s.db.ExecContext(ctx, `
UPDATE node_cert_pins
SET revoked_at = ?, revoked_reason = ?
WHERE node_id = ? AND purpose = 'agent_client' AND cert_sha256 = ?;
`, now, reason, nodeID, certSHA256)
	}
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) ListNodeCertPins(ctx context.Context, nodeID int64, limit int) ([]NodeCertPin, error) {
	if nodeID <= 0 {
		return nil, fmt.Errorf("invalid node id")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, node_id, purpose, spki_sha256, cert_sha256, serial_hex, subject, issuer, not_before, not_after, created_at, last_seen_at, revoked_at, revoked_reason
FROM node_cert_pins
WHERE node_id = ?
ORDER BY id DESC
LIMIT ?;
`, nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NodeCertPin, 0, limit)
	for rows.Next() {
		var p NodeCertPin
		var notBefore, notAfter, createdAt string
		var lastSeenAt, revokedAt, revokedReason sql.NullString
		if err := rows.Scan(&p.ID, &p.NodeID, &p.Purpose, &p.SPKISHA256, &p.CertSHA256, &p.SerialHex, &p.Subject, &p.Issuer, &notBefore, &notAfter, &createdAt, &lastSeenAt, &revokedAt, &revokedReason); err != nil {
			return nil, err
		}
		p.NotBefore, _ = time.Parse(time.RFC3339, notBefore)
		p.NotAfter, _ = time.Parse(time.RFC3339, notAfter)
		p.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		if lastSeenAt.Valid && strings.TrimSpace(lastSeenAt.String) != "" {
			if t, err := time.Parse(time.RFC3339, lastSeenAt.String); err == nil {
				p.LastSeenAt = &t
			}
		}
		if revokedAt.Valid && strings.TrimSpace(revokedAt.String) != "" {
			if t, err := time.Parse(time.RFC3339, revokedAt.String); err == nil {
				p.RevokedAt = &t
			}
		}
		if revokedReason.Valid {
			p.RevokedReason = revokedReason.String
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
