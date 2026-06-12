package repo

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

func randomTokenHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func (s *Store) createSubscriptionTokenTx(ctx context.Context, tx *sql.Tx, userID int64) (SubscriptionToken, error) {
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		token, err := randomTokenHex(24)
		if err != nil {
			return SubscriptionToken{}, err
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO subscription_tokens(user_id, token, enabled, created_at)
VALUES (?, ?, 1, ?)
ON CONFLICT(user_id) DO NOTHING;
`, userID, token, now.Format(time.RFC3339))
		if err != nil {
			return SubscriptionToken{}, err
		}
		out, getErr := s.getSubscriptionTokenByUserTx(ctx, tx, userID)
		if getErr == nil {
			return out, nil
		}
	}
	return SubscriptionToken{}, fmt.Errorf("create subscription token failed")
}

func (s *Store) getSubscriptionTokenByUserTx(ctx context.Context, tx *sql.Tx, userID int64) (SubscriptionToken, error) {
	var out SubscriptionToken
	var createdAt sql.NullString
	var expiresAt sql.NullString
	var lastUsedAt sql.NullString
	var enabledInt int
	err := tx.QueryRowContext(ctx, `
SELECT id, user_id, token, enabled, expires_at, last_used_at, created_at
FROM subscription_tokens
WHERE user_id = ?
LIMIT 1;
`, userID).Scan(&out.ID, &out.UserID, &out.Token, &enabledInt, &expiresAt, &lastUsedAt, &createdAt)
	if err != nil {
		return SubscriptionToken{}, err
	}
	out.Enabled = enabledInt == 1
	out.ExpiresAt = parseOptionalRFC3339(expiresAt)
	out.LastUsedAt = parseOptionalRFC3339(lastUsedAt)
	if createdAt.Valid {
		parsed, parseErr := time.Parse(time.RFC3339, createdAt.String)
		if parseErr == nil {
			out.CreatedAt = parsed
		}
	}
	return out, nil
}

func (s *Store) GetOrCreateSubscriptionToken(ctx context.Context, userID int64) (SubscriptionToken, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SubscriptionToken{}, err
	}
	defer tx.Rollback()

	tok, err := s.getSubscriptionTokenByUserTx(ctx, tx, userID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return SubscriptionToken{}, err
		}
		tok, err = s.createSubscriptionTokenTx(ctx, tx, userID)
		if err != nil {
			return SubscriptionToken{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return SubscriptionToken{}, err
	}
	return tok, nil
}

func (s *Store) GetSubscriptionTokenByUserID(ctx context.Context, userID int64) (SubscriptionToken, error) {
	var out SubscriptionToken
	var createdAt sql.NullString
	var expiresAt sql.NullString
	var lastUsedAt sql.NullString
	var enabledInt int
	err := s.db.QueryRowContext(ctx, `
SELECT id, user_id, token, enabled, expires_at, last_used_at, created_at
FROM subscription_tokens
WHERE user_id = ?
LIMIT 1;
`, userID).Scan(&out.ID, &out.UserID, &out.Token, &enabledInt, &expiresAt, &lastUsedAt, &createdAt)
	if err != nil {
		return SubscriptionToken{}, err
	}
	out.Enabled = enabledInt == 1
	out.ExpiresAt = parseOptionalRFC3339(expiresAt)
	out.LastUsedAt = parseOptionalRFC3339(lastUsedAt)
	if createdAt.Valid {
		parsed, parseErr := time.Parse(time.RFC3339, createdAt.String)
		if parseErr == nil {
			out.CreatedAt = parsed
		}
	}
	return out, nil
}

func (s *Store) RotateSubscriptionToken(ctx context.Context, userID int64) (SubscriptionToken, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SubscriptionToken{}, err
	}
	defer tx.Rollback()

	token, err := randomTokenHex(24)
	if err != nil {
		return SubscriptionToken{}, err
	}
	_, err = tx.ExecContext(ctx, `
UPDATE subscription_tokens
SET token = ?, enabled = 1
WHERE user_id = ?;
`, token, userID)
	if err != nil {
		return SubscriptionToken{}, err
	}
	out, err := s.getSubscriptionTokenByUserTx(ctx, tx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			out, err = s.createSubscriptionTokenTx(ctx, tx, userID)
		}
		if err != nil {
			return SubscriptionToken{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return SubscriptionToken{}, err
	}
	return out, nil
}

func (s *Store) GetUserBySubscriptionToken(ctx context.Context, token string) (User, SubscriptionToken, error) {
	// No SweepExpiredUsers here: /sub is a public endpoint and a sweep is a
	// full write transaction. Freshness is preserved by the status/expiry
	// checks below plus the periodic enforcement sweep.
	nowTime := time.Now().UTC()
	var userID int64
	var enabled int
	var expiresAt sql.NullString
	var lastUsedAt sql.NullString
	var createdAt string
	var subID int64
	err := s.db.QueryRowContext(ctx, `
SELECT id, user_id, enabled, expires_at, last_used_at, created_at
FROM subscription_tokens
WHERE token = ?
LIMIT 1;
`, strings.TrimSpace(token)).Scan(&subID, &userID, &enabled, &expiresAt, &lastUsedAt, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, SubscriptionToken{}, ErrUserNotFound
		}
		return User{}, SubscriptionToken{}, err
	}
	if enabled != 1 {
		return User{}, SubscriptionToken{}, ErrUserInactive
	}
	exp := parseOptionalRFC3339(expiresAt)
	if exp != nil && nowTime.After(*exp) {
		return User{}, SubscriptionToken{}, ErrUserInactive
	}
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return User{}, SubscriptionToken{}, err
	}
	if u.Status != "active" || !u.ExpiresAt.After(nowTime) {
		return User{}, SubscriptionToken{}, ErrUserInactive
	}
	now := nowTime.Format(time.RFC3339)
	_, _ = s.db.ExecContext(ctx, `UPDATE subscription_tokens SET last_used_at = ? WHERE id = ?`, now, subID)
	created, _ := time.Parse(time.RFC3339, createdAt)
	return u, SubscriptionToken{
		ID:         subID,
		UserID:     userID,
		Token:      token,
		Enabled:    true,
		ExpiresAt:  exp,
		LastUsedAt: parseOptionalRFC3339(lastUsedAt),
		CreatedAt:  created,
	}, nil
}

