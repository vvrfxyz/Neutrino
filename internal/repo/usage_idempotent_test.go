package repo

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCreateUser_CreatesInitialProxyLink(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "create-user-has-link",
		MonthlyLimitGB: 10,
		CountingMode:   "double",
		PlanDays:       7,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if u.ActiveLink == nil || u.ActiveLink.UUID == "" || u.ActiveLink.Link == "" {
		t.Fatalf("expected initial active link, got %+v", u.ActiveLink)
	}
}

func TestCreateUser_NormalizesAndValidatesInputs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "  normalized-user  ",
		MonthlyLimitGB: 10,
		CountingMode:   "double",
		QuotaTZ:        "UTC",
		PlanDays:       7,
	})
	if err != nil {
		t.Fatalf("create normalized user: %v", err)
	}
	if u.Username != "normalized-user" {
		t.Fatalf("username=%q, want normalized-user", u.Username)
	}

	badInputs := []CreateUserInput{
		{Username: "   ", MonthlyLimitGB: 10, QuotaTZ: "UTC"},
		{Username: "bad user", MonthlyLimitGB: 10, QuotaTZ: "UTC"},
		{Username: "bad\nuser", MonthlyLimitGB: 10, QuotaTZ: "UTC"},
		{Username: "no-limit", MonthlyLimitGB: 0, QuotaTZ: "UTC"},
		{Username: "bad-tz", MonthlyLimitGB: 10, QuotaTZ: "Not/AZone"},
	}
	for i, in := range badInputs {
		if _, err := s.CreateUser(ctx, in); err == nil {
			t.Fatalf("bad input %d unexpectedly succeeded: %+v", i, in)
		}
	}
}

func TestRecordUsageIdempotent_DuplicateDoesNotChangeCounters(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newActiveUser(t, s)

	at := time.Now().UTC()
	in := UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         1234,
		At:            at,
		Source:        "test",
		SourceEventID: "dup-1",
	}

	first, err := s.RecordUsageIdempotent(ctx, in)
	if err != nil {
		t.Fatalf("first record: %v", err)
	}
	second, err := s.RecordUsageIdempotent(ctx, in)
	if err == nil || err != ErrDuplicateEvent {
		t.Fatalf("expected ErrDuplicateEvent, got %v second=%+v", err, second)
	}

	got, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.WindowEffective != first.WindowEffective {
		t.Fatalf("window effective changed on duplicate: first=%d got=%d", first.WindowEffective, got.WindowEffective)
	}
	if got.OutboundBytes != first.OutboundBytes {
		t.Fatalf("outbound total changed on duplicate: first=%d got=%d", first.OutboundBytes, got.OutboundBytes)
	}
}

func TestUsageIdempotencySurvivesRawEventPrune(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newActiveUser(t, s)

	at := time.Now().UTC().Truncate(time.Second)
	in := UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         1234,
		At:            at,
		Source:        "test-prune",
		SourceEventID: "dup-after-prune",
	}
	first, err := s.RecordUsageIdempotent(ctx, in)
	if err != nil {
		t.Fatalf("first record: %v", err)
	}

	old := at.Add(-2 * time.Hour).Format(time.RFC3339)
	if _, err := s.RawDB().ExecContext(ctx, `
UPDATE traffic_events
SET event_at = ?
WHERE source = ? AND source_event_id = ?;
`, old, in.Source, in.SourceEventID); err != nil {
		t.Fatalf("age raw event: %v", err)
	}
	deleted, err := s.PruneTrafficEvents(ctx, in.Source, at.Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("prune raw events: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d, want 1", deleted)
	}

	if _, err := s.RecordUsageIdempotent(ctx, in); !errors.Is(err, ErrDuplicateEvent) {
		t.Fatalf("duplicate after raw prune should still be deduped, got %v", err)
	}
	got, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.WindowEffective != first.WindowEffective || got.OutboundBytes != first.OutboundBytes {
		t.Fatalf("counters changed after duplicate: first effective/out=%d/%d got=%d/%d",
			first.WindowEffective, first.OutboundBytes, got.WindowEffective, got.OutboundBytes)
	}
}

func TestRecordUsageIdempotentAcceptsActiveUserQueuedPreviousWindow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "idempotent-past-window",
		MonthlyLimitGB: 10,
		CountingMode:   "double",
		QuotaCycle:     "day",
		QuotaTZ:        "UTC",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	currentStart, _, _ := quotaWindowBounds("day", "UTC", time.Now().UTC())
	oldAt := currentStart.Add(-time.Minute)
	if _, err := s.RecordUsageIdempotent(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         1,
		At:            oldAt,
		Source:        "test",
		SourceEventID: "idempotent-past-window",
	}); err != nil {
		t.Fatalf("queued previous-window usage should be recorded, got %v", err)
	}

	got, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.WindowEffective != 0 {
		t.Fatalf("current window should stay empty, got effective=%d", got.WindowEffective)
	}

	var oldEffective int64
	if err := s.RawDB().QueryRowContext(ctx, `
		SELECT effective_bytes
		FROM quota_windows
		WHERE user_id = ? AND window_start <= ? AND window_end > ?;
	`, u.ID, oldAt.Format(time.RFC3339), oldAt.Format(time.RFC3339)).Scan(&oldEffective); err != nil {
		t.Fatalf("query old window: %v", err)
	}
	if oldEffective == 0 {
		t.Fatalf("queued previous-window usage should be accounted in its event-time window")
	}
}

func TestRecordUsageBatchIdempotent_AcceptsSameBatchEventsAfterLimitTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "batch-limit-user",
		MonthlyLimitGB: 1,
		CountingMode:   "double",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	now := time.Now().UTC()
	results, err := s.RecordUsageBatchIdempotent(ctx, []UsageInput{
		{
			UserID:        u.ID,
			Direction:     "outbound",
			Bytes:         1024*1024*1024 + 1,
			At:            now,
			Source:        "test",
			SourceEventID: "batch-limit-out",
		},
		{
			UserID:        u.ID,
			Direction:     "inbound",
			Bytes:         1,
			At:            now,
			Source:        "test",
			SourceEventID: "batch-limit-in",
		},
	})
	if err != nil {
		t.Fatalf("record batch: %v", err)
	}
	for i, got := range results {
		if got.Err != nil {
			t.Fatalf("result %d unexpected error: %v", i, got.Err)
		}
		if got.Duplicate {
			t.Fatalf("result %d unexpectedly duplicate", i)
		}
	}

	got, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Status != "over_limit" {
		t.Fatalf("status=%s, want over_limit", got.Status)
	}
	wantEffective := int64(1024*1024*1024 + 2)
	if got.WindowEffective != wantEffective {
		t.Fatalf("window effective=%d, want %d", got.WindowEffective, wantEffective)
	}
}

func TestRecordUsageIdempotent_AcceptsHistoricalEventBeforeDisable(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "late-after-disable",
		MonthlyLimitGB: 1,
		CountingMode:   "double",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	eventAt := time.Now().UTC().Truncate(time.Second).Add(-time.Second)
	if _, err := s.RecordUsage(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         1024*1024*1024 + 1,
		At:            eventAt,
		Source:        "test",
		SourceEventID: "disable-user",
	}); err != nil {
		t.Fatalf("record disabling usage: %v", err)
	}
	over, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get over-limit user: %v", err)
	}
	if over.Status != "over_limit" || over.RemovedAt == nil {
		t.Fatalf("expected over_limit with removed_at, got %+v", over)
	}

	if _, err := s.RecordUsageIdempotent(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "inbound",
		Bytes:         99,
		At:            over.RemovedAt.Add(-time.Second),
		Source:        "test",
		SourceEventID: "late-before-disable",
	}); err != nil {
		t.Fatalf("historical event before disable should be accepted: %v", err)
	}
	if _, err := s.RecordUsageIdempotent(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "inbound",
		Bytes:         99,
		At:            over.RemovedAt.Add(time.Second),
		Source:        "test",
		SourceEventID: "late-after-disable",
	}); !errors.Is(err, ErrUserInactive) {
		t.Fatalf("event after disable should remain inactive, got %v", err)
	}
}

func TestRecordUsageRejectsActiveUserTimestampSkew(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newActiveUser(t, s)

	if _, err := s.RecordUsage(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         1,
		At:            time.Now().UTC().Add(usageMaxFutureSkew + time.Minute),
		Source:        "test",
		SourceEventID: "future-skew",
	}); !errors.Is(err, ErrUsageTimestampSkew) {
		t.Fatalf("future skew should be rejected, got %v", err)
	}
	if _, err := s.RecordUsage(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         1,
		At:            time.Now().UTC().Add(-(usageMaxActiveBackdate + time.Hour)),
		Source:        "test",
		SourceEventID: "past-skew",
	}); !errors.Is(err, ErrUsageTimestampSkew) {
		t.Fatalf("past skew should be rejected, got %v", err)
	}
	if _, err := s.RecordUsage(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         1,
		At:            time.Now().UTC().Add(-(usageMaxActiveBackdate + 2*time.Hour)),
		Source:        "test",
		SourceEventID: "past-too-old",
	}); !errors.Is(err, ErrUsageTimestampTooOld) {
		t.Fatalf("past skew should expose ErrUsageTimestampTooOld, got %v", err)
	}
}
