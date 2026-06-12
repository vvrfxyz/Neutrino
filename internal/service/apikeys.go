package service

import (
	"context"
	"database/sql"
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
	ErrAPIKeyInvalidInput = errors.New("invalid api key input")
	ErrAPIKeyNotFound     = errors.New("api key not found")
)

// APIKeyStore is the narrow repo surface APIKeyService depends on. GetNode is
// used to validate the optional node binding at create time. *repo.Store
// satisfies it.
type APIKeyStore interface {
	CreateAPIKey(ctx context.Context, name, scopes string, nodeID *int64, expiresAt *time.Time) (string, repo.APIKey, error)
	ListAPIKeys(ctx context.Context, limit int) ([]repo.APIKey, error)
	GetAPIKey(ctx context.Context, id int64) (repo.APIKey, error)
	RevokeAPIKey(ctx context.Context, id int64) error
	GetNode(ctx context.Context, nodeID int64) (repo.Node, error)
}

var _ APIKeyStore = (*repo.Store)(nil)

// APIKeyService owns API key lifecycle: create (returns the plaintext exactly
// once), list/get metadata, and revoke. Keys are stored as SHA-256 hashes;
// no method ever returns the hash or plaintext after creation.
type APIKeyService struct {
	store APIKeyStore
}

func NewAPIKeyService(store APIKeyStore) *APIKeyService {
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
		return "", repo.APIKey{}, fmt.Errorf("%w: name required", ErrAPIKeyInvalidInput)
	}
	if len(name) > 128 {
		return "", repo.APIKey{}, fmt.Errorf("%w: name longer than 128 characters", ErrAPIKeyInvalidInput)
	}
	scopes, err := NormalizeAPIKeyScopes(in.Scopes)
	if err != nil {
		return "", repo.APIKey{}, err
	}
	if in.ExpiresAt != nil {
		if !in.ExpiresAt.After(time.Now().UTC()) {
			return "", repo.APIKey{}, fmt.Errorf("%w: expires_at must be in the future", ErrAPIKeyInvalidInput)
		}
		// RFC3339 cannot round-trip years above 9999: the value would store
		// fine but fail to parse on read, silently turning the key into a
		// never-expiring one.
		if in.ExpiresAt.Year() > 9999 {
			return "", repo.APIKey{}, fmt.Errorf("%w: expires_at year must be <= 9999", ErrAPIKeyInvalidInput)
		}
	}
	if in.NodeID != nil && *in.NodeID <= 0 {
		in.NodeID = nil
	}
	if in.NodeID != nil {
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
		if errors.Is(err, sql.ErrNoRows) {
			return repo.APIKey{}, fmt.Errorf("%w: %d", ErrAPIKeyNotFound, id)
		}
		return repo.APIKey{}, err
	}
	return k, nil
}

// Revoke marks the key revoked (idempotent for already-revoked keys; unknown
// ids return ErrAPIKeyNotFound). The returned metadata always carries a
// revoked_at so callers can audit even if a re-read would fail.
func (s *APIKeyService) Revoke(ctx context.Context, id int64) (repo.APIKey, error) {
	k, err := s.Get(ctx, id)
	if err != nil {
		return repo.APIKey{}, err
	}
	if k.RevokedAt != nil {
		return k, nil
	}
	if err := s.store.RevokeAPIKey(ctx, id); err != nil && !errors.Is(err, sql.ErrNoRows) {
		// sql.ErrNoRows here means a concurrent revoke won the race — the key
		// is revoked either way.
		return repo.APIKey{}, err
	}
	if fresh, err := s.store.GetAPIKey(ctx, id); err == nil {
		return fresh, nil
	}
	// Re-read failed but the revoke committed: report it as revoked now.
	now := time.Now().UTC()
	k.RevokedAt = &now
	return k, nil
}
