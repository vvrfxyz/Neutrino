package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func newFlushTestAgent(t *testing.T, dir string, panelHandler http.HandlerFunc) (*Agent, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(panelHandler)
	t.Cleanup(srv.Close)
	st := NewStateStore(filepath.Join(dir, "state.json"))
	a := &Agent{
		cfg:   Config{UsageQuarantineAfterRejects: 3},
		state: st,
		queue: NewDiskQueue(filepath.Join(dir, "queue"), 0),
	}
	a.panel = &PanelClient{baseURL: srv.URL, client: srv.Client()}
	return a, srv
}

func TestFlushQuarantinesBatchAfterRepeatedPanelRejections(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	a, _ := newFlushTestAgent(t, dir, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A rejection the agent does not treat as permanent-per-event: the
		// batch is rejected as a whole and will be rejected forever.
		_, _ = w.Write([]byte(`{"processed":1,"results":[{"user_id":1,"error":"event outside current quota window"}]}`))
	})
	if err := a.queue.Enqueue(UsageBatch{
		Kind:   "stats",
		Events: []UsageEvent{{UserID: 1, Bytes: 1, Source: "test", SourceEventID: "poison"}},
		Stats: &StatsMeta{
			NextAck: map[string]StatEntry{"poison@example.com": {Uplink: 111, Downlink: 222}},
			Epoch:   7,
		},
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := a.queue.Enqueue(UsageBatch{Kind: "stats", Events: []UsageEvent{{UserID: 2, Bytes: 1, Source: "test", SourceEventID: "good"}}}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	ctx := context.Background()
	// First two rejections: batch stays at the head, loop stops each tick.
	for i := 0; i < 2; i++ {
		advanced, keepGoing := a.flushQueuedBatchOnce(ctx)
		if advanced || keepGoing {
			t.Fatalf("attempt %d: advanced=%v keepGoing=%v, want false/false", i+1, advanced, keepGoing)
		}
		if got := a.queue.QuarantinedBatches(); got != 0 {
			t.Fatalf("attempt %d: quarantined %d batches early", i+1, got)
		}
	}
	// Third rejection reaches the threshold: the batch is quarantined and the
	// loop is told to continue with the next batch.
	advanced, keepGoing := a.flushQueuedBatchOnce(ctx)
	if !advanced || !keepGoing {
		t.Fatalf("threshold attempt: advanced=%v keepGoing=%v, want true/true", advanced, keepGoing)
	}
	if got := a.queue.QuarantinedBatches(); got != 1 {
		t.Fatalf("QuarantinedBatches = %d, want 1", got)
	}
	if got := a.queue.ApproxBatches(); got != 1 {
		t.Fatalf("ApproxBatches = %d, want 1 (the non-poison batch)", got)
	}
	// The panel commits acceptable events of a rejected batch before reporting
	// per-event errors, so quarantine must advance acks like a success —
	// otherwise the next sample re-emits the same ranges under fresh event ids
	// and the panel double-counts.
	st := a.state.Snapshot()
	if got := st.AckedStats["poison@example.com"]; got.Uplink != 111 || got.Downlink != 222 {
		t.Fatalf("acks not committed on quarantine: %+v", st.AckedStats)
	}
	if st.StatsEpoch != 7 {
		t.Fatalf("epoch not committed on quarantine: %d", st.StatsEpoch)
	}
}

func TestFlushDoesNotQuarantineOnTransientPanelRejections(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	a, _ := newFlushTestAgent(t, dir, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// "record failed" covers transient panel-side failures (busy DB);
		// retrying the identical batch can succeed, so it must never count
		// toward quarantine no matter how often it repeats.
		_, _ = w.Write([]byte(`{"processed":1,"results":[{"user_id":1,"error":"record failed"}]}`))
	})
	if err := a.queue.Enqueue(UsageBatch{Kind: "stats", Events: []UsageEvent{{UserID: 1, Bytes: 1, Source: "test", SourceEventID: "a"}}}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	ctx := context.Background()
	for i := 0; i < 10; i++ {
		advanced, keepGoing := a.flushQueuedBatchOnce(ctx)
		if advanced || keepGoing {
			t.Fatalf("attempt %d: advanced=%v keepGoing=%v, want false/false", i+1, advanced, keepGoing)
		}
	}
	if got := a.queue.QuarantinedBatches(); got != 0 {
		t.Fatalf("transient rejections must never quarantine; quarantined=%d", got)
	}
}

func TestFlushQuarantineHonorsMinAge(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	a, _ := newFlushTestAgent(t, dir, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"processed":1,"results":[{"user_id":1,"error":"event outside current quota window"}]}`))
	})
	a.cfg.UsageQuarantineMinAgeSec = 3600
	if err := a.queue.Enqueue(UsageBatch{Kind: "stats", Events: []UsageEvent{{UserID: 1, Bytes: 1, Source: "test", SourceEventID: "young"}}}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	ctx := context.Background()
	// Far beyond the rejection threshold, but the batch is too young.
	for i := 0; i < 10; i++ {
		advanced, _ := a.flushQueuedBatchOnce(ctx)
		if advanced {
			t.Fatalf("attempt %d: batch quarantined before min age", i+1)
		}
	}
	if got := a.queue.QuarantinedBatches(); got != 0 {
		t.Fatalf("QuarantinedBatches = %d, want 0 before min age", got)
	}
}

func TestFlushDoesNotQuarantineOnTransportErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	a, srv := newFlushTestAgent(t, dir, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_ = srv
	if err := a.queue.Enqueue(UsageBatch{Kind: "stats", Events: []UsageEvent{{UserID: 1, Bytes: 1, Source: "test", SourceEventID: "a"}}}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	ctx := context.Background()
	for i := 0; i < 10; i++ {
		advanced, keepGoing := a.flushQueuedBatchOnce(ctx)
		if advanced || keepGoing {
			t.Fatalf("attempt %d: advanced=%v keepGoing=%v, want false/false", i+1, advanced, keepGoing)
		}
	}
	if got := a.queue.QuarantinedBatches(); got != 0 {
		t.Fatalf("transport errors must never quarantine; quarantined=%d", got)
	}
	if got := a.queue.ApproxBatches(); got != 1 {
		t.Fatalf("batch must remain queued; ApproxBatches=%d", got)
	}
}

func TestFlushQuarantinesCorruptBatchImmediately(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	a, _ := newFlushTestAgent(t, dir, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("panel must not be called for a corrupt batch")
	})
	queueDir := filepath.Join(dir, "queue")
	if err := os.MkdirAll(queueDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(queueDir, "batch.20260101T000000.000000000Z.json"), []byte("{not json"), 0600); err != nil {
		t.Fatalf("write corrupt batch: %v", err)
	}

	advanced, keepGoing := a.flushQueuedBatchOnce(context.Background())
	if !advanced || !keepGoing {
		t.Fatalf("advanced=%v keepGoing=%v, want true/true", advanced, keepGoing)
	}
	if got := a.queue.QuarantinedBatches(); got != 1 {
		t.Fatalf("QuarantinedBatches = %d, want 1", got)
	}
}

func TestFlushSuccessClearsRejectionCounter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fail := true
	a, _ := newFlushTestAgent(t, dir, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if fail {
			_, _ = w.Write([]byte(`{"processed":1,"results":[{"user_id":1,"error":"event outside current quota window"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"processed":1,"results":[{"user_id":1,"status":"active"}]}`))
	})
	if err := a.queue.Enqueue(UsageBatch{Kind: "stats", Events: []UsageEvent{{UserID: 1, Bytes: 1, Source: "test", SourceEventID: "x"}}}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	ctx := context.Background()
	// Two rejections (threshold is 3), then the panel recovers.
	a.flushQueuedBatchOnce(ctx)
	a.flushQueuedBatchOnce(ctx)
	fail = false
	advanced, _ := a.flushQueuedBatchOnce(ctx)
	if !advanced {
		t.Fatalf("expected flush to succeed after panel recovery")
	}
	if got := a.queue.QuarantinedBatches(); got != 0 {
		t.Fatalf("QuarantinedBatches = %d, want 0", got)
	}
	a.mu.RLock()
	pending := len(a.usageRejections)
	a.mu.RUnlock()
	if pending != 0 {
		t.Fatalf("rejection counters not cleared: %d entries", pending)
	}
}
