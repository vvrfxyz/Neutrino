package service

import (
	"context"
	"database/sql"
	"os"

	"neutrino/internal/backup"
	"neutrino/internal/repo"
)

// BackupStore is the narrow repo surface BackupService depends on. RawDB is
// required because backup.CreateSQLiteBackup runs VACUUM INTO on the live
// *sql.DB handle. *repo.Store satisfies it.
type BackupStore interface {
	ListBackups(ctx context.Context, limit int) ([]repo.BackupRecord, error)
	GetBackup(ctx context.Context, id int64) (repo.BackupRecord, error)
	InsertBackupRecord(ctx context.Context, rec repo.BackupRecord) (repo.BackupRecord, error)
	DeleteBackup(ctx context.Context, id int64) error
	RawDB() *sql.DB
}

var _ BackupStore = (*repo.Store)(nil)

// BackupService owns backup record lifecycle: creating consistent SQLite
// snapshots, listing/fetching/deleting records. Restore staging stays in the
// HTTP layer because it is pure file plumbing around the upload.
type BackupService struct {
	store     BackupStore
	dbPath    string
	backupDir string
}

func NewBackupService(store BackupStore, dbPath, backupDir string) *BackupService {
	return &BackupService{store: store, dbPath: dbPath, backupDir: backupDir}
}

func (s *BackupService) List(ctx context.Context, limit int) ([]repo.BackupRecord, error) {
	return s.store.ListBackups(ctx, limit)
}

func (s *BackupService) Get(ctx context.Context, id int64) (repo.BackupRecord, error) {
	return s.store.GetBackup(ctx, id)
}

// Create snapshots the live DB (VACUUM INTO + gzip) and inserts the record.
// On record-insert failure the file is removed so we never leak an orphan
// backup on disk.
func (s *BackupService) Create(ctx context.Context) (repo.BackupRecord, error) {
	res, err := backup.CreateSQLiteBackup(ctx, s.store.RawDB(), s.dbPath, s.backupDir)
	if err != nil {
		return repo.BackupRecord{}, err
	}
	rec := repo.BackupRecord{
		FilePath:  res.FilePath,
		SizeBytes: res.SizeBytes,
		SHA256:    res.SHA256,
		Storage:   "local",
	}
	stored, err := s.store.InsertBackupRecord(ctx, rec)
	if err != nil {
		_ = os.Remove(res.FilePath)
		return repo.BackupRecord{}, err
	}
	return stored, nil
}

// Delete removes the on-disk file (best-effort) and the DB row. Returns the
// record that was deleted so callers can audit it.
func (s *BackupService) Delete(ctx context.Context, id int64) (repo.BackupRecord, error) {
	rec, err := s.store.GetBackup(ctx, id)
	if err != nil {
		return repo.BackupRecord{}, err
	}
	if rec.Storage == "local" && rec.FilePath != "" {
		_ = os.Remove(rec.FilePath)
	}
	if err := s.store.DeleteBackup(ctx, id); err != nil {
		return repo.BackupRecord{}, err
	}
	return rec, nil
}
