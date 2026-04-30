package repo

import (
	"context"
	"database/sql"
	"time"
)

func (s *Store) InsertBackupRecord(ctx context.Context, rec BackupRecord) (BackupRecord, error) {
	if rec.Storage == "" {
		rec.Storage = "local"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var id int64
	err := s.db.QueryRowContext(ctx, `
INSERT INTO backups(file_path, size_bytes, sha256, storage, object_key, created_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING id;
`, rec.FilePath, rec.SizeBytes, rec.SHA256, rec.Storage, nullableString(rec.ObjectKey), now).Scan(&id)
	if err != nil {
		return BackupRecord{}, err
	}
	rec.ID = id
	rec.CreatedAt, _ = time.Parse(time.RFC3339, now)
	return rec, nil
}

func (s *Store) ListBackups(ctx context.Context, limit int) ([]BackupRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, file_path, size_bytes, sha256, storage, object_key, created_at
FROM backups
ORDER BY id DESC
LIMIT ?;
`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BackupRecord, 0, limit)
	for rows.Next() {
		var b BackupRecord
		var objectKey sql.NullString
		var createdAt string
		if err := rows.Scan(&b.ID, &b.FilePath, &b.SizeBytes, &b.SHA256, &b.Storage, &objectKey, &createdAt); err != nil {
			return nil, err
		}
		b.ObjectKey = objectKey.String
		b.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) DeleteBackup(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM backups WHERE id = ?;`, id)
	return err
}

func (s *Store) GetBackup(ctx context.Context, id int64) (BackupRecord, error) {
	var b BackupRecord
	var objectKey sql.NullString
	var createdAt string
	err := s.db.QueryRowContext(ctx, `
SELECT id, file_path, size_bytes, sha256, storage, object_key, created_at
FROM backups
WHERE id = ?;
`, id).Scan(&b.ID, &b.FilePath, &b.SizeBytes, &b.SHA256, &b.Storage, &objectKey, &createdAt)
	if err != nil {
		return BackupRecord{}, err
	}
	b.ObjectKey = objectKey.String
	b.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return b, nil
}
