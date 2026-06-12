package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

var ErrInvalidCredentials = errors.New("invalid credentials")

func ensureAdminCredential(ctx context.Context, db *sql.DB, username, password string, overwrite bool) error {
	if username == "" || password == "" {
		return errors.New("empty admin username or password")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	now := nowRFC3339()
	query := `
INSERT INTO admin_accounts(id, username, password_hash, created_at, updated_at)
VALUES (1, ?, ?, ?, ?)
ON CONFLICT(id) DO NOTHING;
`
	if overwrite {
		query = `
INSERT INTO admin_accounts(id, username, password_hash, created_at, updated_at)
VALUES (1, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	username = excluded.username,
	password_hash = excluded.password_hash,
	updated_at = excluded.updated_at;
`
	}
	_, err = db.ExecContext(ctx, query, username, string(hash), now, now)
	return err
}

func (s *Store) EnsureAdminCredential(ctx context.Context, username, password string) error {
	return ensureAdminCredential(ctx, s.db, username, password, false)
}

func (s *Store) UpsertAdminCredential(ctx context.Context, username, password string) error {
	return ensureAdminCredential(ctx, s.db, username, password, true)
}

func (s *Store) AuthenticateAdmin(ctx context.Context, username, password string) error {
	if username == "" || password == "" {
		return ErrInvalidCredentials
	}
	var hash string
	err := s.db.QueryRowContext(ctx, `
SELECT password_hash
FROM admin_accounts
WHERE username = ?
LIMIT 1;
`, username).Scan(&hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidCredentials
		}
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return ErrInvalidCredentials
	}
	return nil
}

func (s *Store) CreateAdminSession(ctx context.Context, username string, ttl time.Duration) (string, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)
	sid := uuid.NewString()

	_, err := s.db.ExecContext(ctx, `
INSERT INTO admin_sessions(session_id, username, expires_at, created_at, last_seen_at)
VALUES (?, ?, ?, ?, ?);
`, sid, username, expiresAt.Format(time.RFC3339), now.Format(time.RFC3339), now.Format(time.RFC3339))
	if err != nil {
		return "", err
	}
	return sid, nil
}

func (s *Store) ValidateAdminSession(ctx context.Context, sessionID string) (string, bool, error) {
	if sessionID == "" {
		return "", false, nil
	}
	var username, expiresAtStr string
	err := s.db.QueryRowContext(ctx, `
SELECT username, expires_at
FROM admin_sessions
WHERE session_id = ?;
`, sessionID).Scan(&username, &expiresAtStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}

	expiresAt, err := time.Parse(time.RFC3339, expiresAtStr)
	if err != nil {
		return "", false, err
	}
	if time.Now().UTC().After(expiresAt) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE session_id = ?`, sessionID)
		return "", false, nil
	}
	_, _ = s.db.ExecContext(ctx, `
UPDATE admin_sessions
SET last_seen_at = ?
WHERE session_id = ?;
`, nowRFC3339(), sessionID)
	return username, true, nil
}

func (s *Store) DeleteAdminSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE session_id = ?`, sessionID)
	return err
}
