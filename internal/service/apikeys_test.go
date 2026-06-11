package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalizeAPIKeyScopes(t *testing.T) {
	got, err := NormalizeAPIKeyScopes([]string{" users:write ", "users:read", "users:read", ""})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got != "users:read,users:write" {
		t.Fatalf("normalized scopes = %q, want users:read,users:write", got)
	}

	if _, err := NormalizeAPIKeyScopes([]string{"users:read", "bogus:scope"}); !errors.Is(err, ErrAPIKeyInvalidScope) {
		t.Fatalf("unknown scope should be ErrAPIKeyInvalidScope, got %v", err)
	}
	if _, err := NormalizeAPIKeyScopes(nil); !errors.Is(err, ErrAPIKeyInvalidScope) {
		t.Fatalf("empty scopes should be ErrAPIKeyInvalidScope, got %v", err)
	}
}

func TestAPIKeyServiceLifecycle(t *testing.T) {
	ctx := context.Background()
	store := newOpsServiceTestStore(t)
	svc := NewAPIKeyService(store)

	plain, meta, err := svc.Create(ctx, CreateAPIKeyInput{
		Name:   "ci-reader",
		Scopes: []string{"users:read", "traffic:read"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(plain, "nk_") {
		t.Fatalf("plaintext key %q missing nk_ prefix", plain)
	}
	if meta.Scopes != "traffic:read,users:read" {
		t.Fatalf("stored scopes = %q", meta.Scopes)
	}

	// Plaintext must validate; metadata must never carry it.
	if _, ok, err := store.ValidateAPIKey(ctx, plain); err != nil || !ok {
		t.Fatalf("created key should validate: ok=%v err=%v", ok, err)
	}

	items, err := svc.List(ctx, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].ID != meta.ID || items[0].Name != "ci-reader" {
		t.Fatalf("list items = %+v", items)
	}

	revoked, err := svc.Revoke(ctx, meta.ID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if revoked.RevokedAt == nil {
		t.Fatalf("revoked_at not set: %+v", revoked)
	}
	if _, ok, _ := store.ValidateAPIKey(ctx, plain); ok {
		t.Fatalf("revoked key must not validate")
	}

	// Revoke is idempotent; unknown ids are ErrAPIKeyNotFound.
	if _, err := svc.Revoke(ctx, meta.ID); err != nil {
		t.Fatalf("second revoke should be idempotent: %v", err)
	}
	if _, err := svc.Revoke(ctx, meta.ID+999); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("unknown id should be ErrAPIKeyNotFound, got %v", err)
	}
}

func TestAPIKeyServiceCreateValidation(t *testing.T) {
	ctx := context.Background()
	store := newOpsServiceTestStore(t)
	svc := NewAPIKeyService(store)

	if _, _, err := svc.Create(ctx, CreateAPIKeyInput{Name: "", Scopes: []string{"users:read"}}); err == nil {
		t.Fatalf("empty name should fail")
	}
	if _, _, err := svc.Create(ctx, CreateAPIKeyInput{Name: "x", Scopes: []string{"nope"}}); !errors.Is(err, ErrAPIKeyInvalidScope) {
		t.Fatalf("invalid scope should fail, got %v", err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	if _, _, err := svc.Create(ctx, CreateAPIKeyInput{Name: "x", Scopes: []string{"users:read"}, ExpiresAt: &past}); err == nil {
		t.Fatalf("past expiry should fail")
	}
	missingNode := int64(424242)
	if _, _, err := svc.Create(ctx, CreateAPIKeyInput{Name: "x", Scopes: []string{"usage:write"}, NodeID: &missingNode}); err == nil {
		t.Fatalf("unknown node binding should fail")
	}
}
