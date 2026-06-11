package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDiskQueueQuarantineMovesBatchAndAdjustsCounters(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	q := NewDiskQueue(dir, 0)
	if err := q.Enqueue(UsageBatch{Kind: "stats", Events: []UsageEvent{{UserID: 1, Bytes: 1, Source: "test", SourceEventID: "a"}}}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := q.Enqueue(UsageBatch{Kind: "stats", Events: []UsageEvent{{UserID: 2, Bytes: 2, Source: "test", SourceEventID: "b"}}}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if got := q.ApproxBatches(); got != 2 {
		t.Fatalf("ApproxBatches = %d, want 2", got)
	}

	path, _, ok, err := q.PeekOldest()
	if err != nil || !ok {
		t.Fatalf("peek: ok=%v err=%v", ok, err)
	}
	dest, err := q.Quarantine(path)
	if err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if filepath.Dir(dest) != filepath.Join(dir, quarantineDirName) {
		t.Fatalf("quarantined to %s, want under %s", dest, filepath.Join(dir, quarantineDirName))
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("quarantined file missing: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("original batch should be gone, stat err=%v", err)
	}
	if got := q.ApproxBatches(); got != 1 {
		t.Fatalf("ApproxBatches after quarantine = %d, want 1", got)
	}
	if got := q.QuarantinedBatches(); got != 1 {
		t.Fatalf("QuarantinedBatches = %d, want 1", got)
	}

	// The remaining batch must now be the queue head.
	path2, b2, ok, err := q.PeekOldest()
	if err != nil || !ok {
		t.Fatalf("peek after quarantine: ok=%v err=%v", ok, err)
	}
	if path2 == path {
		t.Fatalf("queue head did not advance")
	}
	if b2.Events[0].SourceEventID != "b" {
		t.Fatalf("unexpected head batch: %+v", b2)
	}
}

func TestDiskQueuePeekOldestFlagsCorruptBatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	q := NewDiskQueue(dir, 0)
	corrupt := filepath.Join(dir, "batch.20260101T000000.000000000Z.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0600); err != nil {
		t.Fatalf("write corrupt batch: %v", err)
	}

	path, _, ok, err := q.PeekOldest()
	if ok {
		t.Fatalf("peek should not return ok for corrupt batch")
	}
	if !errors.Is(err, ErrBatchCorrupt) {
		t.Fatalf("expected ErrBatchCorrupt, got %v", err)
	}
	if path != corrupt {
		t.Fatalf("corrupt path = %q, want %q", path, corrupt)
	}
}

func TestQuarantinedBatchesCountsExistingFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	qdir := filepath.Join(dir, quarantineDirName)
	if err := os.MkdirAll(qdir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"batch.a.json", "batch.b.json", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(qdir, name), []byte("{}"), 0600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	q := NewDiskQueue(dir, 0)
	if got := q.QuarantinedBatches(); got != 2 {
		t.Fatalf("QuarantinedBatches = %d, want 2", got)
	}
}
