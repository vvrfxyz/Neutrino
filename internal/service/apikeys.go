package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"neutrino/internal/repo"
)

// KnownAPIKeyScopes is the authoritative set of scopes that requiredScope in
// internal/app/auth.go can demand, plus the wildcard grants. Creating a key
// with an unknown scope is rejected so typos do not mint dead keys.
var KnownAPIKeyScopes = []string{
	"users:read", "users:write",
	"nodes:read", "nodes:write", "nodes:report",
	"usage:write",
	"traffic:read",
	"online:read",
	"metrics:read",
	"backups:read", "backups:write",
	"admin", "*",
}

var (
	ErrAPIKeyInvalidScope = errors.New("invalid api key scope")
	ErrAPIKeyNotFound     = errors.New("api key not found")
)

// APIKeyService owns API key lifecycle: create (returns the plaintext exactly
// once), list/get metadata, and revoke. Keys are stored as SHA-256 hashes;
// no method ever returns the hash or plaintext after creation.
type APIKeyService struct {
	store *repo.Store
}

func NewAPIKeyService(store *repo.Store) *APIKeyService {
	return &APIKeyService{store: store}
}

type CreateAPIKeyInput struct {
	Name      string
	Scopes    []string
	NodeID    *int64
	ExpiresAt *time.Time
}

// NormalizeAPIKeyScopes dedupes, trims, sorts, and validates scopes against
// KnownAPIKeyScopes. Returns the canonical comma-joined form.
func NormalizeAPIKeyScopes(scopes []string) (string, error) {
	known := make(map[string]struct{}, len(KnownAPIKeyScopes))
	for _, s := range KnownAPIKeyScopes {
		known[s] = struct{}{}
	}
	seen := make(map[string]struct{}, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, raw := range scopes {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if _, ok := known[s]; !ok {
			return "", fmt.Errorf("%w: %q", ErrAPIKeyInvalidScope, s)
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("%w: at least one scope required", ErrAPIKeyInvalidScope)
	}
	sort.Strings(out)
	return strings.Join(out, ","), nil
}

// Create mints a new key and returns the plaintext (shown exactly once) plus
// the stored metadata.
func (s *APIKeyService) Create(ctx context.Context, in CreateAPIKeyInput) (string, repo.APIKey, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return "", repo.APIKey{}, errors.New("name required")
	}
	scopes, err := NormalizeAPIKeyScopes(in.Scopes)
	if err != nil {
		return "", repo.APIKey{}, err
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(time.Now().UTC()) {
		return "", repo.APIKey{}, errors.New("expires_at must be in the future")
	}
	if in.NodeID != nil && *in.NodeID > 0 {
		if _, err := s.store.GetNode(ctx, *in.NodeID); err != nil {
			return "", repo.APIKey{}, err
		}
	}
	return s.store.CreateAPIKey(ctx, name, scopes, in.NodeID, in.ExpiresAt)
}

func (s *APIKeyService) List(ctx context.Context, limit int) ([]repo.APIKey, error) {
	return s.store.ListAPIKeys(ctx, limit)
}

func (s *APIKeyService) Get(ctx context.Context, id int64) (repo.APIKey, error) {
	k, err := s.store.GetAPIKey(ctx, id)
	if err != nil {
		return repo.APIKey{}, fmt.Errorf("%w: %d", ErrAPIKeyNotFound, id)
	}
	return k, nil
}

// Revoke marks the key revoked (idempotent for already-revoked keys; unknown
// ids return ErrAPIKeyNotFound).
func (s *APIKeyService) Revoke(ctx context.Context, id int64) (repo.APIKey, error) {
	k, err := s.store.GetAPIKey(ctx, id)
	if err != nil {
		return repo.APIKey{}, fmt.Errorf("%w: %d", ErrAPIKeyNotFound, id)
	}
	if k.RevokedAt != nil {
		return k, nil
	}
	if err := s.store.RevokeAPIKey(ctx, id); err != nil {
		return repo.APIKey{}, err
	}
	return s.store.GetAPIKey(ctx, id)
}
