package app

import (
	"compress/gzip"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

const (
	maxBackupUploadBytes = 500 << 20
	// maxBackupRestoreBytes caps the decompressed size of an uploaded backup.
	// Without it a gzip bomb (~1032:1) inside the 500MB upload cap could write
	// hundreds of GB next to the live DB and fill the panel disk.
	maxBackupRestoreBytes = 4 << 30
	pendingRestoreSuffix  = ".pending-restore"
)

// requiredRestoreTables are sanity-checked on uploaded SQLite candidates so that we
// reject unrelated databases (e.g. a random sqlite file) before staging them as
// pending-restore. Schema migrations may add tables; this list is intentionally
// limited to ones that have always existed in Neutrino.
var requiredRestoreTables = []string{"users", "nodes", "admin_accounts"}

func (a *App) handleAPIBackupsV1(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.handleAPIListBackups(w, r)
	case http.MethodPost:
		a.handleAPICreateBackup(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *App) handleAPIBackupRoutesV1(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/backups/"), "/")
	if rest == "" {
		http.NotFound(w, r)
		return
	}
	if rest == "restore" {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleAPIRestoreBackup(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	id, err := parseInt64Path(parts[0])
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		a.handleAPIGetBackup(w, r, id)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		a.handleAPIDeleteBackup(w, r, id)
	case len(parts) == 2 && parts[1] == "download" && r.Method == http.MethodGet:
		a.handleAPIDownloadBackup(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (a *App) handleAPIListBackups(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	recs, err := a.backups().List(r.Context(), limit)
	if err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "list failed"})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"count": len(recs), "items": recs})
}

func (a *App) handleAPICreateBackup(w http.ResponseWriter, r *http.Request) {
	stored, err := a.backups().Create(r.Context())
	if err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "create backup failed: " + err.Error()})
		return
	}
	auditAction(a, r, "backup.create", "backup", strconv.FormatInt(stored.ID, 10), map[string]any{
		"file":       filepath.Base(stored.FilePath),
		"size_bytes": stored.SizeBytes,
		"sha256":     stored.SHA256,
	})
	a.writeJSON(w, http.StatusCreated, stored)
}

func (a *App) handleAPIGetBackup(w http.ResponseWriter, r *http.Request, id int64) {
	rec, err := a.backups().Get(r.Context(), id)
	if err != nil {
		a.writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	a.writeJSON(w, http.StatusOK, rec)
}

func (a *App) handleAPIDeleteBackup(w http.ResponseWriter, r *http.Request, id int64) {
	rec, err := a.backups().Delete(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			a.writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		a.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "delete failed"})
		return
	}
	auditAction(a, r, "backup.delete", "backup", strconv.FormatInt(id, 10), map[string]any{
		"file": filepath.Base(rec.FilePath),
	})
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleAPIDownloadBackup(w http.ResponseWriter, r *http.Request, id int64) {
	rec, err := a.backups().Get(r.Context(), id)
	if err != nil {
		a.writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if rec.Storage != "local" || rec.FilePath == "" {
		a.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "not a local backup"})
		return
	}
	f, err := os.Open(rec.FilePath)
	if err != nil {
		a.writeJSON(w, http.StatusGone, map[string]any{"error": "backup file missing on disk"})
		return
	}
	defer f.Close()
	stat, _ := f.Stat()
	// Downloading a backup exports the entire DB (credentials included), so it
	// is the most sensitive read in the panel — always audit it.
	auditAction(a, r, "backup.download", "backup", strconv.FormatInt(id, 10), map[string]any{
		"file":       filepath.Base(rec.FilePath),
		"size_bytes": rec.SizeBytes,
	})
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(rec.FilePath)+`"`)
	if stat != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	}
	_, _ = io.Copy(w, f)
}

func (a *App) handleAPIRestoreBackup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBackupUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "parse multipart failed: " + err.Error()})
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	file, hdr, err := r.FormFile("backup")
	if err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing 'backup' form field"})
		return
	}
	defer file.Close()

	stagingDir := filepath.Dir(a.cfg.DBPath)
	if stagingDir == "" || stagingDir == "." {
		stagingDir = "."
	}

	rawTmp, err := os.CreateTemp(stagingDir, "restore-*.upload")
	if err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "stage upload failed"})
		return
	}
	rawTmpPath := rawTmp.Name()
	defer os.Remove(rawTmpPath)
	if _, err := io.Copy(rawTmp, file); err != nil {
		_ = rawTmp.Close()
		a.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "upload failed: " + err.Error()})
		return
	}
	if err := rawTmp.Close(); err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "upload close failed"})
		return
	}

	candidate, err := materializeUploadedDB(rawTmpPath, stagingDir, maxBackupRestoreBytes)
	if err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "decompress failed: " + err.Error()})
		return
	}
	defer os.Remove(candidate)

	if err := validateSQLiteCandidate(candidate); err != nil {
		a.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "validation failed: " + err.Error()})
		return
	}

	pending := a.cfg.DBPath + pendingRestoreSuffix
	if err := os.Rename(candidate, pending); err != nil {
		a.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "stage pending failed: " + err.Error()})
		return
	}

	// Staging a restore queues a full DB replacement for the next restart.
	auditAction(a, r, "backup.restore.stage", "backup", "", map[string]any{
		"filename":   hdr.Filename,
		"size_bytes": fileSize(pending),
	})

	a.writeJSON(w, http.StatusAccepted, map[string]any{
		"status":   "staged",
		"pending":  pending,
		"size":     fileSize(pending),
		"filename": hdr.Filename,
		"message":  "panel restart required to apply restore",
	})
}

// materializeUploadedDB reads rawPath, detects gzip magic, and returns the path
// of the materialized .sqlite file (gunzipped if needed, refusing decompressed
// output larger than maxDecompressed). The caller owns the returned file and is
// expected to remove it.
func materializeUploadedDB(rawPath, stagingDir string, maxDecompressed int64) (string, error) {
	in, err := os.Open(rawPath)
	if err != nil {
		return "", err
	}
	defer in.Close()
	magic := make([]byte, 2)
	n, err := io.ReadFull(in, magic)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("read magic: %w", err)
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	out, err := os.CreateTemp(stagingDir, "restore-*.candidate")
	if err != nil {
		return "", err
	}
	outPath := out.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(outPath)
		}
	}()

	if n == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		var zr *gzip.Reader
		zr, err = gzip.NewReader(in)
		if err != nil {
			_ = out.Close()
			return "", fmt.Errorf("gzip: %w", err)
		}
		var copied int64
		copied, err = io.Copy(out, io.LimitReader(zr, maxDecompressed+1))
		_ = zr.Close()
		if err != nil {
			_ = out.Close()
			return "", fmt.Errorf("gunzip: %w", err)
		}
		if copied > maxDecompressed {
			_ = out.Close()
			err = fmt.Errorf("decompressed backup exceeds %d bytes", maxDecompressed)
			return "", err
		}
	} else {
		if _, err = io.Copy(out, in); err != nil {
			_ = out.Close()
			return "", err
		}
	}
	if err = out.Close(); err != nil {
		return "", err
	}
	return outPath, nil
}

// validateSQLiteCandidate opens the file as a read-only SQLite database and
// verifies it passes PRAGMA integrity_check and contains the tables expected
// for a Neutrino backup.
func validateSQLiteCandidate(path string) error {
	conn, err := sql.Open("sqlite3", "file:"+path+"?mode=ro&_busy_timeout=2000")
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer conn.Close()
	var result string
	if err := conn.QueryRow(`PRAGMA integrity_check;`).Scan(&result); err != nil {
		return fmt.Errorf("integrity_check: %w", err)
	}
	if !strings.EqualFold(result, "ok") {
		return fmt.Errorf("integrity_check returned %q", result)
	}
	for _, tbl := range requiredRestoreTables {
		var found int
		if err := conn.QueryRow(
			`SELECT 1 FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("missing required table %q", tbl)
			}
			return fmt.Errorf("inspect %q: %w", tbl, err)
		}
	}
	return nil
}

// ApplyPendingRestore swaps in a staged DB file at startup, snapshotting the
// current DB to a timestamped .pre-restore-<ts>.bak so an admin can revert by
// hand if needed. Returns true if a swap was performed.
func ApplyPendingRestore(dbPath string, now func() string) (bool, error) {
	pending := dbPath + pendingRestoreSuffix
	if _, err := os.Stat(pending); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if err := validateSQLiteCandidate(pending); err != nil {
		return false, fmt.Errorf("pending restore is invalid (kept on disk): %w", err)
	}
	if _, err := os.Stat(dbPath); err == nil {
		ts := "now"
		if now != nil {
			ts = now()
		}
		bak := dbPath + ".pre-restore-" + ts + ".bak"
		if err := os.Rename(dbPath, bak); err != nil {
			return false, fmt.Errorf("snapshot current db: %w", err)
		}
		// Best-effort: move WAL/SHM out of the way too. SQLite recreates them.
		for _, suf := range []string{"-wal", "-shm"} {
			_ = os.Rename(dbPath+suf, bak+suf)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("stat current db: %w", err)
	}
	if err := os.Rename(pending, dbPath); err != nil {
		return false, fmt.Errorf("activate pending: %w", err)
	}
	return true, nil
}

func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}
