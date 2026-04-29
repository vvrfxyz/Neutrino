package repo

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

type NodeEnrollCode struct {
	NodeID    int64
	Code      string
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
}

func (s *Store) GetNodeEnrollCode(ctx context.Context, nodeID int64) (NodeEnrollCode, bool, error) {
	if nodeID <= 0 {
		return NodeEnrollCode{}, false, fmt.Errorf("invalid node id")
	}
	var code, expiresAtRaw, createdAtRaw string
	var usedAtRaw sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT code, expires_at, used_at, created_at
FROM node_enroll_codes
WHERE node_id = ?;
`, nodeID).Scan(&code, &expiresAtRaw, &usedAtRaw, &createdAtRaw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NodeEnrollCode{}, false, nil
		}
		return NodeEnrollCode{}, false, err
	}
	exp, err := time.Parse(time.RFC3339, expiresAtRaw)
	if err != nil {
		return NodeEnrollCode{}, false, err
	}
	createdAt, err := time.Parse(time.RFC3339, createdAtRaw)
	if err != nil {
		return NodeEnrollCode{}, false, err
	}
	var usedAt *time.Time
	if usedAtRaw.Valid && strings.TrimSpace(usedAtRaw.String) != "" {
		if t, err := time.Parse(time.RFC3339, usedAtRaw.String); err == nil {
			usedAt = &t
		}
	}
	return NodeEnrollCode{
		NodeID:    nodeID,
		Code:      strings.TrimSpace(code),
		ExpiresAt: exp,
		UsedAt:    usedAt,
		CreatedAt: createdAt,
	}, true, nil
}

func newEnrollCode() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// URL-safe, no padding.
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Store) CreateNodeEnrollCode(ctx context.Context, nodeID int64, ttl time.Duration) (NodeEnrollCode, error) {
	if nodeID <= 0 {
		return NodeEnrollCode{}, fmt.Errorf("invalid node id")
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	code, err := newEnrollCode()
	if err != nil {
		return NodeEnrollCode{}, err
	}
	now := time.Now().UTC()
	exp := now.Add(ttl)
	nowStr := now.Format(time.RFC3339)
	expStr := exp.Format(time.RFC3339)

	_, err = s.db.ExecContext(ctx, `
INSERT INTO node_enroll_codes(node_id, code, expires_at, used_at, created_at)
VALUES (?, ?, ?, NULL, ?)
ON CONFLICT(node_id) DO UPDATE SET
	code = excluded.code,
	expires_at = excluded.expires_at,
	used_at = NULL,
	created_at = excluded.created_at;
`, nodeID, code, expStr, nowStr)
	if err != nil {
		return NodeEnrollCode{}, err
	}
	return NodeEnrollCode{NodeID: nodeID, Code: code, ExpiresAt: exp, CreatedAt: now}, nil
}

// ConsumeNodeEnrollCode validates and marks the code as used. It is safe to call concurrently.
func (s *Store) ConsumeNodeEnrollCode(ctx context.Context, nodeID int64, code string) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := consumeNodeEnrollCodeTx(ctx, tx, nodeID, code, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// ValidateNodeEnrollCode checks that a one-time code is currently usable without consuming it.
// Callers must still use ConsumeNodeEnrollCode or CompleteNodeEnroll for the final atomic consume.
func (s *Store) ValidateNodeEnrollCode(ctx context.Context, nodeID int64, code string) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	_, err := validateNodeEnrollCode(ctx, s.db, nodeID, code, time.Now().UTC())
	return err
}

type nodeEnrollCodeQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func validateNodeEnrollCode(ctx context.Context, q nodeEnrollCodeQuerier, nodeID int64, code string, now time.Time) (string, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return "", fmt.Errorf("missing enroll_code")
	}

	var dbCode string
	var expiresAt string
	var usedAt sql.NullString
	err := q.QueryRowContext(ctx, `
SELECT code, expires_at, used_at
FROM node_enroll_codes
WHERE node_id = ?;
`, nodeID).Scan(&dbCode, &expiresAt, &usedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("invalid enroll_code")
		}
		return "", err
	}
	if strings.TrimSpace(dbCode) == "" || dbCode != code {
		return "", fmt.Errorf("invalid enroll_code")
	}
	if usedAt.Valid && strings.TrimSpace(usedAt.String) != "" {
		return "", fmt.Errorf("enroll_code already used")
	}
	exp, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return "", fmt.Errorf("invalid enroll_code")
	}
	if now.UTC().After(exp) {
		return "", fmt.Errorf("enroll_code expired")
	}
	return code, nil
}

func consumeNodeEnrollCodeTx(ctx context.Context, tx *sql.Tx, nodeID int64, code string, now time.Time) error {
	code, err := validateNodeEnrollCode(ctx, tx, nodeID, code, now)
	if err != nil {
		return err
	}
	nowStr := now.UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `
UPDATE node_enroll_codes
SET used_at = ?
WHERE node_id = ? AND code = ? AND used_at IS NULL;
`, nowStr, nodeID, code)
	if err != nil {
		return err
	}
	aff, _ := res.RowsAffected()
	if aff == 0 {
		return fmt.Errorf("enroll_code already used")
	}
	return nil
}
