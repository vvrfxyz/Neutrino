package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *Store) SetUserDeviceLimit(ctx context.Context, userID int64, deviceLimit int) (User, error) {
	if userID <= 0 {
		return User{}, errors.New("invalid user id")
	}
	if deviceLimit <= 0 {
		return User{}, errors.New("device_limit must be > 0")
	}
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
UPDATE users
SET device_limit = ?
WHERE id = ?;
`, deviceLimit, userID)
	if err != nil {
		return User{}, err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return User{}, ErrUserNotFound
	}
	_, _ = s.db.ExecContext(ctx, `
INSERT INTO enforcement_logs(user_id, action, reason, detail, created_at)
VALUES (?, 'device_limit', 'manual', ?, ?);
`, userID, fmt.Sprintf("set device_limit=%d", deviceLimit), nowStr)
	u, err := s.GetUser(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	return u, err
}
