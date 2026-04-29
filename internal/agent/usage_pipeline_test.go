package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type stubBatchCounter struct {
	batches int64
}

func (s stubBatchCounter) ApproxBatches() int64 { return s.batches }

func TestShouldGenerateUsageBatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		flushed int
		queue   interface{ ApproxBatches() int64 }
		want    bool
	}{
		{
			name:    "skip when flush succeeded this tick",
			flushed: 1,
			queue:   stubBatchCounter{batches: 0},
			want:    false,
		},
		{
			name:    "skip when queue still has pending batches",
			flushed: 0,
			queue:   stubBatchCounter{batches: 3},
			want:    false,
		},
		{
			name:    "allow when nothing was flushed and queue is empty",
			flushed: 0,
			queue:   stubBatchCounter{batches: 0},
			want:    true,
		},
		{
			name:    "allow when queue is nil",
			flushed: 0,
			queue:   nil,
			want:    true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := shouldGenerateUsageBatches(tc.flushed, tc.queue)
			if got != tc.want {
				t.Fatalf("shouldGenerateUsageBatches(%d, %+v) = %v, want %v", tc.flushed, tc.queue, got, tc.want)
			}
		})
	}
}

func TestEnqueueDueUsageBatches_EnqueuesAccessAndStatsWhenBothDue(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)
	accessBatch := UsageBatch{Kind: "access"}
	statsBatch := UsageBatch{Kind: "stats"}

	var got []string
	lastAccessAt, lastStatsAt, enqueued := enqueueDueUsageBatches(
		now,
		time.Time{},
		time.Time{},
		2*time.Second,
		5*time.Second,
		func() (UsageBatch, bool) { return accessBatch, true },
		func() (UsageBatch, bool) { return statsBatch, true },
		func(batch UsageBatch) error {
			got = append(got, batch.Kind)
			return nil
		},
	)

	if enqueued != 2 {
		t.Fatalf("enqueued=%d, want 2", enqueued)
	}
	if !lastAccessAt.Equal(now) {
		t.Fatalf("lastAccessAt=%v, want %v", lastAccessAt, now)
	}
	if !lastStatsAt.Equal(now) {
		t.Fatalf("lastStatsAt=%v, want %v", lastStatsAt, now)
	}
	if want := []string{"access", "stats"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("enqueued kinds=%v, want %v", got, want)
	}
}

func TestEnqueueDueUsageBatches_StatsStillRunsWhenAccessEnqueueFails(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)
	var got []string

	lastAccessAt, lastStatsAt, enqueued := enqueueDueUsageBatches(
		now,
		time.Time{},
		time.Time{},
		2*time.Second,
		5*time.Second,
		func() (UsageBatch, bool) { return UsageBatch{Kind: "access"}, true },
		func() (UsageBatch, bool) { return UsageBatch{Kind: "stats"}, true },
		func(batch UsageBatch) error {
			got = append(got, batch.Kind)
			if batch.Kind == "access" {
				return errors.New("boom")
			}
			return nil
		},
	)

	if enqueued != 1 {
		t.Fatalf("enqueued=%d, want 1", enqueued)
	}
	if !lastAccessAt.IsZero() {
		t.Fatalf("lastAccessAt=%v, want zero because access enqueue failed", lastAccessAt)
	}
	if !lastStatsAt.Equal(now) {
		t.Fatalf("lastStatsAt=%v, want %v", lastStatsAt, now)
	}
	if want := []string{"access", "stats"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("enqueue order=%v, want %v", got, want)
	}
}

func TestBuildAccessBatchAdvancesOffsetWhenNoEventsAccepted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	content := "unmatched line one\nunmatched line two\n"
	if err := os.WriteFile(logPath, []byte(content), 0600); err != nil {
		t.Fatalf("write access log: %v", err)
	}

	state := NewStateStore(filepath.Join(dir, "state.json"))
	if err := state.Update(func(s *State) {
		s.SyncedUsers = []UserSyncItem{{UserID: 1, Email: "user@example.com", Status: "active"}}
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	a := &Agent{
		cfg: Config{
			NodeID:            7,
			XrayAccessLogPath: logPath,
		},
		state:   state,
		sampler: NewAccessSampler(10, 10),
	}

	if _, ok := a.buildAccessBatch(context.Background()); ok {
		t.Fatalf("expected no usage batch for unmatched access lines")
	}

	snap := state.Snapshot()
	if snap.Access.Path != logPath {
		t.Fatalf("access path=%q, want %q", snap.Access.Path, logPath)
	}
	if snap.Access.Inode == 0 {
		t.Fatalf("expected inode to be recorded")
	}
	if snap.Access.Offset != int64(len(content)) {
		t.Fatalf("access offset=%d, want %d", snap.Access.Offset, len(content))
	}
}

func TestPlanStatsBatch_TruncationOnlyAcknowledgesEmittedDirections(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)
	batch := planStatsBatch(14, []statsCounterSample{
		{
			UserID: 1,
			Email:  "user-alice",
			Acked: StatEntry{
				Uplink:   100,
				Downlink: 200,
			},
			Current: StatEntry{
				Uplink:   150,
				Downlink: 280,
			},
		},
	}, 7, 1, now)

	if len(batch.Events) != 1 {
		t.Fatalf("len(events)=%d, want 1", len(batch.Events))
	}
	if batch.Events[0].Direction != "inbound" {
		t.Fatalf("first event direction=%q, want inbound", batch.Events[0].Direction)
	}
	gotAck := batch.Stats.NextAck["user-alice"]
	wantAck := StatEntry{Uplink: 150, Downlink: 200}
	if gotAck != wantAck {
		t.Fatalf("nextAck=%+v, want %+v", gotAck, wantAck)
	}
}

func TestPlanStatsBatch_ZeroDeltaResetAdvancesEpochAndAckWithoutEvents(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)
	batch := planStatsBatch(14, []statsCounterSample{
		{
			UserID: 2,
			Email:  "user-bob",
			Acked: StatEntry{
				Uplink:   300,
				Downlink: 400,
			},
			Current: StatEntry{},
		},
	}, 9, 500, now)

	if len(batch.Events) != 0 {
		t.Fatalf("len(events)=%d, want 0", len(batch.Events))
	}
	if batch.Stats == nil {
		t.Fatalf("batch.Stats=nil, want non-nil")
	}
	if batch.Stats.Epoch != 10 {
		t.Fatalf("epoch=%d, want 10", batch.Stats.Epoch)
	}
	gotAck := batch.Stats.NextAck["user-bob"]
	if gotAck != (StatEntry{}) {
		t.Fatalf("nextAck=%+v, want zero entry", gotAck)
	}
}

func TestPlanStatsBatch_NonZeroResetEmitsFirstPostResetBytes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)
	batch := planStatsBatch(14, []statsCounterSample{
		{
			UserID: 3,
			Email:  "resetfish",
			Acked: StatEntry{
				Uplink:   300,
				Downlink: 400,
			},
			Current: StatEntry{
				Uplink:   12,
				Downlink: 34,
			},
		},
	}, 9, 500, now)

	if len(batch.Events) != 2 {
		t.Fatalf("len(events)=%d, want 2", len(batch.Events))
	}
	if batch.Stats == nil || batch.Stats.Epoch != 10 {
		t.Fatalf("epoch=%v, want 10", batch.Stats)
	}
	if batch.Events[0].Bytes != 12 || batch.Events[0].Direction != "inbound" {
		t.Fatalf("first event=%+v, want inbound 12", batch.Events[0])
	}
	if batch.Events[1].Bytes != 34 || batch.Events[1].Direction != "outbound" {
		t.Fatalf("second event=%+v, want outbound 34", batch.Events[1])
	}
	gotAck := batch.Stats.NextAck["resetfish"]
	if gotAck != (StatEntry{Uplink: 12, Downlink: 34}) {
		t.Fatalf("nextAck=%+v, want 12/34", gotAck)
	}
	if !strings.Contains(batch.Events[0].SourceEventID, ":e10:") ||
		!strings.Contains(batch.Events[1].SourceEventID, ":e10:") {
		t.Fatalf("reset events should use bumped epoch: %+v", batch.Events)
	}
}

func TestPlanStatsBatch_DoesNotAcknowledgeUsersDroppedByBatchCap(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)
	batch := planStatsBatch(14, []statsCounterSample{
		{
			UserID: 1,
			Email:  "first",
			Acked:  StatEntry{Uplink: 10},
			Current: StatEntry{
				Uplink: 15,
			},
		},
		{
			UserID: 2,
			Email:  "second",
			Acked:  StatEntry{Uplink: 20},
			Current: StatEntry{
				Uplink: 25,
			},
		},
	}, 3, 1, now)

	if len(batch.Events) != 1 {
		t.Fatalf("len(events)=%d, want 1", len(batch.Events))
	}
	if _, ok := batch.Stats.NextAck["second"]; ok {
		t.Fatalf("second user unexpectedly acknowledged: %+v", batch.Stats.NextAck["second"])
	}
	if got := batch.Stats.NextAck["first"]; got != (StatEntry{Uplink: 15}) {
		t.Fatalf("first user nextAck=%+v, want uplink=15", got)
	}
}
