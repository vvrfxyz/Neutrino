package repo

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

func (s *Store) GetXrayState(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `
SELECT value
FROM xray_state
WHERE key = ?;
`, key).Scan(&value)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, err
	}
	return value, true, nil
}

func (s *Store) SetXrayState(ctx context.Context, key, value string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO xray_state(key, value, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET
	value = excluded.value,
	updated_at = excluded.updated_at;
`, key, value, now)
	return err
}

func (s *Store) getXrayStateTx(ctx context.Context, tx *sql.Tx, key string) (string, bool, error) {
	var value string
	err := tx.QueryRowContext(ctx, `
SELECT value
FROM xray_state
WHERE key = ?;
`, key).Scan(&value)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, err
	}
	return value, true, nil
}

func (s *Store) setXrayStateTx(ctx context.Context, tx *sql.Tx, key, value string, at time.Time) error {
	updatedAt := at.UTC().Format(time.RFC3339)
	_, err := tx.ExecContext(ctx, `
INSERT INTO xray_state(key, value, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET
	value = excluded.value,
	updated_at = excluded.updated_at;
`, key, value, updatedAt)
	return err
}

func (s *Store) GetXrayStateInt64(ctx context.Context, key string) (int64, bool, error) {
	raw, ok, err := s.GetXrayState(ctx, key)
	if err != nil || !ok {
		return 0, ok, err
	}
	v, parseErr := strconv.ParseInt(raw, 10, 64)
	if parseErr != nil {
		return 0, false, nil
	}
	return v, true, nil
}

func (s *Store) SetXrayStateInt64(ctx context.Context, key string, value int64) error {
	return s.SetXrayState(ctx, key, strconv.FormatInt(value, 10))
}

func (s *Store) getXrayStateInt64Tx(ctx context.Context, tx *sql.Tx, key string) (int64, bool, error) {
	raw, ok, err := s.getXrayStateTx(ctx, tx, key)
	if err != nil || !ok {
		return 0, ok, err
	}
	v, parseErr := strconv.ParseInt(raw, 10, 64)
	if parseErr != nil {
		return 0, false, nil
	}
	return v, true, nil
}

func (s *Store) setXrayStateInt64Tx(ctx context.Context, tx *sql.Tx, key string, value int64, at time.Time) error {
	return s.setXrayStateTx(ctx, tx, key, strconv.FormatInt(value, 10), at)
}
