package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type UsageBatch struct {
	Kind      string       `json:"kind"` // "stats" or "access"
	CreatedAt string       `json:"created_at"`
	Events    []UsageEvent `json:"events"`
	Stats     *StatsMeta   `json:"stats,omitempty"`
	Access    *AccessMeta  `json:"access,omitempty"`
}

type StatsMeta struct {
	NextAck map[string]StatEntry `json:"next_ack,omitempty"`
	Epoch   int64                `json:"epoch,omitempty"`
}

type AccessMeta struct {
	Path   string `json:"path,omitempty"`
	Inode  uint64 `json:"inode,omitempty"`
	Offset int64  `json:"offset,omitempty"`
}

type DiskQueue struct {
	dir      string
	maxBytes int64

	mu         sync.Mutex
	loadedSize bool
	total      int64
	files      int64
}

func NewDiskQueue(dir string, maxBytes int64) *DiskQueue {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "" {
		dir = "queue"
	}
	if maxBytes <= 0 {
		maxBytes = 200000000
	}
	return &DiskQueue{dir: dir, maxBytes: maxBytes}
}

func (q *DiskQueue) ensureDir() error {
	return os.MkdirAll(q.dir, 0700)
}

func (q *DiskQueue) loadStats() (int64, int64, error) {
	if err := q.ensureDir(); err != nil {
		return 0, 0, err
	}
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return 0, 0, err
	}
	var total int64
	var files int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
		files++
	}
	return total, files, nil
}

func (q *DiskQueue) ensureTotalLoadedLocked() error {
	if q.loadedSize {
		return nil
	}
	total, files, err := q.loadStats()
	if err != nil {
		return err
	}
	q.total = total
	q.files = files
	q.loadedSize = true
	return nil
}

func (q *DiskQueue) Enqueue(batch UsageBatch) error {
	if err := q.ensureDir(); err != nil {
		return err
	}
	// Estimate batch size (JSON bytes).
	b, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.ensureTotalLoadedLocked(); err != nil {
		return err
	}
	if q.total+int64(len(b)) > q.maxBytes {
		return fmt.Errorf("queue full")
	}
	ts := time.Now().UTC().Format("20060102T150405.000000000Z")
	tmp := filepath.Join(q.dir, fmt.Sprintf(".batch.%s.tmp", ts))
	final := filepath.Join(q.dir, fmt.Sprintf("batch.%s.json", ts))
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	q.total += int64(len(b))
	q.files++
	return nil
}

func (q *DiskQueue) PeekOldest() (string, UsageBatch, bool, error) {
	if err := q.ensureDir(); err != nil {
		return "", UsageBatch{}, false, err
	}
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return "", UsageBatch{}, false, err
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "batch.") && strings.HasSuffix(name, ".json") {
			files = append(files, filepath.Join(q.dir, name))
		}
	}
	if len(files) == 0 {
		return "", UsageBatch{}, false, nil
	}
	sort.Strings(files)
	path := files[0]
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", UsageBatch{}, false, err
	}
	var b UsageBatch
	if err := json.Unmarshal(raw, &b); err != nil {
		return "", UsageBatch{}, false, err
	}
	return path, b, true, nil
}

func (q *DiskQueue) Dequeue(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.loadedSize {
		if err := q.ensureTotalLoadedLocked(); err != nil {
			return err
		}
	}
	size := int64(0)
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	q.total -= size
	if q.total < 0 {
		q.total = 0
	}
	if q.files > 0 {
		q.files--
	}
	return nil
}

// ApproxBytes returns the cached queue byte size. On first call it scans the queue
// directory once to initialize cache.
func (q *DiskQueue) ApproxBytes() int64 {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.ensureTotalLoadedLocked(); err != nil {
		return 0
	}
	if q.total < 0 {
		return 0
	}
	return q.total
}

func (q *DiskQueue) ApproxBatches() int64 {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.ensureTotalLoadedLocked(); err != nil {
		return 0
	}
	if q.files < 0 {
		return 0
	}
	return q.files
}
