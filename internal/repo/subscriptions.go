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

const (
	telegramBindCodeBytes      = 16
	telegramBindCodeTTL        = 24 * time.Hour
	telegramBindAttemptWindow  = 10 * time.Minute
	telegramBindMaxAttempts    = 5
	telegramBindAttemptGCGrace = 48 * time.Hour
)

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

func scanTelegramBindingRow(scan func(dest ...any) error) (TelegramBinding, error) {
	var out TelegramBinding
	var bindCodeExpiresAt sql.NullString
	var chatID sql.NullInt64
	var userID sql.NullInt64
	var username sql.NullString
	var verifiedAt sql.NullString
	var createdAt string
	var updatedAt string
	if err := scan(
		&out.ID,
		&out.UserID,
		&out.BindCode,
		&bindCodeExpiresAt,
		&chatID,
		&userID,
		&username,
		&verifiedAt,
		&createdAt,
		&updatedAt,
	); err != nil {
		return TelegramBinding{}, err
	}
	if chatID.Valid {
		v := chatID.Int64
		out.TelegramChatID = &v
	}
	if userID.Valid {
		v := userID.Int64
		out.TelegramUserID = &v
	}
	out.BindCodeExpiresAt = parseOptionalRFC3339(bindCodeExpiresAt)
	out.TelegramUsername = username.String
	out.VerifiedAt = parseOptionalRFC3339(verifiedAt)
	out.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	out.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return out, nil
}

func (s *Store) ensureTelegramBindingTx(ctx context.Context, tx *sql.Tx, userID int64) (TelegramBinding, error) {
	out, err := scanTelegramBindingRow(func(dest ...any) error {
		return tx.QueryRowContext(ctx, `
SELECT id, user_id, bind_code, bind_code_expires_at, telegram_chat_id, telegram_user_id, telegram_username, verified_at, created_at, updated_at
FROM telegram_bindings
WHERE user_id = ?
LIMIT 1;
`, userID).Scan(dest...)
	})
	if err == nil {
		if out.VerifiedAt == nil && (out.BindCodeExpiresAt == nil || !out.BindCodeExpiresAt.After(time.Now().UTC())) {
			if err := s.setTelegramBindCodeTx(ctx, tx, userID, time.Now().UTC(), false); err != nil {
				return TelegramBinding{}, err
			}
			return scanTelegramBindingRow(func(dest ...any) error {
				return tx.QueryRowContext(ctx, `
SELECT id, user_id, bind_code, bind_code_expires_at, telegram_chat_id, telegram_user_id, telegram_username, verified_at, created_at, updated_at
FROM telegram_bindings
WHERE user_id = ?
LIMIT 1;
`, userID).Scan(dest...)
			})
		}
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return TelegramBinding{}, err
	}
	nowTime := time.Now().UTC()
	now := nowTime.Format(time.RFC3339)
	expiresAt := nowTime.Add(telegramBindCodeTTL).Format(time.RFC3339)
	for i := 0; i < 4; i++ {
		code, genErr := randomTokenHex(telegramBindCodeBytes)
		if genErr != nil {
			return TelegramBinding{}, genErr
		}
		_, insErr := tx.ExecContext(ctx, `
INSERT INTO telegram_bindings(user_id, bind_code, bind_code_expires_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?);
`, userID, strings.ToUpper(code), expiresAt, now, now)
		if insErr == nil {
			break
		}
		if !strings.Contains(strings.ToLower(insErr.Error()), "unique") {
			return TelegramBinding{}, insErr
		}
	}
	return scanTelegramBindingRow(func(dest ...any) error {
		return tx.QueryRowContext(ctx, `
SELECT id, user_id, bind_code, bind_code_expires_at, telegram_chat_id, telegram_user_id, telegram_username, verified_at, created_at, updated_at
FROM telegram_bindings
WHERE user_id = ?
LIMIT 1;
`, userID).Scan(dest...)
	})
}

func newTelegramBindCode() (string, error) {
	code, err := randomTokenHex(telegramBindCodeBytes)
	if err != nil {
		return "", err
	}
	return strings.ToUpper(code), nil
}

func (s *Store) setTelegramBindCodeTx(ctx context.Context, tx *sql.Tx, userID int64, now time.Time, clearBinding bool) error {
	now = now.UTC()
	nowStr := now.Format(time.RFC3339)
	expiresAt := now.Add(telegramBindCodeTTL).Format(time.RFC3339)
	for i := 0; i < 4; i++ {
		code, err := newTelegramBindCode()
		if err != nil {
			return err
		}
		var execErr error
		if clearBinding {
			_, execErr = tx.ExecContext(ctx, `
UPDATE telegram_bindings
SET bind_code = ?, bind_code_expires_at = ?, verified_at = NULL, telegram_chat_id = NULL, telegram_user_id = NULL, telegram_username = NULL, updated_at = ?
WHERE user_id = ?;
`, code, expiresAt, nowStr, userID)
		} else {
			_, execErr = tx.ExecContext(ctx, `
UPDATE telegram_bindings
SET bind_code = ?, bind_code_expires_at = ?, updated_at = ?
WHERE user_id = ?;
`, code, expiresAt, nowStr, userID)
		}
		if execErr == nil {
			return nil
		}
		if !strings.Contains(strings.ToLower(execErr.Error()), "unique") {
			return execErr
		}
	}
	return fmt.Errorf("generate telegram bind code failed")
}

func (s *Store) recordTelegramBindAttemptTx(ctx context.Context, tx *sql.Tx, code string, chatID int64, now time.Time) error {
	now = now.UTC()
	cutoff := now.Add(-telegramBindAttemptGCGrace).Format(time.RFC3339)
	_, _ = tx.ExecContext(ctx, `DELETE FROM telegram_bind_attempts WHERE last_attempt_at < ?`, cutoff)

	windowCutoff := now.Add(-telegramBindAttemptWindow).Format(time.RFC3339)
	var chatAttempts int
	if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(SUM(attempts), 0)
FROM telegram_bind_attempts
WHERE chat_id = ? AND window_start >= ?;
`, chatID, windowCutoff).Scan(&chatAttempts); err != nil {
		return err
	}
	if chatAttempts >= telegramBindMaxAttempts*3 {
		return ErrTelegramBindRateLimited
	}

	nowStr := now.Format(time.RFC3339)
	var attempts int
	var windowStartRaw string
	err := tx.QueryRowContext(ctx, `
SELECT attempts, window_start
FROM telegram_bind_attempts
WHERE chat_id = ? AND code = ?;
`, chatID, code).Scan(&attempts, &windowStartRaw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, `
INSERT INTO telegram_bind_attempts(chat_id, code, attempts, window_start, last_attempt_at)
VALUES (?, ?, 1, ?, ?);
`, chatID, code, nowStr, nowStr)
			return err
		}
		return err
	}
	windowStart, parseErr := time.Parse(time.RFC3339, windowStartRaw)
	if parseErr != nil || now.Sub(windowStart) >= telegramBindAttemptWindow {
		_, err = tx.ExecContext(ctx, `
UPDATE telegram_bind_attempts
SET attempts = 1, window_start = ?, last_attempt_at = ?
WHERE chat_id = ? AND code = ?;
`, nowStr, nowStr, chatID, code)
		return err
	}
	if attempts >= telegramBindMaxAttempts {
		return ErrTelegramBindRateLimited
	}
	_, err = tx.ExecContext(ctx, `
UPDATE telegram_bind_attempts
SET attempts = attempts + 1, last_attempt_at = ?
WHERE chat_id = ? AND code = ?;
`, nowStr, chatID, code)
	return err
}

func (s *Store) EnsureTelegramBinding(ctx context.Context, userID int64) (TelegramBinding, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TelegramBinding{}, err
	}
	defer tx.Rollback()
	out, err := s.ensureTelegramBindingTx(ctx, tx, userID)
	if err != nil {
		return TelegramBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return TelegramBinding{}, err
	}
	return out, nil
}

func (s *Store) GetTelegramBindingByUserID(ctx context.Context, userID int64) (TelegramBinding, error) {
	return scanTelegramBindingRow(func(dest ...any) error {
		return s.db.QueryRowContext(ctx, `
SELECT id, user_id, bind_code, bind_code_expires_at, telegram_chat_id, telegram_user_id, telegram_username, verified_at, created_at, updated_at
FROM telegram_bindings
WHERE user_id = ?
LIMIT 1;
`, userID).Scan(dest...)
	})
}

func (s *Store) RegenerateTelegramBindCode(ctx context.Context, userID int64) (TelegramBinding, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TelegramBinding{}, err
	}
	defer tx.Rollback()
	if _, err := s.ensureTelegramBindingTx(ctx, tx, userID); err != nil {
		return TelegramBinding{}, err
	}
	if err := s.setTelegramBindCodeTx(ctx, tx, userID, time.Now().UTC(), true); err != nil {
		return TelegramBinding{}, err
	}
	out, err := scanTelegramBindingRow(func(dest ...any) error {
		return tx.QueryRowContext(ctx, `
SELECT id, user_id, bind_code, bind_code_expires_at, telegram_chat_id, telegram_user_id, telegram_username, verified_at, created_at, updated_at
FROM telegram_bindings
WHERE user_id = ?
LIMIT 1;
`, userID).Scan(dest...)
	})
	if err != nil {
		return TelegramBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return TelegramBinding{}, err
	}
	return out, nil
}

func (s *Store) BindTelegramChatByCode(ctx context.Context, code string, chatID, telegramUserID int64, telegramUsername string) (User, TelegramBinding, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" || chatID == 0 {
		return User{}, TelegramBinding{}, ErrUserNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, TelegramBinding{}, err
	}
	defer tx.Rollback()

	nowTime := time.Now().UTC()
	if err := s.recordTelegramBindAttemptTx(ctx, tx, code, chatID, nowTime); err != nil {
		return User{}, TelegramBinding{}, err
	}

	var userID int64
	var expiresAtRaw sql.NullString
	if err := tx.QueryRowContext(ctx, `
SELECT user_id, bind_code_expires_at
FROM telegram_bindings
WHERE bind_code = ?
LIMIT 1;
`, code).Scan(&userID, &expiresAtRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, TelegramBinding{}, ErrUserNotFound
		}
		return User{}, TelegramBinding{}, err
	}
	expiresAt := parseOptionalRFC3339(expiresAtRaw)
	if expiresAt == nil || !expiresAt.After(nowTime) {
		return User{}, TelegramBinding{}, ErrUserNotFound
	}

	now := nowTime.Format(time.RFC3339)
	_, _ = tx.ExecContext(ctx, `
UPDATE telegram_bindings
SET telegram_chat_id = NULL, telegram_user_id = NULL, telegram_username = NULL, verified_at = NULL, updated_at = ?
WHERE telegram_chat_id = ? AND user_id <> ?;
`, now, chatID, userID)

	nextCode, err := newTelegramBindCode()
	if err != nil {
		return User{}, TelegramBinding{}, err
	}
	_, err = tx.ExecContext(ctx, `
UPDATE telegram_bindings
SET bind_code = ?, bind_code_expires_at = ?, telegram_chat_id = ?, telegram_user_id = ?, telegram_username = ?, verified_at = ?, updated_at = ?
WHERE user_id = ?;
`, nextCode, now, chatID, nullableInt64(telegramUserID), nullableString(telegramUsername), now, now, userID)
	if err != nil {
		return User{}, TelegramBinding{}, err
	}
	_, _ = tx.ExecContext(ctx, `DELETE FROM telegram_bind_attempts WHERE chat_id = ? OR code = ?`, chatID, code)
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return User{}, TelegramBinding{}, err
	}
	binding, err := scanTelegramBindingRow(func(dest ...any) error {
		return tx.QueryRowContext(ctx, `
SELECT id, user_id, bind_code, bind_code_expires_at, telegram_chat_id, telegram_user_id, telegram_username, verified_at, created_at, updated_at
FROM telegram_bindings
WHERE user_id = ?
LIMIT 1;
`, userID).Scan(dest...)
	})
	if err != nil {
		return User{}, TelegramBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, TelegramBinding{}, err
	}
	return u, binding, nil
}

func (s *Store) GetUserByTelegramChatID(ctx context.Context, chatID int64) (User, error) {
	if chatID == 0 {
		return User{}, ErrUserNotFound
	}
	var userID int64
	err := s.db.QueryRowContext(ctx, `
SELECT user_id
FROM telegram_bindings
WHERE telegram_chat_id = ? AND verified_at IS NOT NULL
LIMIT 1;
`, chatID).Scan(&userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrUserNotFound
		}
		return User{}, err
	}
	return s.GetUser(ctx, userID)
}
