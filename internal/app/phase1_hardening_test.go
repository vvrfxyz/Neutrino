package app

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRateLimiterEvictsExpiredWindows(t *testing.T) {
	t.Parallel()

	rl := newRateLimiter()
	base := time.Now()
	for i := 0; i < 1000; i++ {
		key := "ip:" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + time.Duration(i).String()
		rl.Allow(key, 10, time.Minute, base)
	}
	rl.mu.Lock()
	before := len(rl.m)
	rl.mu.Unlock()
	if before < 900 {
		t.Fatalf("setup: expected ~1000 windows, got %d", before)
	}

	// After the idle horizon passes, a single Allow call sweeps the map.
	later := base.Add(rateLimiterMaxIdle + rateLimiterSweepEvery + time.Second)
	rl.Allow("fresh", 10, time.Minute, later)
	rl.mu.Lock()
	after := len(rl.m)
	rl.mu.Unlock()
	if after > 2 {
		t.Fatalf("expected expired windows evicted, still %d entries", after)
	}
}

func TestRateLimiterStillEnforcesLimits(t *testing.T) {
	t.Parallel()

	rl := newRateLimiter()
	now := time.Now()
	for i := 0; i < 5; i++ {
		if !rl.Allow("k", 5, time.Minute, now) {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	if rl.Allow("k", 5, time.Minute, now) {
		t.Fatalf("6th request should be denied")
	}
	if !rl.Allow("k", 5, time.Minute, now.Add(time.Minute)) {
		t.Fatalf("new window should be allowed")
	}
}

func TestMaterializeUploadedDBRejectsOversizedDecompression(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bomb := filepath.Join(dir, "bomb.gz")
	f, err := os.Create(bomb)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	zw := gzip.NewWriter(f)
	payload := make([]byte, 1<<20)
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	if _, err := materializeUploadedDB(bomb, dir, 1<<19); err == nil {
		t.Fatalf("expected oversized decompression to be rejected")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unexpected error: %v", err)
	}
	// No stray candidate files may remain.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "candidate") {
			t.Fatalf("stray candidate file left behind: %s", e.Name())
		}
	}

	// Under the cap it must succeed.
	out, err := materializeUploadedDB(bomb, dir, 1<<21)
	if err != nil {
		t.Fatalf("expected within-cap decompression to succeed: %v", err)
	}
	defer os.Remove(out)
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 1<<20 {
		t.Fatalf("decompressed size = %d, want %d", info.Size(), 1<<20)
	}
}
