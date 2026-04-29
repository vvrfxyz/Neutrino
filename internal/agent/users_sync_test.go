package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeUserSyncApplier struct {
	upserted     []string
	removed      []string
	removeErrFor map[string]error
	upsertErrFor map[string]error
}

func (f *fakeUserSyncApplier) UpsertUser(_ context.Context, email, uuid string) error {
	f.upserted = append(f.upserted, email+"="+uuid)
	if f.upsertErrFor != nil {
		if err, ok := f.upsertErrFor[email]; ok {
			return err
		}
	}
	return nil
}

func (f *fakeUserSyncApplier) RemoveUser(_ context.Context, email string) error {
	f.removed = append(f.removed, email)
	if f.removeErrFor != nil {
		if err, ok := f.removeErrFor[email]; ok {
			return err
		}
	}
	return nil
}

func TestStaleSyncedUsers(t *testing.T) {
	prev := []UserSyncItem{
		{UserID: 1, Email: "alice@example.com", UUID: "u1", Status: "active"},
		{UserID: 2, Email: "bob@example.com", UUID: "u2", Status: "active"},
		{UserID: 3, Email: "bob@example.com", UUID: "u2-old", Status: "disabled"},
		{UserID: 4, Email: "   ", UUID: "u4", Status: "active"},
	}
	current := []UserSyncItem{
		{UserID: 1, Email: "alice@example.com", UUID: "u1", Status: "active"},
		{UserID: 5, Email: "carol@example.com", UUID: "u5", Status: "active"},
	}

	stale := staleSyncedUsers(prev, current)
	if len(stale) != 1 {
		t.Fatalf("expected 1 stale user, got %d: %+v", len(stale), stale)
	}
	if stale[0].Email != "bob@example.com" {
		t.Fatalf("expected bob@example.com stale, got %+v", stale[0])
	}
}

func TestApplyUsersSyncFailsOnPartialError(t *testing.T) {
	prev := []UserSyncItem{
		{UserID: 1, Email: "stale@example.com", UUID: "old", Status: "active"},
	}
	current := []UserSyncItem{
		{UserID: 2, Email: "alice@example.com", UUID: "u1", Status: "active"},
	}
	xray := &fakeUserSyncApplier{
		removeErrFor: map[string]error{
			"stale@example.com": errors.New("temporary remove failure"),
		},
	}

	synced, removed, err := applyUsersSync(context.Background(), xray, prev, current)
	if synced != 1 {
		t.Fatalf("expected 1 synced user, got %d", synced)
	}
	if removed != 0 {
		t.Fatalf("expected 0 removed users, got %d", removed)
	}
	if err == nil {
		t.Fatalf("expected retryable error")
	}
	var jobErr JobError
	if !errors.As(err, &jobErr) || !jobErr.Retryable {
		t.Fatalf("expected retryable JobError, got %T %v", err, err)
	}
	if !strings.Contains(jobErr.Msg, "stale@example.com") {
		t.Fatalf("expected error message to mention failing email, got %q", jobErr.Msg)
	}
	result, ok := jobErr.ResultJSON.(map[string]any)
	if !ok {
		t.Fatalf("expected structured result json, got %T", jobErr.ResultJSON)
	}
	if got := result["synced"]; got != 1 {
		t.Fatalf("expected synced=1 in result json, got %#v", got)
	}
	if got := result["removed"]; got != 0 {
		t.Fatalf("expected removed=0 in result json, got %#v", got)
	}
	if got := result["failed"]; got != 1 {
		t.Fatalf("expected failed=1 in result json, got %#v", got)
	}
	failures, ok := result["failures"].([]string)
	if !ok || len(failures) != 1 || !strings.Contains(failures[0], "stale@example.com") {
		t.Fatalf("unexpected failures payload: %#v", result["failures"])
	}
	if len(xray.upserted) != 1 || xray.upserted[0] != "alice@example.com=u1" {
		t.Fatalf("unexpected upserts: %+v", xray.upserted)
	}
}

func TestApplyUsersSyncCountsRemovalsAndSkipsBlankEmails(t *testing.T) {
	prev := []UserSyncItem{
		{UserID: 1, Email: "stale@example.com", UUID: "old", Status: "active"},
		{UserID: 2, Email: "  ", UUID: "ignored", Status: "active"},
	}
	current := []UserSyncItem{
		{UserID: 3, Email: "disabled@example.com", UUID: "u3", Status: "disabled"},
		{UserID: 4, Email: "active@example.com", UUID: "u4", Status: "active"},
	}
	xray := &fakeUserSyncApplier{}

	synced, removed, err := applyUsersSync(context.Background(), xray, prev, current)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if synced != 1 {
		t.Fatalf("expected 1 synced user, got %d", synced)
	}
	if removed != 2 {
		t.Fatalf("expected 2 removals, got %d", removed)
	}
	if got := strings.Join(xray.removed, ","); got != "stale@example.com,disabled@example.com" {
		t.Fatalf("unexpected removals: %s", got)
	}
	if len(xray.upserted) != 1 || xray.upserted[0] != "active@example.com=u4" {
		t.Fatalf("unexpected upserts: %+v", xray.upserted)
	}
}

func TestRestoreActiveUsersUpsertsPersistedActiveUsersOnly(t *testing.T) {
	xray := &fakeUserSyncApplier{}
	synced, err := restoreActiveUsers(context.Background(), xray, []UserSyncItem{
		{UserID: 1, Email: "active@example.com", UUID: "u1", Status: "active"},
		{UserID: 2, Email: "disabled@example.com", UUID: "u2", Status: "disabled"},
		{UserID: 3, Email: "missing-uuid@example.com", Status: "active"},
	})
	if err != nil {
		t.Fatalf("restoreActiveUsers: %v", err)
	}
	if synced != 1 {
		t.Fatalf("synced=%d, want 1", synced)
	}
	if len(xray.upserted) != 1 || xray.upserted[0] != "active@example.com=u1" {
		t.Fatalf("unexpected upserts: %+v", xray.upserted)
	}
	if len(xray.removed) != 0 {
		t.Fatalf("restore should not remove users, got %+v", xray.removed)
	}
}
