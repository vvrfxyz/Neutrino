package repo

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

type APIKeyAuth struct {
	ID     int64
	NodeID *int64
	Scopes map[string]struct{}
}

func hashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (s *Store) CreateAPIKey(ctx context.Context, name, scopes string, nodeID *int64, expiresAt *time.Time) (string, APIKey, error) {
	name = strings.TrimSpace(name)
	scopes = strings.TrimSpace(scopes)
	if name == "" || scopes == "" {
		return "", APIKey{}, errors.New("name and scopes required")
	}
	raw, err := randomTokenHex(32)
	if err != nil {
		return "", APIKey{}, err
	}
	plain := "nk_" + raw
	hash := hashAPIKey(plain)
	now := time.Now().UTC().Format(time.RFC3339)
	var exp sql.NullString
	if expiresAt != nil {
		exp = sql.NullString{String: expiresAt.UTC().Format(time.RFC3339), Valid: true}
	}
	var node sql.NullInt64
	if nodeID != nil && *nodeID > 0 {
		node = sql.NullInt64{Int64: *nodeID, Valid: true}
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `
	INSERT INTO api_keys(name, key_hash, scopes, node_id, expires_at, created_at)
	VALUES (?, ?, ?, ?, ?, ?)
	RETURNING id;
	`, name, hash, scopes, node, exp, now).Scan(&id)
	if err != nil {
		return "", APIKey{}, err
	}
	meta := APIKey{ID: id, Name: name, Scopes: scopes, NodeID: nodeID, ExpiresAt: expiresAt, CreatedAt: time.Now().UTC()}
	return plain, meta, nil
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, scopes, node_id, expires_at, last_used_at, revoked_at, created_at
FROM api_keys
ORDER BY id DESC;
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]APIKey, 0, 8)
	for rows.Next() {
		var k APIKey
		var nodeID sql.NullInt64
		var exp, lastUsed, revoked sql.NullString
		var createdAt string
		if err := rows.Scan(&k.ID, &k.Name, &k.Scopes, &nodeID, &exp, &lastUsed, &revoked, &createdAt); err != nil {
			return nil, err
		}
		if nodeID.Valid {
			k.NodeID = &nodeID.Int64
		}
		k.ExpiresAt = parseOptionalRFC3339(exp)
		k.LastUsedAt = parseOptionalRFC3339(lastUsed)
		k.RevokedAt = parseOptionalRFC3339(revoked)
		k.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) RevokeAPIKey(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE api_keys
SET revoked_at = ?
WHERE id = ?;
`, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

func (s *Store) ValidateAPIKey(ctx context.Context, raw string) (APIKeyAuth, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return APIKeyAuth{}, false, nil
	}
	hash := hashAPIKey(raw)
	var id int64
	var scopes string
	var expiresAt sql.NullString
	var revokedAt sql.NullString
	var nodeID sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
SELECT id, scopes, expires_at, revoked_at, node_id
FROM api_keys
WHERE key_hash = ?
LIMIT 1;
`, hash).Scan(&id, &scopes, &expiresAt, &revokedAt, &nodeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return APIKeyAuth{}, false, nil
		}
		return APIKeyAuth{}, false, err
	}
	if revokedAt.Valid {
		return APIKeyAuth{}, false, nil
	}
	exp := parseOptionalRFC3339(expiresAt)
	if exp != nil && time.Now().UTC().After(*exp) {
		return APIKeyAuth{}, false, nil
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), id)

	scopeMap := map[string]struct{}{}
	for _, part := range strings.Split(scopes, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			scopeMap[p] = struct{}{}
		}
	}

	auth := APIKeyAuth{ID: id, Scopes: scopeMap}
	if nodeID.Valid {
		auth.NodeID = &nodeID.Int64
	}
	return auth, true, nil
}
