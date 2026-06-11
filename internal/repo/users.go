package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// ListUsersForNode returns users allowed on the given node.
// Users with no rows in user_node_access are unrestricted (allowed on all nodes).
// Users with rows are only included if the node is in their allowed list.
func (s *Store) ListUsersForNode(ctx context.Context, nodeID int64) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, usersSelectQuery+`
WHERE NOT EXISTS (SELECT 1 FROM user_node_access WHERE user_id = u.id)
   OR EXISTS (SELECT 1 FROM user_node_access WHERE user_id = u.id AND node_id = ?)
ORDER BY u.id DESC;
`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]User, 0, 16)
	for rows.Next() {
		u, scanErr := scanUserRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, usersSelectQuery+`
ORDER BY u.id DESC;
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := make([]User, 0, 16)
	for rows.Next() {
		u, scanErr := scanUserRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *Store) GetUser(ctx context.Context, userID int64) (User, error) {
	rows, err := s.db.QueryContext(ctx, usersSelectQuery+`
WHERE u.id = ?
LIMIT 1;
`, userID)
	if err != nil {
		return User{}, err
	}
	defer rows.Close()
	if rows.Next() {
		u, scanErr := scanUserRow(rows)
		if scanErr != nil {
			return User{}, scanErr
		}
		return u, nil
	}
	if err := rows.Err(); err != nil {
		return User{}, err
	}
	return User{}, ErrUserNotFound
}

func (s *Store) CreateUser(ctx context.Context, in CreateUserInput) (User, error) {
	var err error
	in, err = normalizeCreateUserInput(in)
	if err != nil {
		return User{}, err
	}

	now := time.Now().UTC()
	expiresAt := now.AddDate(0, 0, in.PlanDays)
	const bytesPerGB = int64(1024 * 1024 * 1024)
	if in.MonthlyLimitGB > math.MaxInt64/bytesPerGB {
		return User{}, fmt.Errorf("monthly_limit_gb is too large")
	}
	monthlyLimitBytes := in.MonthlyLimitGB * bytesPerGB

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()

	var id int64
	if err := tx.QueryRowContext(ctx, `
	INSERT INTO users(username, monthly_limit_bytes, counting_mode, quota_cycle, quota_tz, device_limit, plan_days, status, created_at, expires_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, 'active', ?, ?)
	RETURNING id;
	`, in.Username, monthlyLimitBytes, in.CountingMode, in.QuotaCycle, in.QuotaTZ, in.DeviceLimit, in.PlanDays, now.Format(time.RFC3339), expiresAt.Format(time.RFC3339)).Scan(&id); err != nil {
		return User{}, err
	}

	winStart, winEnd, cycleKey := quotaWindowBounds(in.QuotaCycle, in.QuotaTZ, now)
	if _, err := tx.ExecContext(ctx, `
	INSERT INTO quota_windows(user_id, window_start, window_end, cycle_type, cycle_key, reset_reason, inbound_bytes, outbound_bytes, effective_bytes, credit_bytes)
	VALUES (?, ?, ?, ?, ?, 'create_user', 0, 0, 0, 0)
	ON CONFLICT(user_id, window_start) DO NOTHING;
	`, id, winStart.Format(time.RFC3339), winEnd.Format(time.RFC3339), in.QuotaCycle, cycleKey); err != nil {
		return User{}, err
	}
	if _, err := s.createSubscriptionTokenTx(ctx, tx, id); err != nil {
		return User{}, err
	}
	if _, err := s.ensureTelegramBindingTx(ctx, tx, id); err != nil {
		return User{}, err
	}

	// Ensure the user is immediately usable: create an initial active proxy link.
	//
	// This avoids subscriptions emitting placeholder UUIDs and matches the admin UI expectation.
	linkUUID := uuid.NewString()
	inboundTag := fmt.Sprintf("user-%d", id)
	link := s.buildVLESSLink(linkUUID, in.Username)
	if _, err := tx.ExecContext(ctx, `
	INSERT INTO proxy_links(user_id, protocol, uuid, inbound_tag, link, active, created_at)
	VALUES (?, 'vless-reality-vision', ?, ?, ?, 1, ?);
	`, id, linkUUID, inboundTag, link, now.Format(time.RFC3339)); err != nil {
		return User{}, err
	}

	if err := s.setUserNodeAccessTx(ctx, tx, id, in.NodeIDs); err != nil {
		return User{}, err
	}

	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return s.GetUser(ctx, id)
}

func normalizeCreateUserInput(in CreateUserInput) (CreateUserInput, error) {
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" {
		return CreateUserInput{}, fmt.Errorf("username is required")
	}
	if len(in.Username) > 128 {
		return CreateUserInput{}, fmt.Errorf("username is too long")
	}
	if strings.ContainsFunc(in.Username, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return CreateUserInput{}, fmt.Errorf("username must not contain whitespace or control characters")
	}
	if in.MonthlyLimitGB <= 0 {
		return CreateUserInput{}, fmt.Errorf("monthly_limit_gb must be > 0")
	}
	if in.CountingMode != "single" && in.CountingMode != "double" {
		in.CountingMode = "double"
	}
	if in.PlanDays <= 0 {
		in.PlanDays = 30
	}
	if in.QuotaCycle != "day" && in.QuotaCycle != "week" && in.QuotaCycle != "month" {
		in.QuotaCycle = "month"
	}
	in.QuotaTZ = strings.TrimSpace(in.QuotaTZ)
	if strings.TrimSpace(in.QuotaTZ) == "" {
		in.QuotaTZ = "Asia/Shanghai"
	}
	if _, err := time.LoadLocation(in.QuotaTZ); err != nil {
		return CreateUserInput{}, fmt.Errorf("invalid quota_tz")
	}
	if in.DeviceLimit <= 0 {
		in.DeviceLimit = 2
	}
	return in, nil
}

func (s *Store) CreateProxyLink(ctx context.Context, userID int64) (User, error) {
	if _, err := s.SweepExpiredUsers(ctx); err != nil {
		return User{}, err
	}
	if _, err := s.SweepQuotaWindows(ctx); err != nil {
		return User{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()

	var username, status, expiresAtStr string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT username, status, expires_at FROM users WHERE id = ?`,
		userID,
	).Scan(&username, &status, &expiresAtStr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrUserNotFound
		}
		return User{}, err
	}

	expiresAt, err := time.Parse(time.RFC3339, expiresAtStr)
	if err != nil {
		return User{}, err
	}
	if status != "active" || expiresAt.Before(time.Now().UTC()) {
		return User{}, ErrUserInactive
	}

	linkUUID := uuid.NewString()
	inboundTag := fmt.Sprintf("user-%d", userID)
	link := s.buildVLESSLink(linkUUID, username)
	now := time.Now().UTC().Format(time.RFC3339)

	if _, err := tx.ExecContext(ctx, `UPDATE proxy_links SET active = 0 WHERE user_id = ?`, userID); err != nil {
		return User{}, err
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO proxy_links(user_id, protocol, uuid, inbound_tag, link, active, created_at)
VALUES (?, 'vless-reality-vision', ?, ?, ?, 1, ?)
`, userID, linkUUID, inboundTag, link, now); err != nil {
		return User{}, err
	}

	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return s.GetUser(ctx, userID)
}

func (s *Store) SetUserStatus(ctx context.Context, userID int64, targetStatus string) (User, error) {
	if targetStatus != "active" && targetStatus != "disabled" {
		return User{}, fmt.Errorf("unsupported status: %s", targetStatus)
	}

	if _, err := s.SweepExpiredUsers(ctx); err != nil {
		return User{}, err
	}
	if _, err := s.SweepQuotaWindows(ctx); err != nil {
		return User{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()

	var currentStatus, expiresAtStr string
	if err := tx.QueryRowContext(ctx, `
SELECT status, expires_at
FROM users
WHERE id = ?;
`, userID).Scan(&currentStatus, &expiresAtStr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrUserNotFound
		}
		return User{}, err
	}

	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)

	switch targetStatus {
	case "active":
		expiresAt, err := time.Parse(time.RFC3339, expiresAtStr)
		if err != nil {
			return User{}, err
		}
		if now.After(expiresAt) {
			// The pre-tx SweepExpiredUsers only converts status='active' users,
			// so a disabled/over_limit user whose plan has lapsed reaches here.
			// Persist the truthful 'expired' state (ExtendUserPlan recovers
			// expired users), then report the activation failure.
			if _, err := tx.ExecContext(ctx, `
UPDATE users
SET status = 'expired', removed_at = COALESCE(removed_at, ?)
WHERE id = ?;
`, nowStr, userID); err != nil {
				return User{}, err
			}
			if err := tx.Commit(); err != nil {
				return User{}, err
			}
			return User{}, ErrUserInactive
		}
		if currentStatus != "active" {
			if _, err := tx.ExecContext(ctx, `
UPDATE users
SET status = 'active', removed_at = NULL
WHERE id = ?;
`, userID); err != nil {
				return User{}, err
			}
			// Manual disable deactivates links; re-enable should restore the latest link.
			if err := s.activateLatestProxyLinkTx(ctx, tx, userID); err != nil {
				return User{}, err
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO enforcement_logs(user_id, action, reason, detail, created_at)
VALUES (?, 'enable_user', 'manual', 'manual enable from admin', ?);
`, userID, nowStr); err != nil {
				return User{}, err
			}
		}
	case "disabled":
		if currentStatus != "disabled" {
			if _, err := tx.ExecContext(ctx, `
UPDATE users
SET status = 'disabled', removed_at = ?
WHERE id = ?;
`, nowStr, userID); err != nil {
				return User{}, err
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO enforcement_logs(user_id, action, reason, detail, created_at)
VALUES (?, 'disable_user', 'manual', 'manual disable from admin', ?);
`, userID, nowStr); err != nil {
				return User{}, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE proxy_links SET active = 0 WHERE user_id = ?`, userID); err != nil {
			return User{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return s.GetUser(ctx, userID)
}

func (s *Store) activateLatestProxyLinkTx(ctx context.Context, tx *sql.Tx, userID int64) error {
	if _, err := tx.ExecContext(ctx, `
UPDATE proxy_links
SET active = 0
WHERE user_id = ?;
`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE proxy_links
SET active = 1
WHERE id = (
	SELECT id
	FROM proxy_links
	WHERE user_id = ?
	ORDER BY id DESC
	LIMIT 1
);
`, userID); err != nil {
		return err
	}
	return nil
}

func (s *Store) DeleteUser(ctx context.Context, userID int64) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var username string
	if err := tx.QueryRowContext(ctx, `
SELECT username
FROM users
WHERE id = ?;
`, userID).Scan(&username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrUserNotFound
		}
		return "", err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?;`, userID); err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	return username, nil
}

func (s *Store) buildVLESSLink(clientUUID, username string) string {
	v := url.Values{}
	v.Set("encryption", "none")
	v.Set("type", s.cfg.ProxyType)
	v.Set("security", s.cfg.ProxySecurity)
	v.Set("flow", s.cfg.ProxyFlow)
	v.Set("sni", s.cfg.ProxySNI)
	v.Set("fp", s.cfg.RealityFP)
	v.Set("pbk", s.cfg.RealityPublicK)
	v.Set("sid", s.cfg.RealityShortID)

	return "vless://" + clientUUID + "@" + s.cfg.ProxyHost + ":" + strconv.Itoa(s.cfg.ProxyPort) + "?" + v.Encode() + "#" + url.QueryEscape(username)
}

func (s *Store) GetUserByUsername(ctx context.Context, username string) (User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return User{}, ErrUserNotFound
	}
	rows, err := s.db.QueryContext(ctx, usersSelectQuery+`
WHERE u.username = ?
LIMIT 1;
`, username)
	if err != nil {
		return User{}, err
	}
	defer rows.Close()
	if rows.Next() {
		u, scanErr := scanUserRow(rows)
		if scanErr != nil {
			return User{}, scanErr
		}
		return u, nil
	}
	if err := rows.Err(); err != nil {
		return User{}, err
	}
	return User{}, ErrUserNotFound
}

// ── user-node access control ─────────────────────────────────────────

func normalizeNodeIDs(nodeIDs []int64) []int64 {
	if len(nodeIDs) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(nodeIDs))
	out := make([]int64, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if nodeID <= 0 {
			continue
		}
		if _, ok := seen[nodeID]; ok {
			continue
		}
		seen[nodeID] = struct{}{}
		out = append(out, nodeID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func validateNodeIDsTx(ctx context.Context, tx *sql.Tx, nodeIDs []int64) ([]int64, error) {
	nodeIDs = normalizeNodeIDs(nodeIDs)
	if len(nodeIDs) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(nodeIDs))
	args := make([]any, 0, len(nodeIDs))
	for i, nodeID := range nodeIDs {
		placeholders[i] = "?"
		args = append(args, nodeID)
	}

	rows, err := tx.QueryContext(ctx, `
SELECT id
FROM nodes
WHERE id IN (`+strings.Join(placeholders, ",")+`)
ORDER BY id ASC;
`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	found := make([]int64, 0, len(nodeIDs))
	for rows.Next() {
		var nodeID int64
		if err := rows.Scan(&nodeID); err != nil {
			return nil, err
		}
		found = append(found, nodeID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(found) != len(nodeIDs) {
		foundSet := make(map[int64]struct{}, len(found))
		for _, nodeID := range found {
			foundSet[nodeID] = struct{}{}
		}
		for _, nodeID := range nodeIDs {
			if _, ok := foundSet[nodeID]; !ok {
				return nil, fmt.Errorf("%w: %d", ErrNodeNotFound, nodeID)
			}
		}
	}
	return nodeIDs, nil
}

// SetUserNodeAccess replaces the full set of allowed node IDs for a user.
// An empty slice means "all nodes" (no restriction).
func (s *Store) SetUserNodeAccess(ctx context.Context, userID int64, nodeIDs []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ? LIMIT 1`, userID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		return err
	}
	nodeIDs, err = validateNodeIDsTx(ctx, tx, nodeIDs)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_node_access WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if len(nodeIDs) > 0 {
		now := time.Now().UTC().Format(time.RFC3339)
		for _, nid := range nodeIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO user_node_access(user_id, node_id, created_at) VALUES (?, ?, ?)`, userID, nid, now); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// setUserNodeAccessTx is the in-transaction variant used during CreateUser.
func (s *Store) setUserNodeAccessTx(ctx context.Context, tx *sql.Tx, userID int64, nodeIDs []int64) error {
	nodeIDs, err := validateNodeIDsTx(ctx, tx, nodeIDs)
	if err != nil {
		return err
	}
	if len(nodeIDs) == 0 {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, nid := range nodeIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_node_access(user_id, node_id, created_at) VALUES (?, ?, ?)`, userID, nid, now); err != nil {
			return err
		}
	}
	return nil
}

// ListUserNodeIDs returns the node IDs the user is restricted to.
// An empty slice means no restriction (all nodes allowed).
func (s *Store) ListUserNodeIDs(ctx context.Context, userID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id FROM user_node_access WHERE user_id = ? ORDER BY node_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListEnabledNodesForUser returns enabled nodes filtered by the user's access list.
// If the user has no access rows, all enabled nodes are returned.
func (s *Store) ListEnabledNodesForUser(ctx context.Context, userID int64) ([]Node, error) {
	nodeIDs, err := s.ListUserNodeIDs(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(nodeIDs) == 0 {
		return s.ListEnabledNodes(ctx)
	}
	placeholders := make([]string, len(nodeIDs))
	args := make([]any, 0, len(nodeIDs)+1)
	for i, id := range nodeIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := `
	SELECT
		id, name, core_type, protocol, host, port, transport, security, flow, sni, fp, public_key, short_id,
		extra_json, enabled, managed, agent_url, last_seen_at, last_error,
		observed_ip,
		desired_users_version, applied_users_version, desired_xray_version, applied_xray_version, last_job_error_kind, last_job_error_at,
		created_at, updated_at
	FROM nodes
	WHERE enabled = 1 AND id IN (` + strings.Join(placeholders, ",") + `)
	ORDER BY id ASC;
	`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Node, 0, len(nodeIDs))
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
