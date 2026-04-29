package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// UpdateNodeLastSeen updates the node's last_seen_at and last_error fields
func (s *Store) UpdateNodeLastSeen(ctx context.Context, nodeID int64, at time.Time, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
	UPDATE nodes
	SET last_seen_at = ?, last_error = ?, updated_at = ?
	WHERE id = ?;
	`, at.Format(time.RFC3339), nullableString(errMsg), time.Now().UTC().Format(time.RFC3339), nodeID)
	return err
}

// UpdateNodeAgentStatus updates last_error, and optionally last_seen_at (only on successful contact).
func (s *Store) UpdateNodeAgentStatus(ctx context.Context, nodeID int64, lastSeenAt *time.Time, errMsg string) error {
	var seen sql.NullString
	if lastSeenAt != nil && !lastSeenAt.IsZero() {
		seen = sql.NullString{String: lastSeenAt.UTC().Format(time.RFC3339), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
	UPDATE nodes
	SET last_seen_at = COALESCE(?, last_seen_at),
		last_error = ?,
		updated_at = ?
	WHERE id = ?;
	`, seen, nullableString(errMsg), time.Now().UTC().Format(time.RFC3339), nodeID)
	return err
}

// GetNode returns a single node by ID
func (s *Store) GetNode(ctx context.Context, nodeID int64) (Node, error) {
	var n Node
	var enabled int
	var managed int
	var createdAt, updatedAt string
	var agentURL sql.NullString
	var lastSeenAt sql.NullString
	var lastError sql.NullString
	var observedIP sql.NullString
	var desiredUsersVersion sql.NullString
	var appliedUsersVersion sql.NullString
	var desiredXrayVersion sql.NullString
	var appliedXrayVersion sql.NullString
	var lastJobErrKind sql.NullString
	var lastJobErrAt sql.NullString

	err := s.db.QueryRowContext(ctx, `
SELECT
	id, name, core_type, protocol, host, port, transport, security, flow, sni, fp, public_key, short_id,
	extra_json, enabled, managed, agent_url, last_seen_at, last_error,
	observed_ip,
	desired_users_version, applied_users_version, desired_xray_version, applied_xray_version, last_job_error_kind, last_job_error_at,
	created_at, updated_at
FROM nodes
WHERE id = ?
LIMIT 1;
`, nodeID).Scan(
		&n.ID, &n.Name, &n.CoreType, &n.Protocol, &n.Host, &n.Port, &n.Transport, &n.Security, &n.Flow, &n.SNI, &n.FP, &n.PublicKey, &n.ShortID,
		&n.ExtraJSON, &enabled, &managed, &agentURL, &lastSeenAt, &lastError,
		&observedIP,
		&desiredUsersVersion, &appliedUsersVersion, &desiredXrayVersion, &appliedXrayVersion, &lastJobErrKind, &lastJobErrAt,
		&createdAt, &updatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Node{}, fmt.Errorf("node not found")
		}
		return Node{}, err
	}

	n.Enabled = enabled == 1
	n.Managed = managed == 1
	if agentURL.Valid {
		n.AgentURL = agentURL.String
	}
	if lastSeenAt.Valid && lastSeenAt.String != "" {
		if t, parseErr := time.Parse(time.RFC3339, lastSeenAt.String); parseErr == nil {
			n.LastSeenAt = &t
		}
	}
	if lastError.Valid {
		n.LastError = lastError.String
	}
	if observedIP.Valid {
		n.ObservedIP = observedIP.String
	}
	if desiredUsersVersion.Valid {
		n.DesiredUsersVersion = desiredUsersVersion.String
	}
	if appliedUsersVersion.Valid {
		n.AppliedUsersVersion = appliedUsersVersion.String
	}
	if desiredXrayVersion.Valid {
		n.DesiredXrayVersion = desiredXrayVersion.String
	}
	if appliedXrayVersion.Valid {
		n.AppliedXrayVersion = appliedXrayVersion.String
	}
	if lastJobErrKind.Valid {
		n.LastJobErrorKind = lastJobErrKind.String
	}
	if lastJobErrAt.Valid && lastJobErrAt.String != "" {
		if t, parseErr := time.Parse(time.RFC3339, lastJobErrAt.String); parseErr == nil {
			n.LastJobErrorAt = &t
		}
	}
	n.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	n.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return n, nil
}

func normalizeNodeInput(in CreateNodeInput) (CreateNodeInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.CoreType = strings.TrimSpace(in.CoreType)
	in.Protocol = strings.TrimSpace(in.Protocol)
	in.Host = strings.TrimSpace(in.Host)
	in.Transport = strings.TrimSpace(in.Transport)
	in.Security = strings.TrimSpace(in.Security)
	in.Flow = strings.TrimSpace(in.Flow)
	in.SNI = strings.TrimSpace(in.SNI)
	in.FP = strings.TrimSpace(in.FP)
	in.PublicKey = strings.TrimSpace(in.PublicKey)
	in.ShortID = strings.TrimSpace(in.ShortID)
	in.AgentURL = strings.TrimSpace(in.AgentURL)
	// host can be empty (will fall back to observed_ip for subscription rendering)
	if in.Name == "" {
		return in, errors.New("invalid node payload")
	}
	if in.CoreType == "" {
		in.CoreType = "xray"
	}
	if in.Protocol == "" {
		in.Protocol = "vless_reality"
	}
	if in.CoreType != "xray" && in.CoreType != "singbox" {
		return in, errors.New("unsupported core_type")
	}
	if in.Protocol != "vless_reality" && in.Protocol != "hysteria2" && in.Protocol != "tuic" {
		return in, errors.New("unsupported protocol")
	}

	// Default ports: keep admin ergonomics and firewall simple.
	// vless_reality: 443/tcp
	// hysteria2/tuic: 8443/udp (protocol implies transport; stored port is the public connect port for subscription)
	if in.Port <= 0 {
		switch in.Protocol {
		case "vless_reality":
			in.Port = 443
		case "hysteria2", "tuic":
			in.Port = 8443
		default:
			in.Port = 443
		}
	}

	// Sensible defaults for vless+reality (subscription rendering will also default, but store consistent values).
	if in.Protocol == "vless_reality" {
		if in.Transport == "" {
			in.Transport = "tcp"
		}
		if in.Security == "" {
			in.Security = "reality"
		}
		if in.Flow == "" {
			in.Flow = "xtls-rprx-vision"
		}
		if in.FP == "" {
			in.FP = "chrome"
		}
	}

	if strings.TrimSpace(in.ExtraJSON) != "" {
		var check map[string]any
		if err := json.Unmarshal([]byte(in.ExtraJSON), &check); err != nil {
			return in, errors.New("invalid extra_json")
		}
	}
	return in, nil
}

type nodeScanner interface {
	Scan(dest ...any) error
}

func scanNode(row nodeScanner) (Node, error) {
	var n Node
	var enabled int
	var managed int
	var createdAt, updatedAt string
	var agentURL sql.NullString
	var lastSeenAt sql.NullString
	var lastError sql.NullString
	var observedIP sql.NullString
	var desiredUsersVersion sql.NullString
	var appliedUsersVersion sql.NullString
	var desiredXrayVersion sql.NullString
	var appliedXrayVersion sql.NullString
	var lastJobErrKind sql.NullString
	var lastJobErrAt sql.NullString
	if err := row.Scan(
		&n.ID, &n.Name, &n.CoreType, &n.Protocol, &n.Host, &n.Port, &n.Transport, &n.Security, &n.Flow, &n.SNI, &n.FP, &n.PublicKey, &n.ShortID,
		&n.ExtraJSON, &enabled, &managed, &agentURL, &lastSeenAt, &lastError,
		&observedIP,
		&desiredUsersVersion, &appliedUsersVersion, &desiredXrayVersion, &appliedXrayVersion, &lastJobErrKind, &lastJobErrAt,
		&createdAt, &updatedAt,
	); err != nil {
		return Node{}, err
	}
	n.Enabled = enabled == 1
	n.Managed = managed == 1
	if agentURL.Valid {
		n.AgentURL = agentURL.String
	}
	if lastSeenAt.Valid && lastSeenAt.String != "" {
		if t, err := time.Parse(time.RFC3339, lastSeenAt.String); err == nil {
			n.LastSeenAt = &t
		}
	}
	if lastError.Valid {
		n.LastError = lastError.String
	}
	if observedIP.Valid {
		n.ObservedIP = observedIP.String
	}
	if desiredUsersVersion.Valid {
		n.DesiredUsersVersion = desiredUsersVersion.String
	}
	if appliedUsersVersion.Valid {
		n.AppliedUsersVersion = appliedUsersVersion.String
	}
	if desiredXrayVersion.Valid {
		n.DesiredXrayVersion = desiredXrayVersion.String
	}
	if appliedXrayVersion.Valid {
		n.AppliedXrayVersion = appliedXrayVersion.String
	}
	if lastJobErrKind.Valid {
		n.LastJobErrorKind = lastJobErrKind.String
	}
	if lastJobErrAt.Valid && lastJobErrAt.String != "" {
		if t, err := time.Parse(time.RFC3339, lastJobErrAt.String); err == nil {
			n.LastJobErrorAt = &t
		}
	}
	n.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	n.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return n, nil
}

func (s *Store) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx, `
	SELECT
		id, name, core_type, protocol, host, port, transport, security, flow, sni, fp, public_key, short_id,
		extra_json, enabled, managed, agent_url, last_seen_at, last_error,
		observed_ip,
		desired_users_version, applied_users_version, desired_xray_version, applied_xray_version, last_job_error_kind, last_job_error_at,
		created_at, updated_at
	FROM nodes
	ORDER BY id DESC;
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Node, 0, 8)
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) ListEnabledNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx, `
	SELECT
		id, name, core_type, protocol, host, port, transport, security, flow, sni, fp, public_key, short_id,
		extra_json, enabled, managed, agent_url, last_seen_at, last_error,
		observed_ip,
		desired_users_version, applied_users_version, desired_xray_version, applied_xray_version, last_job_error_kind, last_job_error_at,
		created_at, updated_at
	FROM nodes
	WHERE enabled = 1
	ORDER BY id ASC;
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Node, 0, 8)
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) ListManagedNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx, `
	SELECT
		id, name, core_type, protocol, host, port, transport, security, flow, sni, fp, public_key, short_id,
		extra_json, enabled, managed, agent_url, last_seen_at, last_error,
		observed_ip,
		desired_users_version, applied_users_version, desired_xray_version, applied_xray_version, last_job_error_kind, last_job_error_at,
		created_at, updated_at
	FROM nodes
	WHERE enabled = 1 AND managed = 1
	ORDER BY id ASC;
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Node, 0, 8)
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) CreateNode(ctx context.Context, in CreateNodeInput) (Node, error) {
	in, err := normalizeNodeInput(in)
	if err != nil {
		return Node{}, err
	}
	enabledInt := 0
	if in.Enabled {
		enabledInt = 1
	}
	managedInt := 0
	if in.Managed {
		managedInt = 1
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var id int64
	err = s.db.QueryRowContext(ctx, `
	INSERT INTO nodes(name, core_type, protocol, host, port, transport, security, flow, sni, fp, public_key, short_id, extra_json, enabled, managed, agent_url, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	RETURNING id;
	`, in.Name, in.CoreType, in.Protocol, in.Host, in.Port, in.Transport, in.Security, in.Flow, in.SNI, in.FP, in.PublicKey, in.ShortID, in.ExtraJSON, enabledInt, managedInt, nullableString(in.AgentURL), now, now).Scan(&id)
	if err != nil {
		return Node{}, err
	}
	items, err := s.ListNodes(ctx)
	if err != nil {
		return Node{}, err
	}
	for _, n := range items {
		if n.ID == id {
			return n, nil
		}
	}
	return Node{}, sql.ErrNoRows
}

func (s *Store) UpdateNode(ctx context.Context, id int64, in CreateNodeInput) (Node, error) {
	in, err := normalizeNodeInput(in)
	if err != nil {
		return Node{}, err
	}
	enabledInt := 0
	if in.Enabled {
		enabledInt = 1
	}
	managedInt := 0
	if in.Managed {
		managedInt = 1
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(ctx, `
	UPDATE nodes
	SET name = ?, core_type = ?, protocol = ?, host = ?, port = ?, transport = ?, security = ?, flow = ?, sni = ?, fp = ?, public_key = ?, short_id = ?, extra_json = ?, enabled = ?, managed = ?, agent_url = ?, updated_at = ?
	WHERE id = ?;
	`, in.Name, in.CoreType, in.Protocol, in.Host, in.Port, in.Transport, in.Security, in.Flow, in.SNI, in.FP, in.PublicKey, in.ShortID, in.ExtraJSON, enabledInt, managedInt, nullableString(in.AgentURL), now, id)
	if err != nil {
		return Node{}, err
	}
	items, err := s.ListNodes(ctx)
	if err != nil {
		return Node{}, err
	}
	for _, n := range items {
		if n.ID == id {
			return n, nil
		}
	}
	return Node{}, sql.ErrNoRows
}

func (s *Store) DeleteNode(ctx context.Context, id int64) error {
	if id <= 0 {
		return ErrNodeNotFound
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id = ? LIMIT 1`, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNodeNotFound
		}
		return err
	}

	affectedUsers, err := s.listNodeDeleteBlockedUsernamesTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if len(affectedUsers) > 0 {
		return ErrNodeDeleteWouldWidenAccess
	}
	if _, err := tx.ExecContext(ctx, `
	UPDATE api_keys
	SET revoked_at = COALESCE(revoked_at, ?)
	WHERE node_id = ?;
	`, time.Now().UTC().Format(time.RFC3339), id); err != nil {
		return err
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNodeNotFound
	}
	return tx.Commit()
}

func (s *Store) NodeDeleteWouldWidenAccess(ctx context.Context, id int64) (bool, error) {
	if id <= 0 {
		return false, ErrNodeNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM nodes WHERE id = ? LIMIT 1`, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNodeNotFound
		}
		return false, err
	}
	affectedUsers, err := s.listNodeDeleteBlockedUsernamesTx(ctx, tx, id)
	if err != nil {
		return false, err
	}
	return len(affectedUsers) > 0, nil
}

func (s *Store) listNodeDeleteBlockedUsernamesTx(ctx context.Context, tx *sql.Tx, nodeID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT u.username
FROM user_node_access una
JOIN users u ON u.id = una.user_id
WHERE una.node_id = ?
  AND (SELECT COUNT(*) FROM user_node_access WHERE user_id = una.user_id) = 1
ORDER BY u.id ASC;
`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	affectedUsers := make([]string, 0, 4)
	for rows.Next() {
		var username string
		if err := rows.Scan(&username); err != nil {
			return nil, err
		}
		affectedUsers = append(affectedUsers, username)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return affectedUsers, nil
}

func (s *Store) DeleteStaleNodes(ctx context.Context, cutoff time.Time) ([]int64, error) {
	if cutoff.IsZero() {
		return nil, nil
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT id
FROM nodes
WHERE enabled = 1
  AND last_seen_at IS NOT NULL
  AND last_seen_at != ''
  AND last_seen_at < ?;
`, cutoff.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]int64, 0, 8)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	disabled := make([]int64, 0, len(ids))
	for _, id := range ids {
		blocked, err := s.NodeDeleteWouldWidenAccess(ctx, id)
		if err != nil {
			return nil, err
		}
		if blocked {
			continue
		}
		if err := s.SetNodeEnabled(ctx, id, false); err != nil {
			if errors.Is(err, ErrNodeNotFound) {
				continue
			}
			return nil, err
		}
		disabled = append(disabled, id)
	}
	return disabled, nil
}
