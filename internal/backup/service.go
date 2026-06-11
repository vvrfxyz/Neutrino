package backup

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Result struct {
	FilePath  string
	SizeBytes int64
	SHA256    string
}

func quoteSQLitePath(path string) string {
	return strings.ReplaceAll(path, "'", "''")
}

func CreateSQLiteBackup(ctx context.Context, db *sql.DB, dbPath, backupDir string) (Result, error) {
	if strings.TrimSpace(backupDir) == "" {
		backupDir = "backups"
	}
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return Result{}, err
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	rawPath := filepath.Join(backupDir, "neutrino-"+ts+".sqlite")
	gzPath := rawPath + ".gz"

	if _, err := db.ExecContext(ctx, fmt.Sprintf("VACUUM INTO '%s';", quoteSQLitePath(rawPath))); err != nil {
		return Result{}, err
	}
	defer os.Remove(rawPath)

	in, err := os.Open(rawPath)
	if err != nil {
		return Result{}, err
	}
	defer in.Close()

	out, err := os.Create(gzPath)
	if err != nil {
		return Result{}, err
	}
	h := sha256.New()
	mw := io.MultiWriter(out, h)
	zw := gzip.NewWriter(mw)
	if _, err := io.Copy(zw, in); err != nil {
		_ = zw.Close()
		_ = out.Close()
		return Result{}, err
	}
	if err := zw.Close(); err != nil {
		_ = out.Close()
		return Result{}, err
	}
	if err := out.Close(); err != nil {
		return Result{}, err
	}
	info, err := os.Stat(gzPath)
	if err != nil {
		return Result{}, err
	}
	return Result{FilePath: gzPath, SizeBytes: info.Size(), SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
