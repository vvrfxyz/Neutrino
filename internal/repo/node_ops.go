package repo

import (
	"context"
	"fmt"
)

func (s *Store) SetNodeEnabled(ctx context.Context, nodeID int64, enabled bool) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	v := 0
	if enabled {
		v = 1
	}
	now := nowRFC3339()
	res, err := s.db.ExecContext(ctx, `
UPDATE nodes
SET enabled = ?, updated_at = ?
WHERE id = ?;
`, v, now, nodeID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNodeNotFound
	}
	return nil
}
