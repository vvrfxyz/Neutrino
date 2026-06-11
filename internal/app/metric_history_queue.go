package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"neutrino/internal/repo"
)

type nodeMetricHistoryItem struct {
	nodeID    int64
	sampledAt time.Time
	metrics   repo.NodeReportMetricsInput
}

type nodeMetricHistoryDiskRecord struct {
	NodeID    int64                       `json:"node_id"`
	SampledAt string                      `json:"sampled_at"`
	Metrics   repo.NodeReportMetricsInput `json:"metrics"`
}

type nodeMetricHistoryQueue struct {
	store      *repo.Store
	ch         chan nodeMetricHistoryItem
	disk       *nodeMetricHistoryDiskQueue
	dropped    atomic.Int64
	alerted    atomic.Int64
	batchSize  int
	flushEvery time.Duration
}

func newNodeMetricHistoryQueue(store *repo.Store, capacity int) *nodeMetricHistoryQueue {
	if capacity < 0 {
		capacity = 0
	}
	return &nodeMetricHistoryQueue{
		store:      store,
		ch:         make(chan nodeMetricHistoryItem, capacity),
		batchSize:  64,
		flushEvery: time.Second,
	}
}

func newNodeMetricHistoryQueueWithDisk(store *repo.Store, capacity int, diskDir string, maxBytes int64) *nodeMetricHistoryQueue {
	q := newNodeMetricHistoryQueue(store, capacity)
	diskDir = strings.TrimSpace(diskDir)
	if diskDir != "" && maxBytes > 0 {
		q.disk = newNodeMetricHistoryDiskQueue(diskDir, maxBytes)
	}
	return q
}

func (q *nodeMetricHistoryQueue) Enqueue(nodeID int64, sampledAt time.Time, metrics repo.NodeReportMetricsInput) bool {
	if q == nil || q.store == nil || nodeID <= 0 {
		return false
	}
	item := nodeMetricHistoryItem{nodeID: nodeID, sampledAt: sampledAt, metrics: metrics}
	select {
	case q.ch <- item:
		return true
	default:
		if q.disk != nil {
			if err := q.disk.Enqueue(item); err == nil {
				return true
			}
		}
		q.dropped.Add(1)
		return false
	}
}

func (q *nodeMetricHistoryQueue) Dropped() int64 {
	if q == nil {
		return 0
	}
	return q.dropped.Load()
}

func (q *nodeMetricHistoryQueue) Run(ctx context.Context) {
	if q == nil || q.store == nil {
		return
	}
	ticker := time.NewTicker(q.flushEvery)
	defer ticker.Stop()
	batch := make([]nodeMetricHistoryItem, 0, q.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := q.flushBatch(ctx, batch); err != nil {
			log.Printf("node metric history batch write completed with errors items=%d: %v", len(batch), err)
		}
		batch = batch[:0]
	}
	q.flushDiskBacklog(ctx)
	for {
		select {
		case <-ctx.Done():
			if len(batch) > 0 {
				if err := q.flushBatch(context.Background(), batch); err != nil {
					log.Printf("node metric history shutdown write completed with errors items=%d: %v", len(batch), err)
				}
				batch = batch[:0]
			}
			q.flushRemaining(context.Background())
			q.flushDiskBacklog(context.Background())
			q.syncDroppedAlert(context.Background())
			return
		case item := <-q.ch:
			batch = append(batch, item)
			if len(batch) >= q.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
			q.flushDiskBacklog(ctx)
			q.syncDroppedAlert(ctx)
		}
	}
}

func (q *nodeMetricHistoryQueue) flushRemaining(ctx context.Context) {
	if q == nil {
		return
	}
	batch := make([]nodeMetricHistoryItem, 0, q.batchSize)
	for {
		select {
		case item := <-q.ch:
			batch = append(batch, item)
			if len(batch) >= q.batchSize {
				if err := q.flushBatch(ctx, batch); err != nil {
					log.Printf("node metric history remaining write completed with errors items=%d: %v", len(batch), err)
				}
				batch = batch[:0]
			}
		default:
			if len(batch) > 0 {
				if err := q.flushBatch(ctx, batch); err != nil {
					log.Printf("node metric history remaining write completed with errors items=%d: %v", len(batch), err)
				}
			}
			return
		}
	}
}

func (q *nodeMetricHistoryQueue) flushBatch(ctx context.Context, batch []nodeMetricHistoryItem) error {
	items := make([]repo.NodeMetricHistoryInput, 0, len(batch))
	for _, item := range batch {
		items = append(items, repo.NodeMetricHistoryInput{
			NodeID:    item.nodeID,
			SampledAt: item.sampledAt,
			Metrics:   item.metrics,
		})
	}
	if len(items) == 0 {
		return nil
	}
	return q.store.InsertNodeMetricHistoryBatch(ctx, items)
}

func (q *nodeMetricHistoryQueue) flushDiskBacklog(ctx context.Context) {
	if q == nil || q.disk == nil || q.store == nil {
		return
	}
	if q.batchSize <= 0 {
		q.batchSize = 64
	}
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		paths, err := q.disk.oldestPaths(q.batchSize)
		if err != nil {
			log.Printf("node metric history disk queue scan failed: %v", err)
			return
		}
		if len(paths) == 0 {
			return
		}
		retained := false
		for _, path := range paths {
			item, err := q.disk.read(path)
			if err != nil {
				log.Printf("node metric history disk queue item discarded path=%s: %v", path, err)
				if removeErr := q.disk.Dequeue(path); removeErr != nil {
					log.Printf("node metric history disk queue discard failed path=%s: %v", path, removeErr)
				}
				continue
			}
			if err := q.flushBatch(ctx, []nodeMetricHistoryItem{item}); err != nil {
				log.Printf("node metric history disk backlog write failed path=%s: %v", path, err)
				if nodeMetricHistoryErrorMayHaveCommitted(err) {
					if removeErr := q.disk.Dequeue(path); removeErr != nil {
						log.Printf("node metric history disk queue dequeue failed path=%s: %v", path, removeErr)
						return
					}
					continue
				}
				retained = true
				continue
			}
			if err := q.disk.Dequeue(path); err != nil {
				log.Printf("node metric history disk queue dequeue failed path=%s: %v", path, err)
				return
			}
		}
		if retained || len(paths) < q.batchSize {
			return
		}
	}
}

// nodeMetricHistoryErrorMayHaveCommitted reports whether a flush error came
// from a transaction that nevertheless committed (per-item failures). Such
// items must be dequeued, not retried — retrying would re-insert the items
// that succeeded. Detected via the repo's typed sentinel; the previous
// substring heuristic ("details node_id=") missed sample-only failures, which
// also commit.
func nodeMetricHistoryErrorMayHaveCommitted(err error) bool {
	return errors.Is(err, repo.ErrBatchCommittedWithItemErrors)
}

func (q *nodeMetricHistoryQueue) syncDroppedAlert(ctx context.Context) {
	if q == nil || q.store == nil {
		return
	}
	dropped := q.Dropped()
	if dropped == 0 || dropped <= q.alerted.Load() {
		return
	}
	message := fmt.Sprintf("node metric history queue full; dropped=%d", dropped)
	if _, err := q.store.UpsertOpsAlert(ctx, nil, "metric_history_dropped", "warning", message, "metric-history-dropped", time.Now().UTC()); err != nil {
		log.Printf("node metric history drop alert failed: %v", err)
		return
	}
	q.alerted.Store(dropped)
}

type nodeMetricHistoryDiskQueue struct {
	dir      string
	maxBytes int64

	mu     sync.Mutex
	loaded bool
	total  int64
	files  int64
}

func newNodeMetricHistoryDiskQueue(dir string, maxBytes int64) *nodeMetricHistoryDiskQueue {
	return &nodeMetricHistoryDiskQueue{
		dir:      filepath.Clean(strings.TrimSpace(dir)),
		maxBytes: maxBytes,
	}
}

func (q *nodeMetricHistoryDiskQueue) ensureDir() error {
	if q == nil || q.dir == "" {
		return fmt.Errorf("disk queue disabled")
	}
	return os.MkdirAll(q.dir, 0700)
}

func (q *nodeMetricHistoryDiskQueue) loadStats() (int64, int64, error) {
	if err := q.ensureDir(); err != nil {
		return 0, 0, err
	}
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return 0, 0, err
	}
	var total int64
	var files int64
	for _, entry := range entries {
		if entry.IsDir() || !isNodeMetricHistoryQueueFile(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		total += info.Size()
		files++
	}
	return total, files, nil
}

func (q *nodeMetricHistoryDiskQueue) ensureTotalLoadedLocked() error {
	if q.loaded {
		return nil
	}
	total, files, err := q.loadStats()
	if err != nil {
		return err
	}
	q.total = total
	q.files = files
	q.loaded = true
	return nil
}

func (q *nodeMetricHistoryDiskQueue) Enqueue(item nodeMetricHistoryItem) error {
	if q == nil {
		return fmt.Errorf("disk queue disabled")
	}
	record := nodeMetricHistoryDiskRecord{
		NodeID:    item.nodeID,
		SampledAt: item.sampledAt.UTC().Format(time.RFC3339Nano),
		Metrics:   item.metrics,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.ensureTotalLoadedLocked(); err != nil {
		return err
	}
	if q.maxBytes <= 0 || q.total+int64(len(raw)) > q.maxBytes {
		return fmt.Errorf("disk queue full")
	}
	tmp, err := os.CreateTemp(q.dir, ".metric-history-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	ts := time.Now().UTC().Format("20060102T150405.000000000Z")
	suffix := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(tmpPath), ".metric-history-"), ".tmp")
	finalPath := filepath.Join(q.dir, fmt.Sprintf("metric-history.%s.%s.json", ts, suffix))
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return err
	}
	cleanup = false
	q.total += int64(len(raw))
	q.files++
	return nil
}

func (q *nodeMetricHistoryDiskQueue) oldestPaths(limit int) ([]string, error) {
	if q == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 64
	}
	if err := q.ensureDir(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !isNodeMetricHistoryQueueFile(entry.Name()) {
			continue
		}
		paths = append(paths, filepath.Join(q.dir, entry.Name()))
	}
	sort.Strings(paths)
	if len(paths) > limit {
		paths = paths[:limit]
	}
	return paths, nil
}

func (q *nodeMetricHistoryDiskQueue) read(path string) (nodeMetricHistoryItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nodeMetricHistoryItem{}, err
	}
	var record nodeMetricHistoryDiskRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nodeMetricHistoryItem{}, err
	}
	sampledAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(record.SampledAt))
	if err != nil {
		return nodeMetricHistoryItem{}, err
	}
	if record.NodeID <= 0 {
		return nodeMetricHistoryItem{}, fmt.Errorf("invalid node id")
	}
	return nodeMetricHistoryItem{
		nodeID:    record.NodeID,
		sampledAt: sampledAt.UTC(),
		metrics:   record.Metrics,
	}, nil
}

func (q *nodeMetricHistoryDiskQueue) Dequeue(path string) error {
	if q == nil {
		return nil
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.ensureTotalLoadedLocked(); err != nil {
		return err
	}
	var size int64
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

func (q *nodeMetricHistoryDiskQueue) ApproxBytes() int64 {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.ensureTotalLoadedLocked(); err != nil {
		return 0
	}
	return q.total
}

func (q *nodeMetricHistoryDiskQueue) ApproxFiles() int64 {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.ensureTotalLoadedLocked(); err != nil {
		return 0
	}
	return q.files
}

func isNodeMetricHistoryQueueFile(name string) bool {
	return strings.HasPrefix(name, "metric-history.") && strings.HasSuffix(name, ".json")
}
