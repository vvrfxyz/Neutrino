package functional_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"neutrino/internal/app"
	"neutrino/internal/db"
)

// uploadBackup posts a multipart form with the given bytes as the "backup"
// field. Returns the response so the caller can assert on it.
func (e *testEnv) uploadBackup(t *testing.T, filename string, body []byte) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("backup", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, e.http.URL+"/api/v1/backups/restore", &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.SetBasicAuth(e.cfg.AdminUser, e.cfg.AdminPass)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("do upload: %v", err)
	}
	return resp
}

func TestFunctional_Backup_CreateListDownload(t *testing.T) {
	env := setupTestEnv(t)
	env.cfg.BackupDir = filepath.Join(t.TempDir(), "backups")

	resp := env.request(t, http.MethodPost, "/api/v1/backups", nil, true, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create backup: status=%d body=%s", resp.StatusCode, body)
	}
	var created struct {
		ID        int64  `json:"id"`
		FilePath  string `json:"file_path"`
		SizeBytes int64  `json:"size_bytes"`
		SHA256    string `json:"sha256"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.ID == 0 || created.FilePath == "" || created.SizeBytes == 0 || created.SHA256 == "" {
		t.Fatalf("unexpected created backup: %+v", created)
	}
	if _, err := os.Stat(created.FilePath); err != nil {
		t.Fatalf("backup file should exist: %v", err)
	}

	listResp := env.request(t, http.MethodGet, "/api/v1/backups", nil, true, "")
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d", listResp.StatusCode)
	}
	var list struct {
		Count int `json:"count"`
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Count != 1 || len(list.Items) != 1 || list.Items[0].ID != created.ID {
		t.Fatalf("unexpected list: %+v", list)
	}

	dlResp := env.request(t, http.MethodGet, "/api/v1/backups/"+strconv.FormatInt(created.ID, 10)+"/download", nil, true, "")
	defer dlResp.Body.Close()
	if dlResp.StatusCode != http.StatusOK {
		t.Fatalf("download status=%d", dlResp.StatusCode)
	}
	if ct := dlResp.Header.Get("Content-Type"); ct != "application/gzip" {
		t.Fatalf("download content-type=%q", ct)
	}
	dlBytes, err := io.ReadAll(dlResp.Body)
	if err != nil {
		t.Fatalf("read download: %v", err)
	}
	if int64(len(dlBytes)) != created.SizeBytes {
		t.Fatalf("download bytes=%d, want %d", len(dlBytes), created.SizeBytes)
	}

	delResp := env.request(t, http.MethodDelete, "/api/v1/backups/"+strconv.FormatInt(created.ID, 10), nil, true, "")
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status=%d", delResp.StatusCode)
	}
	if _, err := os.Stat(created.FilePath); !os.IsNotExist(err) {
		t.Fatalf("backup file should be removed after delete, got err=%v", err)
	}

	// Backup operations are sensitive (full-DB export incl. credentials), so
	// each lifecycle step must leave an audit trail.
	for _, action := range []string{"backup.create", "backup.download", "backup.delete"} {
		var count int
		if err := env.store.RawDB().QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM audit_logs WHERE action = ?`, action).Scan(&count); err != nil {
			t.Fatalf("count audit %s: %v", action, err)
		}
		if count != 1 {
			t.Fatalf("audit action %s count=%d, want 1", action, count)
		}
	}
}

func TestFunctional_Backup_RestoreStagesValidUpload(t *testing.T) {
	env := setupTestEnv(t)
	env.cfg.BackupDir = filepath.Join(t.TempDir(), "backups")

	// Build a known-good Neutrino DB on disk so we can upload it for restore.
	srcPath := filepath.Join(t.TempDir(), "source.db")
	srcConn, err := db.Open(srcPath)
	if err != nil {
		t.Fatalf("open source db: %v", err)
	}
	if err := db.Migrate(srcConn); err != nil {
		t.Fatalf("migrate source db: %v", err)
	}
	if err := srcConn.Close(); err != nil {
		t.Fatalf("close source db: %v", err)
	}
	srcBytes, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read source db: %v", err)
	}

	t.Run("plain sqlite", func(t *testing.T) {
		resp := env.uploadBackup(t, "neutrino.sqlite", srcBytes)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("restore status=%d body=%s", resp.StatusCode, body)
		}
		pending := env.cfg.DBPath + ".pending-restore"
		if _, err := os.Stat(pending); err != nil {
			t.Fatalf("pending file should exist: %v", err)
		}
		_ = os.Remove(pending) // reset for next subtest
	})

	t.Run("gzipped sqlite", func(t *testing.T) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(srcBytes); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
		resp := env.uploadBackup(t, "neutrino.sqlite.gz", buf.Bytes())
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("restore status=%d body=%s", resp.StatusCode, body)
		}
		if _, err := os.Stat(env.cfg.DBPath + ".pending-restore"); err != nil {
			t.Fatalf("pending file should exist: %v", err)
		}
	})
}

func TestFunctional_Backup_RestoreRejectsCorrupt(t *testing.T) {
	env := setupTestEnv(t)

	// Random bytes — definitely not a sqlite header.
	resp := env.uploadBackup(t, "garbage.sqlite", []byte("this is not a sqlite database"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got status=%d body=%s", resp.StatusCode, body)
	}
	if _, err := os.Stat(env.cfg.DBPath + ".pending-restore"); !os.IsNotExist(err) {
		t.Fatalf("no pending file should be staged on rejection, err=%v", err)
	}
}

func TestFunctional_Backup_RestoreRejectsUnrelatedSQLite(t *testing.T) {
	env := setupTestEnv(t)

	// Real sqlite DB but with no Neutrino tables.
	otherPath := filepath.Join(t.TempDir(), "other.db")
	otherConn, err := db.Open(otherPath)
	if err != nil {
		t.Fatalf("open other db: %v", err)
	}
	if _, err := otherConn.ExecContext(context.Background(), `CREATE TABLE foo(x INTEGER);`); err != nil {
		t.Fatalf("create foo: %v", err)
	}
	if err := otherConn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	otherBytes, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatalf("read other db: %v", err)
	}

	resp := env.uploadBackup(t, "other.sqlite", otherBytes)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for unrelated sqlite, got status=%d body=%s", resp.StatusCode, body)
	}
}

func TestApplyPendingRestoreSwapsValidFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "active.db")

	// Seed a current "active" DB so the snapshot path exercises the rename.
	curConn, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open current: %v", err)
	}
	if err := db.Migrate(curConn); err != nil {
		t.Fatalf("migrate current: %v", err)
	}
	if err := curConn.Close(); err != nil {
		t.Fatalf("close current: %v", err)
	}

	// Build a different valid DB and write it as the pending file.
	pendingSrc := filepath.Join(dir, "src.db")
	srcConn, err := db.Open(pendingSrc)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	if err := db.Migrate(srcConn); err != nil {
		t.Fatalf("migrate src: %v", err)
	}
	if err := srcConn.Close(); err != nil {
		t.Fatalf("close src: %v", err)
	}
	srcBytes, err := os.ReadFile(pendingSrc)
	if err != nil {
		t.Fatalf("read src: %v", err)
	}
	pendingPath := dbPath + ".pending-restore"
	if err := os.WriteFile(pendingPath, srcBytes, 0o600); err != nil {
		t.Fatalf("write pending: %v", err)
	}

	applied, err := app.ApplyPendingRestore(dbPath, func() string { return "20260101T000000Z" })
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !applied {
		t.Fatalf("expected applied=true")
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("pending file should be consumed, err=%v", err)
	}
	bak := dbPath + ".pre-restore-20260101T000000Z.bak"
	if _, err := os.Stat(bak); err != nil {
		t.Fatalf("snapshot bak should exist: %v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("active db should be in place: %v", err)
	}
}

func TestApplyPendingRestoreRejectsInvalidPending(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "active.db")
	if err := os.WriteFile(dbPath+".pending-restore", []byte("garbage"), 0o600); err != nil {
		t.Fatalf("write pending: %v", err)
	}

	applied, err := app.ApplyPendingRestore(dbPath, func() string { return "ts" })
	if err == nil {
		t.Fatalf("expected error for invalid pending")
	}
	if applied {
		t.Fatalf("expected applied=false")
	}
	// The invalid pending must be left on disk so an operator can inspect it.
	if _, err := os.Stat(dbPath + ".pending-restore"); err != nil {
		t.Fatalf("pending should remain on disk for inspection: %v", err)
	}
}
