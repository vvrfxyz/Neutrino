package repo

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func moveUserMonthlyQuotaWindowToPreviousCycle(t *testing.T, s *Store, userID int64, now time.Time) {
	t.Helper()

	currentStart, _, _ := quotaWindowBounds("month", "UTC", now)
	prevStart := currentStart.AddDate(0, -1, 0)
	prevEnd := currentStart
	prevKey := prevStart.Format("2006-01")
	if _, err := s.RawDB().ExecContext(context.Background(), `
UPDATE quota_windows
SET window_start = ?, window_end = ?, cycle_type = 'month', cycle_key = ?, closed_at = ?
WHERE user_id = ? AND window_start = ?;
`, prevStart.Format(time.RFC3339), prevEnd.Format(time.RFC3339), prevKey, prevEnd.Format(time.RFC3339), userID, currentStart.Format(time.RFC3339)); err != nil {
		t.Fatalf("shift quota window: %v", err)
	}
}

func TestQuotaWindowBounds(t *testing.T) {
	base := time.Date(2026, 2, 6, 18, 30, 0, 0, time.UTC)

	dayStart, dayEnd, dayKey := quotaWindowBounds("day", "Asia/Shanghai", base)
	if dayKey != "2026-02-07" {
		t.Fatalf("unexpected day key: %s", dayKey)
	}
	if dayEnd.Sub(dayStart) != 24*time.Hour {
		t.Fatalf("unexpected day window duration: %s", dayEnd.Sub(dayStart))
	}

	weekStart, weekEnd, weekKey := quotaWindowBounds("week", "Asia/Shanghai", base)
	if weekKey == "" {
		t.Fatalf("unexpected empty week key")
	}
	if weekEnd.Sub(weekStart) != 7*24*time.Hour {
		t.Fatalf("unexpected week window duration: %s", weekEnd.Sub(weekStart))
	}

	monthStart, monthEnd, monthKey := quotaWindowBounds("month", "Asia/Shanghai", base)
	if monthKey != "2026-02" {
		t.Fatalf("unexpected month key: %s", monthKey)
	}
	if monthEnd.Before(monthStart) {
		t.Fatalf("month end before start")
	}
}

func TestSweepQuotaWindows_ReactivatesMonthlyOverLimitUser(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "monthly-reset-user",
		MonthlyLimitGB: 1,
		CountingMode:   "double",
		QuotaCycle:     "month",
		QuotaTZ:        "UTC",
		DeviceLimit:    2,
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	now := time.Now().UTC()
	if _, err := s.RecordUsage(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         2 * 1024 * 1024 * 1024,
		Source:        "test",
		SourceEventID: "monthly-over-limit",
		At:            now,
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	got, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user after over-limit: %v", err)
	}
	if got.Status != "over_limit" {
		t.Fatalf("expected over_limit before sweep, got %s", got.Status)
	}

	currentStart, _, currentKey := quotaWindowBounds("month", "UTC", now)
	prevStart := currentStart.AddDate(0, -1, 0)
	prevEnd := currentStart
	prevKey := prevStart.Format("2006-01")
	if _, err := s.RawDB().ExecContext(ctx, `
UPDATE quota_windows
SET window_start = ?, window_end = ?, cycle_type = 'month', cycle_key = ?, closed_at = ?
WHERE user_id = ? AND window_start = ?;
`, prevStart.Format(time.RFC3339), prevEnd.Format(time.RFC3339), prevKey, prevEnd.Format(time.RFC3339), u.ID, currentStart.Format(time.RFC3339)); err != nil {
		t.Fatalf("shift quota window: %v", err)
	}

	reactivated, err := s.SweepQuotaWindows(ctx)
	if err != nil {
		t.Fatalf("SweepQuotaWindows: %v", err)
	}
	if reactivated != 1 {
		t.Fatalf("expected 1 reactivated user, got %d", reactivated)
	}

	got, err = s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user after sweep: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("expected active after month rollover, got %s", got.Status)
	}
	if got.RemovedAt != nil {
		t.Fatalf("expected removed_at cleared after reactivation, got %v", *got.RemovedAt)
	}
	if got.WindowCycleKey != currentKey {
		t.Fatalf("expected current window key %s, got %s", currentKey, got.WindowCycleKey)
	}

	var activeLinks int
	if err := s.RawDB().QueryRowContext(ctx, `
SELECT COUNT(*)
FROM proxy_links
WHERE user_id = ? AND active = 1;
`, u.ID).Scan(&activeLinks); err != nil {
		t.Fatalf("count active links: %v", err)
	}
	if activeLinks != 1 {
		t.Fatalf("expected 1 active proxy link after reactivation, got %d", activeLinks)
	}

	var enableLogs int
	if err := s.RawDB().QueryRowContext(ctx, `
SELECT COUNT(*)
FROM enforcement_logs
WHERE user_id = ? AND action = 'enable_user' AND reason = 'quota_cycle_reset';
`, u.ID).Scan(&enableLogs); err != nil {
		t.Fatalf("count enable logs: %v", err)
	}
	if enableLogs != 1 {
		t.Fatalf("expected 1 quota_cycle_reset enable log, got %d", enableLogs)
	}
}

func TestSweepQuotaWindows_ReactivatesDailyOverLimitUser(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "daily-reset-user",
		MonthlyLimitGB: 1,
		CountingMode:   "double",
		QuotaCycle:     "day",
		QuotaTZ:        "UTC",
		DeviceLimit:    2,
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	now := time.Now().UTC()
	if _, err := s.RecordUsage(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         2 * 1024 * 1024 * 1024,
		Source:        "test",
		SourceEventID: "daily-over-limit",
		At:            now,
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	currentStart, _, currentKey := quotaWindowBounds("day", "UTC", now)
	prevStart := currentStart.AddDate(0, 0, -1)
	prevEnd := currentStart
	prevKey := prevStart.Format("2006-01-02")
	if _, err := s.RawDB().ExecContext(ctx, `
UPDATE quota_windows
SET window_start = ?, window_end = ?, cycle_type = 'day', cycle_key = ?, closed_at = ?
WHERE user_id = ? AND window_start = ?;
`, prevStart.Format(time.RFC3339), prevEnd.Format(time.RFC3339), prevKey, prevEnd.Format(time.RFC3339), u.ID, currentStart.Format(time.RFC3339)); err != nil {
		t.Fatalf("shift quota window: %v", err)
	}

	reactivated, err := s.SweepQuotaWindows(ctx)
	if err != nil {
		t.Fatalf("SweepQuotaWindows: %v", err)
	}
	if reactivated != 1 {
		t.Fatalf("expected 1 reactivated user, got %d", reactivated)
	}

	got, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user after sweep: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("expected active after day rollover, got %s", got.Status)
	}
	if got.WindowCycleKey != currentKey {
		t.Fatalf("expected current window key %s, got %s", currentKey, got.WindowCycleKey)
	}
	if got.ActiveLink == nil {
		t.Fatalf("expected active link after day rollover reactivation")
	}
}

func TestCreateProxyLinkSweepsQuotaWindowsBeforeStatusCheck(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "link-sweep-user",
		MonthlyLimitGB: 1,
		CountingMode:   "double",
		QuotaCycle:     "month",
		QuotaTZ:        "UTC",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Now().UTC()
	if _, err := s.RecordUsage(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         2 * 1024 * 1024 * 1024,
		Source:        "test",
		SourceEventID: "link-sweep-over-limit",
		At:            now,
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	moveUserMonthlyQuotaWindowToPreviousCycle(t, s, u.ID, now)

	got, err := s.CreateProxyLink(ctx, u.ID)
	if err != nil {
		t.Fatalf("create proxy link after cycle rollover: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("status=%s, want active", got.Status)
	}
	if got.ActiveLink == nil {
		t.Fatalf("expected active link after create")
	}
}

func TestSetUserStatusSweepsQuotaWindowsBeforeStatusCheck(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "status-sweep-user",
		MonthlyLimitGB: 1,
		CountingMode:   "double",
		QuotaCycle:     "month",
		QuotaTZ:        "UTC",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Now().UTC()
	if _, err := s.RecordUsage(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         2 * 1024 * 1024 * 1024,
		Source:        "test",
		SourceEventID: "status-sweep-over-limit",
		At:            now,
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	moveUserMonthlyQuotaWindowToPreviousCycle(t, s, u.ID, now)

	got, err := s.SetUserStatus(ctx, u.ID, "active")
	if err != nil {
		t.Fatalf("set user active after cycle rollover: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("status=%s, want active", got.Status)
	}
}

func TestSetUserStatusActivateLapsedDisabledUserMarksExpired(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u := newActiveUser(t, s)
	if _, err := s.SetUserStatus(ctx, u.ID, "disabled"); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	// Lapse the plan while disabled: SweepExpiredUsers only touches
	// status='active', so the disabled user keeps status='disabled'.
	expiredAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := s.RawDB().ExecContext(ctx, `
UPDATE users
SET expires_at = ?
WHERE id = ?;
`, expiredAt, u.ID); err != nil {
		t.Fatalf("expire user: %v", err)
	}

	_, err := s.SetUserStatus(ctx, u.ID, "active")
	if !errors.Is(err, ErrUserInactive) {
		t.Fatalf("expected ErrUserInactive, got %v", err)
	}

	got, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Status != "expired" {
		t.Fatalf("expired mark must be committed: status=%s, want expired", got.Status)
	}
}

func TestExtendUserPlan_ReactivatesExpiredUserLink(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u := newActiveUser(t, s)
	expiredAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := s.RawDB().ExecContext(ctx, `
UPDATE users
SET expires_at = ?
WHERE id = ?;
`, expiredAt, u.ID); err != nil {
		t.Fatalf("expire user: %v", err)
	}
	expired, err := s.SweepExpiredUsers(ctx)
	if err != nil {
		t.Fatalf("SweepExpiredUsers: %v", err)
	}
	if expired != 1 {
		t.Fatalf("expected 1 expired user, got %d", expired)
	}

	before, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get expired user: %v", err)
	}
	if before.Status != "expired" {
		t.Fatalf("expected expired status before extend, got %s", before.Status)
	}
	if before.ActiveLink != nil {
		t.Fatalf("expected no active link after expiry sweep")
	}

	if err := s.ExtendUserPlan(ctx, u.ID, 2); err != nil {
		t.Fatalf("ExtendUserPlan: %v", err)
	}

	after, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get extended user: %v", err)
	}
	if after.Status != "active" {
		t.Fatalf("expected active status after extend, got %s", after.Status)
	}
	if after.ActiveLink == nil {
		t.Fatalf("expected active link after extending expired user")
	}
}

func TestExtendUserPlan_ExtendsLongExpiredUserFromNow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u := newActiveUser(t, s)
	expiredAt := time.Now().UTC().AddDate(0, 0, -60).Format(time.RFC3339)
	if _, err := s.RawDB().ExecContext(ctx, `
UPDATE users
SET expires_at = ?
WHERE id = ?;
`, expiredAt, u.ID); err != nil {
		t.Fatalf("expire user: %v", err)
	}
	if _, err := s.SweepExpiredUsers(ctx); err != nil {
		t.Fatalf("SweepExpiredUsers: %v", err)
	}

	beforeExtend := time.Now().UTC()
	if err := s.ExtendUserPlan(ctx, u.ID, 2); err != nil {
		t.Fatalf("ExtendUserPlan: %v", err)
	}

	after, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get extended user: %v", err)
	}
	if after.Status != "active" {
		t.Fatalf("expected active status after extend, got %s", after.Status)
	}
	if !after.ExpiresAt.After(beforeExtend) {
		t.Fatalf("expected expires_at after extension start, got %s before %s", after.ExpiresAt, beforeExtend)
	}
	if time.Until(after.ExpiresAt) < 47*time.Hour {
		t.Fatalf("expected roughly 2 days remaining, got expires_at %s", after.ExpiresAt)
	}
	if after.ActiveLink == nil {
		t.Fatalf("expected active link after extending long expired user")
	}
}

func TestExtendUserPlanPreservesRemovedAtForDisabledUserHistoricalUsage(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u := newActiveUser(t, s)
	removedAt := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	if _, err := s.RawDB().ExecContext(ctx, `
UPDATE users
SET status = 'disabled', removed_at = ?
WHERE id = ?;
`, removedAt.Format(time.RFC3339), u.ID); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	if err := s.ExtendUserPlan(ctx, u.ID, 2); err != nil {
		t.Fatalf("ExtendUserPlan: %v", err)
	}
	after, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get extended disabled user: %v", err)
	}
	if after.Status != "disabled" {
		t.Fatalf("expected disabled user to stay disabled, got %s", after.Status)
	}
	if after.RemovedAt == nil {
		t.Fatalf("expected removed_at to be preserved")
	}
	if !after.RemovedAt.Equal(removedAt) {
		t.Fatalf("removed_at changed: got %s want %s", after.RemovedAt.Format(time.RFC3339), removedAt.Format(time.RFC3339))
	}

	if _, err := s.RecordUsage(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         1,
		Source:        "test",
		SourceEventID: "disabled-historical-after-extend",
		At:            removedAt.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("historical usage before removed_at should still be accepted: %v", err)
	}
}

func TestRecordUsageDoesNotRegressLastSeenOrClearLastTarget(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newActiveUser(t, s)

	newAt := time.Now().UTC().Truncate(time.Second)
	if _, err := s.RecordUsage(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         10,
		Source:        "test",
		SourceEventID: "last-seen-new",
		TargetHost:    "example.com",
		TargetPort:    443,
		At:            newAt,
	}); err != nil {
		t.Fatalf("record new usage: %v", err)
	}
	if _, err := s.RecordUsage(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         5,
		Source:        "test",
		SourceEventID: "last-seen-old-empty-target",
		At:            newAt.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("record older usage: %v", err)
	}

	got, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.OutboundBytes != 15 {
		t.Fatalf("expected counters to include both events, got outbound=%d", got.OutboundBytes)
	}
	if got.LastSeenAt == nil || !got.LastSeenAt.Equal(newAt) {
		t.Fatalf("last_seen regressed: got %v want %s", got.LastSeenAt, newAt.Format(time.RFC3339))
	}
	if got.LastTargetHost != "example.com" {
		t.Fatalf("last target host was cleared/regressed: got %q", got.LastTargetHost)
	}
	if got.LastTargetPort == nil || *got.LastTargetPort != 443 {
		t.Fatalf("last target port was cleared/regressed: got %v", got.LastTargetPort)
	}
}

func TestResetUserQuota_ReactivatesOverLimitUser(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "reset-reactivates",
		MonthlyLimitGB: 1,
		CountingMode:   "double",
		QuotaCycle:     "month",
		QuotaTZ:        "UTC",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := s.RecordUsage(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         2 * 1024 * 1024 * 1024,
		Source:        "test",
		SourceEventID: "reset-over-limit",
		At:            time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	over, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get over-limit user: %v", err)
	}
	if over.Status != "over_limit" || over.ActiveLink != nil {
		t.Fatalf("expected over_limit without active link, got %+v", over)
	}

	if err := s.ResetUserQuota(ctx, u.ID, "test reset"); err != nil {
		t.Fatalf("ResetUserQuota: %v", err)
	}
	got, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get reset user: %v", err)
	}
	if got.Status != "active" || got.ActiveLink == nil {
		t.Fatalf("expected active user with restored link, got %+v", got)
	}
}

func TestResetUserQuota_TwiceWithinSameSecond(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "reset-same-second",
		MonthlyLimitGB: 1,
		CountingMode:   "double",
		QuotaCycle:     "month",
		QuotaTZ:        "UTC",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// window_start has second granularity; align to just after a second
	// boundary so both resets land in the same RFC3339 second.
	now := time.Now()
	next := now.Truncate(time.Second).Add(time.Second)
	time.Sleep(time.Until(next.Add(50 * time.Millisecond)))

	if err := s.ResetUserQuota(ctx, u.ID, "first reset"); err != nil {
		t.Fatalf("first ResetUserQuota: %v", err)
	}
	if err := s.ResetUserQuota(ctx, u.ID, "second reset"); err != nil {
		t.Fatalf("second ResetUserQuota in same second: %v", err)
	}

	got, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("user should stay active after double reset, got %s", got.Status)
	}
	if got.WindowEffective != 0 {
		t.Fatalf("reset window should be empty, got effective=%d", got.WindowEffective)
	}
}

func TestResetUserQuota_LateOldWindowUsageDoesNotDisableCurrentUser(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "reset-late-usage",
		MonthlyLimitGB: 1,
		CountingMode:   "double",
		QuotaCycle:     "month",
		QuotaTZ:        "UTC",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	lateAt := time.Now().UTC().Add(-time.Second)
	if err := s.ResetUserQuota(ctx, u.ID, "manual reset"); err != nil {
		t.Fatalf("ResetUserQuota: %v", err)
	}

	if _, err := s.RecordUsage(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         2 * 1024 * 1024 * 1024,
		Source:        "test",
		SourceEventID: "reset-late-old-window",
		At:            lateAt,
	}); err != nil {
		t.Fatalf("record late usage: %v", err)
	}

	got, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("late old-window usage after reset should not disable current user, got %s", got.Status)
	}
	if got.WindowEffective != 0 {
		t.Fatalf("current reset window should stay empty, got effective=%d", got.WindowEffective)
	}
}

func TestCreditUserQuota_ReactivatesOnlyWhenCreditCoversUsage(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "credit-reactivates",
		MonthlyLimitGB: 1,
		CountingMode:   "double",
		QuotaCycle:     "month",
		QuotaTZ:        "UTC",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := s.RecordUsage(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         2 * 1024 * 1024 * 1024,
		Source:        "test",
		SourceEventID: "credit-over-limit",
		At:            time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	if err := s.CreditUserQuota(ctx, u.ID, 512*1024*1024); err != nil {
		t.Fatalf("small CreditUserQuota: %v", err)
	}
	stillOver, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get after small credit: %v", err)
	}
	if stillOver.Status != "over_limit" {
		t.Fatalf("small credit should not reactivate, got %s", stillOver.Status)
	}
	if err := s.CreditUserQuota(ctx, u.ID, 2*1024*1024*1024); err != nil {
		t.Fatalf("covering CreditUserQuota: %v", err)
	}
	got, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get after covering credit: %v", err)
	}
	if got.Status != "active" || got.ActiveLink == nil {
		t.Fatalf("expected active user with restored link after credit, got %+v", got)
	}
}

func TestRecordUsageAcceptsActiveUserQueuedPreviousWindow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "late-usage-user",
		MonthlyLimitGB: 1,
		CountingMode:   "double",
		QuotaCycle:     "day",
		QuotaTZ:        "UTC",
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	currentStart, _, _ := quotaWindowBounds("day", "UTC", time.Now().UTC())
	oldAt := currentStart.Add(-time.Hour)
	if _, err := s.RecordUsage(ctx, UsageInput{
		UserID:        u.ID,
		Direction:     "outbound",
		Bytes:         5 * 1024 * 1024 * 1024,
		Source:        "test",
		SourceEventID: "late-previous-window",
		At:            oldAt,
	}); err != nil {
		t.Fatalf("queued previous-window usage should be recorded, got %v", err)
	}
	got, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("previous-window usage should not disable the current user, got %s", got.Status)
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

func TestListUserOnlineStats(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u1 := newActiveUser(t, s)
	u2 := newActiveUser(t, s)
	node1, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "stats-node-a",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node1: %v", err)
	}
	node2, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "stats-node-b",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.org",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node2: %v", err)
	}

	now := time.Now().UTC()
	if err := s.ApplyOnlineSnapshot(ctx, node1.ID, OnlineSnapshotInput{
		ObservedAt: now,
		Items: []OnlineSnapshotItemInput{
			{UserID: u1.ID, ClientIP: "10.0.0.1", LastSeenAt: now},
			{UserID: u1.ID, ClientIP: "10.0.0.2", LastSeenAt: now.Add(2 * time.Second)},
			{UserID: u2.ID, ClientIP: "10.0.1.1", LastSeenAt: now.Add(3 * time.Second)},
		},
	}); err != nil {
		t.Fatalf("apply node1 snapshot: %v", err)
	}
	if err := s.ApplyOnlineSnapshot(ctx, node2.ID, OnlineSnapshotInput{
		ObservedAt: now,
		Items: []OnlineSnapshotItemInput{
			{UserID: u1.ID, ClientIP: "10.0.0.1", LastSeenAt: now.Add(time.Second)},
		},
	}); err != nil {
		t.Fatalf("apply node2 snapshot: %v", err)
	}

	stats, err := s.ListUserOnlineStats(ctx, 120)
	if err != nil {
		t.Fatalf("list user online stats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected stats for 2 users, got %d", len(stats))
	}
	if got := stats[u1.ID]; got.SessionCount != 3 || got.DeviceCount != 2 {
		t.Fatalf("unexpected stats for user1: %+v", got)
	}
	if got := stats[u2.ID]; got.SessionCount != 1 || got.DeviceCount != 1 {
		t.Fatalf("unexpected stats for user2: %+v", got)
	}
}

func TestEnforceIPLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "ip-limit-user",
		MonthlyLimitGB: 500,
		CountingMode:   "double",
		QuotaCycle:     "month",
		QuotaTZ:        "Asia/Shanghai",
		DeviceLimit:    1,
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "ip-limit-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC()
	if err := s.ApplyOnlineSnapshot(ctx, node.ID, OnlineSnapshotInput{
		ObservedAt: now,
		Items: []OnlineSnapshotItemInput{
			{UserID: u.ID, ClientIP: "10.0.0.1", LastSeenAt: now},
			{UserID: u.ID, ClientIP: "10.0.0.2", LastSeenAt: now},
		},
	}); err != nil {
		t.Fatalf("apply online snapshot: %v", err)
	}

	affected, err := s.EnforceIPLimit(ctx, 120, 1)
	if err != nil {
		t.Fatalf("enforce ip limit: %v", err)
	}
	if len(affected) != 1 {
		t.Fatalf("expected 1 affected user, got %d", len(affected))
	}
	if affected[0].Status != "over_ip_limit" {
		t.Fatalf("expected status over_ip_limit, got %s", affected[0].Status)
	}
}

func TestOnlineSessionTimestampsDoNotRegressForLateEvents(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "online-regress-user",
		MonthlyLimitGB: 500,
		CountingMode:   "double",
		QuotaCycle:     "month",
		QuotaTZ:        "UTC",
		DeviceLimit:    2,
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "online-regress-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-time.Minute)
	if err := s.ApplyOnlineSnapshot(ctx, node.ID, OnlineSnapshotInput{
		ObservedAt: now,
		Items: []OnlineSnapshotItemInput{
			{UserID: u.ID, ClientIP: "10.0.0.9", LastSeenAt: now},
		},
	}); err != nil {
		t.Fatalf("apply current snapshot: %v", err)
	}
	if err := s.ApplyOnlineSnapshot(ctx, node.ID, OnlineSnapshotInput{
		ObservedAt: now,
		Items: []OnlineSnapshotItemInput{
			{UserID: u.ID, ClientIP: "10.0.0.9", LastSeenAt: old},
		},
	}); err != nil {
		t.Fatalf("apply old snapshot: %v", err)
	}

	sessions, err := s.ListUserOnlineSessions(ctx, u.ID, 120)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected session to remain online after late old event, got %d", len(sessions))
	}
	if !sessions[0].LastSeen.Equal(now) {
		t.Fatalf("last_seen regressed to %s, want %s", sessions[0].LastSeen.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	if !sessions[0].FirstSeen.Equal(old) {
		t.Fatalf("first_seen=%s, want earliest %s", sessions[0].FirstSeen.Format(time.RFC3339), old.Format(time.RFC3339))
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	raw, meta, err := s.CreateAPIKey(ctx, "ci", "users:read,users:write", nil, nil)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	if raw == "" || meta.ID == 0 {
		t.Fatalf("invalid api key result")
	}
	auth, ok, err := s.ValidateAPIKey(ctx, raw)
	if err != nil {
		t.Fatalf("validate api key: %v", err)
	}
	if !ok {
		t.Fatalf("api key should be valid")
	}
	if _, has := auth.Scopes["users:read"]; !has {
		t.Fatalf("missing users:read scope")
	}

	if err := s.RevokeAPIKey(ctx, meta.ID); err != nil {
		t.Fatalf("revoke api key: %v", err)
	}
	_, ok, err = s.ValidateAPIKey(ctx, raw)
	if err != nil {
		t.Fatalf("validate revoked api key: %v", err)
	}
	if ok {
		t.Fatalf("revoked api key should be invalid")
	}
}

func TestSetUserNodeAccessRejectsMissingUserEvenWithoutNodeIDs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	err := s.SetUserNodeAccess(ctx, 999999, nil)
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound, got %v", err)
	}
}

func TestCreateUserRejectsMissingNodeID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	_, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "missing-node-user",
		MonthlyLimitGB: 100,
		CountingMode:   "double",
		QuotaCycle:     "month",
		QuotaTZ:        "UTC",
		DeviceLimit:    2,
		PlanDays:       30,
		NodeIDs:        []int64{999999},
	})
	if !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("expected ErrNodeNotFound, got %v", err)
	}

	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 0 {
		t.Fatalf("expected rollback on missing node, got %d users", len(users))
	}
}

func TestSetUserNodeAccessDedupesNodeIDs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	nodeA, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "dedupe-node-a",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create nodeA: %v", err)
	}
	nodeB, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "dedupe-node-b",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.net",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create nodeB: %v", err)
	}
	user := newActiveUser(t, s)

	if err := s.SetUserNodeAccess(ctx, user.ID, []int64{nodeB.ID, nodeA.ID, nodeB.ID, nodeA.ID}); err != nil {
		t.Fatalf("SetUserNodeAccess: %v", err)
	}

	allowedIDs, err := s.ListUserNodeIDs(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListUserNodeIDs: %v", err)
	}
	if len(allowedIDs) != 2 || allowedIDs[0] != nodeA.ID || allowedIDs[1] != nodeB.ID {
		t.Fatalf("unexpected allowed node ids: %+v", allowedIDs)
	}
}

func TestRecordUsageBatchDuplicateSurvivesNodeAccessChange(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	nodeA, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "usage-dup-node-a",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "a.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create nodeA: %v", err)
	}
	nodeB, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "usage-dup-node-b",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "b.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create nodeB: %v", err)
	}
	user, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "usage-dup-restricted",
		MonthlyLimitGB: 100,
		CountingMode:   "double",
		QuotaCycle:     "month",
		QuotaTZ:        "UTC",
		DeviceLimit:    2,
		PlanDays:       30,
		NodeIDs:        []int64{nodeA.ID},
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	event := UsageInput{
		UserID:        user.ID,
		NodeID:        &nodeA.ID,
		Direction:     "outbound",
		Bytes:         123,
		At:            time.Now().UTC(),
		Source:        "xray-stats",
		SourceEventID: "dup-before-node-access-change",
	}
	first, err := s.RecordUsageBatchIdempotent(ctx, []UsageInput{event})
	if err != nil {
		t.Fatalf("record first batch: %v", err)
	}
	if len(first) != 1 || first[0].Err != nil || first[0].Duplicate {
		t.Fatalf("unexpected first result: %+v", first)
	}

	if err := s.SetUserNodeAccess(ctx, user.ID, []int64{nodeB.ID}); err != nil {
		t.Fatalf("change node access: %v", err)
	}

	second, err := s.RecordUsageBatchIdempotent(ctx, []UsageInput{event})
	if err != nil {
		t.Fatalf("record duplicate batch: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("unexpected result count: %d", len(second))
	}
	if second[0].Err != nil {
		t.Fatalf("duplicate should not fail after node access changes: %+v", second[0])
	}
	if !second[0].Duplicate {
		t.Fatalf("expected duplicate result after node access changes: %+v", second[0])
	}
}

func TestDeleteNodeRevokesBoundAPIKeys(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "bound-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	raw, _, err := s.CreateAPIKey(ctx, "bound-key", "usage:write", &node.ID, nil)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	if err := s.DeleteNode(ctx, node.ID); err != nil {
		t.Fatalf("delete node: %v", err)
	}

	_, ok, err := s.ValidateAPIKey(ctx, raw)
	if err != nil {
		t.Fatalf("validate api key after delete: %v", err)
	}
	if ok {
		t.Fatalf("deleted node should revoke bound api key")
	}
}

func TestSubscriptionTokenRejectsInactiveOrExpiredUser(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newActiveUser(t, s)
	tok, err := s.GetSubscriptionTokenByUserID(ctx, u.ID)
	if err != nil {
		t.Fatalf("get token: %v", err)
	}

	if _, err := s.RawDB().ExecContext(ctx, `UPDATE users SET status = 'disabled', removed_at = ? WHERE id = ?;`, time.Now().UTC().Format(time.RFC3339), u.ID); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if _, err := s.RawDB().ExecContext(ctx, `UPDATE proxy_links SET active = 1 WHERE user_id = ?;`, u.ID); err != nil {
		t.Fatalf("make stale active link: %v", err)
	}
	if _, _, err := s.GetUserBySubscriptionToken(ctx, tok.Token); !errors.Is(err, ErrUserInactive) {
		t.Fatalf("disabled user with stale active link should be inactive, got %v", err)
	}

	u2 := newActiveUser(t, s)
	tok2, err := s.GetSubscriptionTokenByUserID(ctx, u2.ID)
	if err != nil {
		t.Fatalf("get token 2: %v", err)
	}
	if _, err := s.RawDB().ExecContext(ctx, `UPDATE users SET status = 'active', expires_at = ? WHERE id = ?;`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), u2.ID); err != nil {
		t.Fatalf("expire user: %v", err)
	}
	if _, err := s.RawDB().ExecContext(ctx, `UPDATE proxy_links SET active = 1 WHERE user_id = ?;`, u2.ID); err != nil {
		t.Fatalf("restore stale active link: %v", err)
	}
	if _, _, err := s.GetUserBySubscriptionToken(ctx, tok2.Token); !errors.Is(err, ErrUserInactive) {
		t.Fatalf("expired active user should be inactive, got %v", err)
	}
}

func TestTelegramBindingFlow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newActiveUser(t, s)

	bind, err := s.EnsureTelegramBinding(ctx, u.ID)
	if err != nil {
		t.Fatalf("ensure telegram binding: %v", err)
	}
	if bind.BindCode == "" {
		t.Fatalf("empty bind code")
	}
	if len(bind.BindCode) < 32 {
		t.Fatalf("bind code too short: %q", bind.BindCode)
	}
	if bind.BindCodeExpiresAt == nil || !bind.BindCodeExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("expected future bind code expiry, got %+v", bind.BindCodeExpiresAt)
	}
	usedCode := bind.BindCode

	boundUser, updated, err := s.BindTelegramChatByCode(ctx, bind.BindCode, 123456, 777, "alice_tg")
	if err != nil {
		t.Fatalf("bind by code: %v", err)
	}
	if boundUser.ID != u.ID {
		t.Fatalf("bound wrong user id: %d", boundUser.ID)
	}
	if updated.TelegramChatID == nil || *updated.TelegramChatID != 123456 {
		t.Fatalf("unexpected chat id in binding: %+v", updated)
	}
	if _, _, err := s.BindTelegramChatByCode(ctx, usedCode, 654321, 888, "mallory"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("used bind code should be one-time, got %v", err)
	}

	byChat, err := s.GetUserByTelegramChatID(ctx, 123456)
	if err != nil {
		t.Fatalf("get user by chat id: %v", err)
	}
	if byChat.ID != u.ID {
		t.Fatalf("unexpected user by chat id: %d", byChat.ID)
	}
}

func TestReadOnlyLookupHelpers(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newActiveUser(t, s)

	if _, err := s.GetSubscriptionTokenByUserID(ctx, u.ID); err != nil {
		t.Fatalf("get subscription token by user id: %v", err)
	}
	if _, err := s.GetTelegramBindingByUserID(ctx, u.ID); err != nil {
		t.Fatalf("get telegram binding by user id: %v", err)
	}

	u2, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "lookup-missing-user",
		MonthlyLimitGB: 500,
		CountingMode:   "double",
		QuotaCycle:     "month",
		QuotaTZ:        "Asia/Shanghai",
		DeviceLimit:    1,
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := s.RawDB().ExecContext(ctx, `DELETE FROM subscription_tokens WHERE user_id = ?`, u2.ID); err != nil {
		t.Fatalf("delete subscription token: %v", err)
	}
	if _, err := s.RawDB().ExecContext(ctx, `DELETE FROM telegram_bindings WHERE user_id = ?`, u2.ID); err != nil {
		t.Fatalf("delete telegram binding: %v", err)
	}

	if _, err := s.GetSubscriptionTokenByUserID(ctx, u2.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows for subscription token, got %v", err)
	}
	if _, err := s.GetTelegramBindingByUserID(ctx, u2.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows for telegram binding, got %v", err)
	}
}

func TestDeleteNode_RefusesWhenItWouldWidenRestrictedUserAccess(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "restricted-only-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	user, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "restricted-user",
		MonthlyLimitGB: 100,
		CountingMode:   "double",
		QuotaCycle:     "month",
		QuotaTZ:        "UTC",
		DeviceLimit:    2,
		PlanDays:       30,
		NodeIDs:        []int64{node.ID},
	})
	if err != nil {
		t.Fatalf("create restricted user: %v", err)
	}

	err = s.DeleteNode(ctx, node.ID)
	if !errors.Is(err, ErrNodeDeleteWouldWidenAccess) {
		t.Fatalf("expected ErrNodeDeleteWouldWidenAccess, got %v", err)
	}

	if _, err := s.GetNode(ctx, node.ID); err != nil {
		t.Fatalf("node should still exist after refused delete: %v", err)
	}

	allowedIDs, err := s.ListUserNodeIDs(ctx, user.ID)
	if err != nil {
		t.Fatalf("list user node ids: %v", err)
	}
	if len(allowedIDs) != 1 || allowedIDs[0] != node.ID {
		t.Fatalf("unexpected allowed node ids after refused delete: %+v", allowedIDs)
	}
}

func TestDeleteNode_AllowsDeleteWhenRestrictedUserKeepsAnotherNode(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	nodeA, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "keep-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create nodeA: %v", err)
	}
	nodeB, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "delete-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.net",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create nodeB: %v", err)
	}

	user, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "multi-node-user",
		MonthlyLimitGB: 100,
		CountingMode:   "double",
		QuotaCycle:     "month",
		QuotaTZ:        "UTC",
		DeviceLimit:    2,
		PlanDays:       30,
		NodeIDs:        []int64{nodeA.ID, nodeB.ID},
	})
	if err != nil {
		t.Fatalf("create restricted user: %v", err)
	}

	if err := s.DeleteNode(ctx, nodeB.ID); err != nil {
		t.Fatalf("delete nodeB: %v", err)
	}

	if _, err := s.GetNode(ctx, nodeB.ID); err == nil {
		t.Fatalf("deleted nodeB should no longer exist")
	}

	allowedIDs, err := s.ListUserNodeIDs(ctx, user.ID)
	if err != nil {
		t.Fatalf("list user node ids: %v", err)
	}
	if len(allowedIDs) != 1 || allowedIDs[0] != nodeA.ID {
		t.Fatalf("unexpected allowed node ids after deleting nodeB: %+v", allowedIDs)
	}
}

func TestDeleteStaleNodes_SkipsProtectedNodesAndDisablesOthers(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	protectedNode, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "stale-protected-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "protected.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create protected node: %v", err)
	}
	deletableNode, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "stale-deletable-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "deletable.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create deletable node: %v", err)
	}

	if _, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "stale-guarded-user",
		MonthlyLimitGB: 100,
		CountingMode:   "double",
		QuotaCycle:     "month",
		QuotaTZ:        "UTC",
		DeviceLimit:    2,
		PlanDays:       30,
		NodeIDs:        []int64{protectedNode.ID},
	}); err != nil {
		t.Fatalf("create guarded user: %v", err)
	}

	oldSeenAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := s.RawDB().ExecContext(ctx, `UPDATE nodes SET last_seen_at = ? WHERE id IN (?, ?)`, oldSeenAt, protectedNode.ID, deletableNode.ID); err != nil {
		t.Fatalf("mark nodes stale: %v", err)
	}

	disabled, err := s.DeleteStaleNodes(ctx, time.Now().UTC().Add(-10*time.Minute))
	if err != nil {
		t.Fatalf("DeleteStaleNodes: %v", err)
	}
	if len(disabled) != 1 || disabled[0] != deletableNode.ID {
		t.Fatalf("unexpected disabled ids: %+v", disabled)
	}

	if _, err := s.GetNode(ctx, protectedNode.ID); err != nil {
		t.Fatalf("protected stale node should remain: %v", err)
	}
	got, err := s.GetNode(ctx, deletableNode.ID)
	if err != nil {
		t.Fatalf("stale node should remain disabled for cleanup sync: %v", err)
	}
	if got.Enabled {
		t.Fatalf("stale node should be disabled before cleanup")
	}
}

func TestQuotaWindowBounds_DSTSpringForward_London_2026_03_29(t *testing.T) {
	// 2026-03-29 is the DST spring-forward day in Europe/London (01:00 -> 02:00 BST).
	// A naive AddDate(0,0,1) on a midnight-local time can land at 01:00 on the next day.
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	at := time.Date(2026, 3, 29, 1, 30, 0, 0, time.UTC) // mid-afternoon UK, post-jump
	_, end, key := quotaWindowBounds("day", "Europe/London", at)
	if key != "2026-03-29" {
		t.Fatalf("unexpected day key: %s", key)
	}
	endLocal := end.In(loc)
	if endLocal.Year() != 2026 || endLocal.Month() != time.March || endLocal.Day() != 30 || endLocal.Hour() != 0 || endLocal.Minute() != 0 {
		t.Fatalf("expected end to be 2026-03-30 00:00 London, got %s", endLocal.Format(time.RFC3339))
	}
}

func TestQuotaWindowBounds_DSTSpringForward_NewYork_2026_03_08(t *testing.T) {
	// America/New_York spring-forward 2026 is 2026-03-08 (02:00 EST -> 03:00 EDT).
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// Use a mid-day UTC moment that's unambiguously on 2026-03-08 local.
	at := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)

	_, dayEnd, dayKey := quotaWindowBounds("day", "America/New_York", at)
	if dayKey != "2026-03-08" {
		t.Fatalf("unexpected day key: %s", dayKey)
	}
	dayEndLocal := dayEnd.In(loc)
	if dayEndLocal.Year() != 2026 || dayEndLocal.Month() != time.March || dayEndLocal.Day() != 9 || dayEndLocal.Hour() != 0 || dayEndLocal.Minute() != 0 {
		t.Fatalf("expected day end 2026-03-09 00:00 New_York, got %s", dayEndLocal.Format(time.RFC3339))
	}

	weekStart, weekEnd, _ := quotaWindowBounds("week", "America/New_York", at)
	weekStartLocal := weekStart.In(loc)
	weekEndLocal := weekEnd.In(loc)
	if weekStartLocal.Hour() != 0 || weekStartLocal.Minute() != 0 {
		t.Fatalf("expected week start at midnight New_York, got %s", weekStartLocal.Format(time.RFC3339))
	}
	if weekEndLocal.Hour() != 0 || weekEndLocal.Minute() != 0 {
		t.Fatalf("expected week end at midnight New_York (post-DST), got %s", weekEndLocal.Format(time.RFC3339))
	}
	if days := int(weekEndLocal.Sub(weekStartLocal).Hours() / 24); days != 7 {
		// We expect 7 *calendar* days; real-time may differ by 1 hour due to DST.
		// The window should still span exactly 7 local-midnights.
		deltaHours := weekEndLocal.Sub(weekStartLocal).Hours()
		if deltaHours < 6*24+20 || deltaHours > 7*24+4 {
			t.Fatalf("expected ~7-day span, got %v hours", deltaHours)
		}
	}
}
