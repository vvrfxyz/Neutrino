package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"neutrino/internal/usersync"
)

// usersSyncQuerier is the *sql.DB / *sql.Tx subset the users-sync helpers
// need. Every helper below must work inside a caller-owned transaction so
// finish/backfill paths can compose prepare + follow-ups atomically.
type usersSyncQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Users-sync job reasons. Protected reasons mark forced-full jobs that repair
// runtime state: redundant-job cleanup must never cancel them even when the
// job's desired version already equals the node's applied version.
var protectedUsersSyncReasons = map[string]bool{
	"baseline_backfill":             true,
	"xray_apply_followup":           true,
	"xray_apply_rollback_followup":  true,
	"xray_rollback_followup":        true,
	"xray_runtime_unknown_followup": true,
}

// repairCompatibleUsersSyncReasons are full-job reasons that satisfy a
// forceFull request: a pending/running full carrying one of these is already
// the repair the caller wants.
var repairCompatibleUsersSyncReasons = func() map[string]bool {
	m := map[string]bool{
		"repair":                      true,
		"finish_version_mismatch":     true,
		"base_version_mismatch":       true,
		"local_hash_mismatch":         true,
		"remove_base_mismatch":        true,
		"invalid_local_baseline":      true,
		"full_response_hash_mismatch": true,
		"pending_journal_mismatch":    true,
		"invalid_delta_payload":       true,
	}
	for r := range protectedUsersSyncReasons {
		m[r] = true
	}
	return m
}()

type usersSyncPayloadClass int

const (
	usersSyncPayloadLegacyFull usersSyncPayloadClass = iota // "" or "{}": old full contract
	usersSyncPayloadFull
	usersSyncPayloadDelta
	usersSyncPayloadInvalid
)

// classifyUsersSyncPayload decides how a stored users_sync payload behaves for
// overwrite/priority decisions. Unparsable or unknown schema/mode payloads are
// invalid: an ordinary delta must not overwrite them blindly, but forced full
// repair replaces them so they cannot poison the queue.
func classifyUsersSyncPayload(raw string) (usersync.JobPayload, usersSyncPayloadClass) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return usersync.JobPayload{Mode: usersync.ModeFull}, usersSyncPayloadLegacyFull
	}
	var p usersync.JobPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return usersync.JobPayload{}, usersSyncPayloadInvalid
	}
	if p.Schema == usersync.SchemaV1 && p.Mode == usersync.ModeFull && strings.TrimSpace(p.TargetVersion) != "" {
		return p, usersSyncPayloadFull
	}
	if p.Schema == usersync.SchemaV1 && p.Mode == usersync.ModeDelta &&
		strings.TrimSpace(p.BaseVersion) != "" && strings.TrimSpace(p.TargetVersion) != "" && len(p.Changes) > 0 {
		return p, usersSyncPayloadDelta
	}
	// A schema-0 object with fields (e.g. hand-written payload) is invalid,
	// not legacy full: legacy full is exactly empty/{}.
	return p, usersSyncPayloadInvalid
}

// --- snapshots ---

func upsertNodeUserSyncSnapshotExec(ctx context.Context, q usersSyncQuerier, nodeID int64, version string, items []usersync.Item, now string) error {
	sorted := make([]usersync.Item, len(items))
	copy(sorted, items)
	sortItemsByUserID(sorted)
	b, err := json.Marshal(sorted)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `
INSERT INTO node_user_sync_snapshots(node_id, version, users_json, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(node_id, version) DO UPDATE SET created_at = excluded.created_at;
`, nodeID, version, string(b), now)
	return err
}

func sortItemsByUserID(items []usersync.Item) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j-1].UserID > items[j].UserID; j-- {
			items[j-1], items[j] = items[j], items[j-1]
		}
	}
}

func getNodeUserSyncSnapshotExec(ctx context.Context, q usersSyncQuerier, nodeID int64, version string) ([]usersync.Item, bool, error) {
	var usersJSON string
	err := q.QueryRowContext(ctx, `
SELECT users_json FROM node_user_sync_snapshots WHERE node_id = ? AND version = ?;
`, nodeID, version).Scan(&usersJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var items []usersync.Item
	if err := json.Unmarshal([]byte(usersJSON), &items); err != nil {
		return nil, false, fmt.Errorf("decode snapshot node=%d version=%s: %w", nodeID, version, err)
	}
	return items, true, nil
}

func (s *Store) GetNodeUserSyncSnapshot(ctx context.Context, nodeID int64, version string) ([]usersync.Item, bool, error) {
	return getNodeUserSyncSnapshotExec(ctx, s.db, nodeID, version)
}

// PruneNodeUserSyncSnapshots deletes old snapshots while always retaining the
// node's desired/applied versions, every pending/running users_sync job's
// desired version and delta base version, and the newest 10 snapshots. If any
// pending/running payload cannot be parsed, pruning is skipped entirely
// rather than risking deletion of a baseline a job still needs.
func (s *Store) PruneNodeUserSyncSnapshots(ctx context.Context, nodeID int64) error {
	retained := make(map[string]bool, 8)
	var desired, applied sql.NullString
	if err := s.db.QueryRowContext(ctx, `
SELECT desired_users_version, applied_users_version FROM nodes WHERE id = ?;
`, nodeID).Scan(&desired, &applied); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if v := strings.TrimSpace(desired.String); v != "" {
		retained[v] = true
	}
	if v := strings.TrimSpace(applied.String); v != "" {
		retained[v] = true
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(desired_version, ''), payload_json
FROM node_jobs
WHERE node_id = ? AND kind = 'users_sync' AND status IN ('pending','running');
`, nodeID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var jobDesired, payloadJSON string
		if err := rows.Scan(&jobDesired, &payloadJSON); err != nil {
			rows.Close()
			return err
		}
		if v := strings.TrimSpace(jobDesired); v != "" {
			retained[v] = true
		}
		payload, class := classifyUsersSyncPayload(payloadJSON)
		if class == usersSyncPayloadInvalid {
			rows.Close()
			return nil // cannot prove which baseline the job needs; keep everything
		}
		if v := strings.TrimSpace(payload.TargetVersion); v != "" {
			retained[v] = true
		}
		if v := strings.TrimSpace(payload.BaseVersion); v != "" {
			retained[v] = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	args := []any{nodeID, nodeID}
	placeholders := make([]string, 0, len(retained))
	for range retained {
		placeholders = append(placeholders, "?")
	}
	query := `
DELETE FROM node_user_sync_snapshots
WHERE node_id = ?
  AND version NOT IN (
	SELECT version FROM node_user_sync_snapshots
	WHERE node_id = ?
	ORDER BY created_at DESC, version DESC
	LIMIT 10
  )`
	if len(placeholders) > 0 {
		query += `
  AND version NOT IN (` + strings.Join(placeholders, ", ") + `)`
		for v := range retained {
			args = append(args, v)
		}
	}
	query += ";"
	_, err = s.db.ExecContext(ctx, query, args...)
	return err
}

// --- canonical node users query ---

// listNodeUserSyncItemsExec reads the canonical user list for a node inside
// the caller's transaction. Access semantics match ListUsersForNode: users
// without user_node_access rows are unrestricted; restricted users are
// included only for listed nodes.
func listNodeUserSyncItemsExec(ctx context.Context, q usersSyncQuerier, nodeID int64) ([]usersync.Item, error) {
	rows, err := q.QueryContext(ctx, `
SELECT u.id, u.username, u.status, COALESCE(pl.uuid, '')
FROM users u
LEFT JOIN proxy_links pl ON pl.id = (
	SELECT id FROM proxy_links WHERE user_id = u.id AND active = 1 ORDER BY id DESC LIMIT 1
)
WHERE NOT EXISTS (SELECT 1 FROM user_node_access WHERE user_id = u.id)
   OR EXISTS (SELECT 1 FROM user_node_access WHERE user_id = u.id AND node_id = ?)
ORDER BY u.id ASC;
`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]usersync.Item, 0, 16)
	for rows.Next() {
		var it usersync.Item
		if err := rows.Scan(&it.UserID, &it.Email, &it.Status, &it.UUID); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// --- prepare (materialize + mode selection + enqueue) ---

type PrepareUsersSyncResult struct {
	JobID         int64
	Enqueued      bool
	TargetVersion string
	Mode          string // "full" | "delta" | "" when no job was needed
}

// PrepareUsersSync owns the transaction for ordinary service calls. Finish
// and backfill paths must use prepareUsersSyncTx with their own transaction.
func (s *Store) PrepareUsersSync(ctx context.Context, nodeID int64, forceFull bool, reason string) (PrepareUsersSyncResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PrepareUsersSyncResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := s.prepareUsersSyncTx(ctx, tx, nodeID, forceFull, reason)
	if err != nil {
		return PrepareUsersSyncResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return PrepareUsersSyncResult{}, err
	}
	if err := s.PruneNodeUserSyncSnapshots(ctx, nodeID); err != nil {
		log.Printf("prune user sync snapshots node=%d: %v", nodeID, err)
	}
	return res, nil
}

// prepareUsersSyncTx materializes the canonical target snapshot, updates the
// node's desired users version, decides full vs delta, and enqueues the job —
// all on the supplied transaction. It never opens or commits a transaction.
func (s *Store) prepareUsersSyncTx(ctx context.Context, tx usersSyncQuerier, nodeID int64, forceFull bool, reason string) (PrepareUsersSyncResult, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "reconcile"
	}

	var enabled, baselineSchema int
	var appliedNS sql.NullString
	err := tx.QueryRowContext(ctx, `
SELECT enabled, COALESCE(applied_users_version, ''), users_sync_baseline_schema
FROM nodes WHERE id = ?;
`, nodeID).Scan(&enabled, &appliedNS, &baselineSchema)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PrepareUsersSyncResult{}, ErrNodeNotFound
		}
		return PrepareUsersSyncResult{}, err
	}
	applied := strings.TrimSpace(appliedNS.String)

	target := make([]usersync.Item, 0)
	if enabled == 1 {
		target, err = listNodeUserSyncItemsExec(ctx, tx, nodeID)
		if err != nil {
			return PrepareUsersSyncResult{}, err
		}
	}
	if err := usersync.ValidateItems(target); err != nil {
		return PrepareUsersSyncResult{}, fmt.Errorf("canonical users for node %d invalid: %w", nodeID, err)
	}
	targetVersion := usersync.HashItems(target)
	now := time.Now().UTC().Format(time.RFC3339)

	if err := upsertNodeUserSyncSnapshotExec(ctx, tx, nodeID, targetVersion, target, now); err != nil {
		return PrepareUsersSyncResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE nodes SET desired_users_version = ?, updated_at = ? WHERE id = ?;
`, targetVersion, now, nodeID); err != nil {
		return PrepareUsersSyncResult{}, err
	}

	pending, hasPending, err := getPendingUsersSyncJobExec(ctx, tx, nodeID)
	if err != nil {
		return PrepareUsersSyncResult{}, err
	}
	var pendingPayload usersync.JobPayload
	pendingClass := usersSyncPayloadLegacyFull
	if hasPending {
		pendingPayload, pendingClass = classifyUsersSyncPayload(pending.PayloadJSON)
		// An unparsable pending payload would be claimed, fail strangely, and
		// can poison the queue. Ordinary reconcile repairs it with forced full.
		if pendingClass == usersSyncPayloadInvalid && !forceFull {
			log.Printf("users_sync node=%d: invalid pending payload job=%d; forcing full repair", nodeID, pending.ID)
			forceFull = true
			reason = "repair"
		}
	}

	if !forceFull && applied == targetVersion {
		// No drift. A stale pending delta (base no longer applied) would fail
		// immediately after claim; cancel it instead of leaving it claimable.
		if hasPending && pendingClass == usersSyncPayloadDelta && strings.TrimSpace(pendingPayload.BaseVersion) != applied {
			if err := cancelUsersSyncJobExec(ctx, tx, pending.ID, now, "stale delta base"); err != nil {
				return PrepareUsersSyncResult{}, err
			}
		}
		return PrepareUsersSyncResult{TargetVersion: targetVersion}, nil
	}

	mode := usersync.ModeFull
	payload := usersync.JobPayload{
		Schema:        usersync.SchemaV1,
		Mode:          usersync.ModeFull,
		TargetVersion: targetVersion,
		Reason:        reason,
		CreatedAt:     now,
	}
	if !forceFull && baselineSchema >= 1 && applied != "" {
		base, ok, err := getNodeUserSyncSnapshotExec(ctx, tx, nodeID, applied)
		if err != nil {
			return PrepareUsersSyncResult{}, err
		}
		if ok {
			if usersync.HashItems(base) != applied {
				log.Printf("users_sync node=%d: stored snapshot for %s does not hash-match; using full", nodeID, applied)
			} else if changes, safe := usersync.DiffItems(base, target); safe && len(changes) > 0 {
				mode = usersync.ModeDelta
				payload = usersync.JobPayload{
					Schema:        usersync.SchemaV1,
					Mode:          usersync.ModeDelta,
					BaseVersion:   applied,
					TargetVersion: targetVersion,
					Reason:        reason,
					CreatedAt:     now,
					Changes:       changes,
				}
			}
			// safe && len(changes)==0 with applied != target means the stored
			// snapshot and version disagree somewhere; full repairs it without
			// hiding the inconsistency.
		}
	}

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return PrepareUsersSyncResult{}, err
	}
	jobID, enqueued, err := enqueueUsersSyncJobExec(ctx, tx, nodeID, targetVersion, string(payloadJSON), mode, reason, forceFull, now)
	if err != nil {
		return PrepareUsersSyncResult{}, err
	}
	return PrepareUsersSyncResult{JobID: jobID, Enqueued: enqueued, TargetVersion: targetVersion, Mode: mode}, nil
}

type pendingUsersSyncJob struct {
	ID             int64
	DesiredVersion string
	PayloadJSON    string
}

func getPendingUsersSyncJobExec(ctx context.Context, q usersSyncQuerier, nodeID int64) (pendingUsersSyncJob, bool, error) {
	var j pendingUsersSyncJob
	var desired sql.NullString
	err := q.QueryRowContext(ctx, `
SELECT id, desired_version, payload_json
FROM node_jobs
WHERE node_id = ? AND kind = 'users_sync' AND status = 'pending'
ORDER BY id DESC LIMIT 1;
`, nodeID).Scan(&j.ID, &desired, &j.PayloadJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return pendingUsersSyncJob{}, false, nil
		}
		return pendingUsersSyncJob{}, false, err
	}
	j.DesiredVersion = strings.TrimSpace(desired.String)
	return j, true, nil
}

func cancelUsersSyncJobExec(ctx context.Context, q usersSyncQuerier, jobID int64, now, why string) error {
	_, err := q.ExecContext(ctx, `
UPDATE node_jobs
SET status = 'canceled', last_error = ?, finished_at = ?
WHERE id = ? AND status = 'pending';
`, "canceled: "+why, now, jobID)
	return err
}

// enqueueUsersSyncJobExec enforces full-over-delta priority on the single
// pending users_sync slot per node:
//   - a later delta never overwrites a pending full (or legacy/invalid) job;
//   - a newer delta overwrites an older pending delta;
//   - full overwrites pending deltas and invalid payloads;
//   - ordinary full keeps a same-target pending full as-is;
//   - forced full reuses a pending/running full only when it already carries
//     a repair-compatible reason and the same target; otherwise it rewrites
//     the pending slot so the repair cannot be lost.
func enqueueUsersSyncJobExec(ctx context.Context, q usersSyncQuerier, nodeID int64, targetVersion, payloadJSON, mode, reason string, forceFull bool, now string) (jobID int64, enqueued bool, err error) {
	var runningID int64
	var runningDesired sql.NullString
	var runningPayload string
	err = q.QueryRowContext(ctx, `
SELECT id, desired_version, payload_json
FROM node_jobs
WHERE node_id = ? AND kind = 'users_sync' AND status = 'running'
ORDER BY id DESC LIMIT 1;
`, nodeID).Scan(&runningID, &runningDesired, &runningPayload)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	if err == nil && runningID > 0 {
		rPayload, rClass := classifyUsersSyncPayload(runningPayload)
		sameTarget := strings.TrimSpace(runningDesired.String) == targetVersion && targetVersion != ""
		if !forceFull && sameTarget {
			return runningID, false, nil
		}
		if forceFull && sameTarget && rClass == usersSyncPayloadFull && repairCompatibleUsersSyncReasons[strings.TrimSpace(rPayload.Reason)] {
			// The running job is already the repair we want.
			return runningID, false, nil
		}
		// Otherwise fall through: leave/create a pending job behind the
		// running one. A running delta must never satisfy forced full.
	}

	pending, hasPending, err := getPendingUsersSyncJobExec(ctx, q, nodeID)
	if err != nil {
		return 0, false, err
	}
	if hasPending {
		pPayload, pClass := classifyUsersSyncPayload(pending.PayloadJSON)
		newIsDelta := mode == usersync.ModeDelta
		if newIsDelta {
			// Full-over-delta: pending full (legacy or structured) and invalid
			// payloads are never displaced by a delta.
			if pClass != usersSyncPayloadDelta {
				return pending.ID, false, nil
			}
		} else if forceFull {
			if pClass == usersSyncPayloadFull && pending.DesiredVersion == targetVersion &&
				repairCompatibleUsersSyncReasons[strings.TrimSpace(pPayload.Reason)] {
				return pending.ID, false, nil
			}
		} else {
			if (pClass == usersSyncPayloadFull || pClass == usersSyncPayloadLegacyFull) && pending.DesiredVersion == targetVersion {
				return pending.ID, false, nil
			}
		}
		if _, err := q.ExecContext(ctx, `
UPDATE node_jobs
SET desired_version = ?,
	payload_json = ?,
	timeout_sec = 20,
	correlation_id = NULLIF(?, ''),
	attempts = 0,
	retryable = 1,
	http_status = NULL,
	result_json = NULL,
	last_error = NULL,
	started_at = NULL,
	finished_at = NULL,
	created_at = ?
WHERE id = ? AND status = 'pending';
`, targetVersion, payloadJSON, reason, now, pending.ID); err != nil {
			return 0, false, err
		}
		return pending.ID, false, nil
	}

	if err := q.QueryRowContext(ctx, `
INSERT INTO node_jobs(node_id, kind, desired_version, payload_json, status, attempts, retryable, created_at, timeout_sec, correlation_id)
VALUES (?, 'users_sync', ?, ?, 'pending', 0, 1, ?, 20, NULLIF(?, ''))
RETURNING id;
`, nodeID, targetVersion, payloadJSON, now, reason).Scan(&jobID); err != nil {
		return 0, false, err
	}
	return jobID, true, nil
}

// --- finish (terminal transition + follow-up decisions in one transaction) ---

type FinishUsersSyncResult struct {
	FinalStatus        string
	Accepted           bool
	RepairEnqueued     bool
	FollowUpEnqueued   bool
	StaleDeltaCanceled bool
	RedundantCanceled  bool
	DeltaReadyMarked   bool
	DeltaReadyCleared  bool
}

type usersSyncAgentResult struct {
	Mode          string `json:"mode"`
	TargetVersion string `json:"target_version"`
	NeedFullSync  bool   `json:"need_full_sync"`
	Reason        string `json:"reason"`
}

// FinishUsersSyncJobForNode finishes a users_sync job with the same retry
// semantics as FinishNodeJobForNode and runs every follow-up decision in the
// same transaction: applied-version validation, the delta-ready baseline
// marker, stale/redundant pending cleanup, forced full repair, and follow-up
// reconcile. The HTTP handler must not perform best-effort writes after this.
func (s *Store) FinishUsersSyncJobForNode(ctx context.Context, nodeID, jobID int64, in FinishNodeJobInput, appliedVersion string) (FinishUsersSyncResult, error) {
	var out FinishUsersSyncResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()

	// Capture the job's identity before the terminal transition.
	var jobKind, jobPayload string
	var jobDesired sql.NullString
	err = tx.QueryRowContext(ctx, `
SELECT kind, desired_version, payload_json FROM node_jobs WHERE id = ? AND node_id = ?;
`, jobID, nodeID).Scan(&jobKind, &jobDesired, &jobPayload)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, ErrNodeJobNotRunning
		}
		return out, err
	}
	if jobKind != "users_sync" {
		return out, fmt.Errorf("job %d is %s, not users_sync", jobID, jobKind)
	}

	final, err := finishNodeJobExec(ctx, tx, nodeID, jobID, in)
	if err != nil {
		return out, err
	}
	out.FinalStatus = final
	if final == "pending" {
		// Retry already scheduled; no terminal follow-ups.
		return out, tx.Commit()
	}

	now := time.Now().UTC().Format(time.RFC3339)
	applied := strings.TrimSpace(appliedVersion)

	var currentDesiredNS sql.NullString
	var baselineSchema int
	if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(desired_users_version, ''), users_sync_baseline_schema FROM nodes WHERE id = ?;
`, nodeID).Scan(&currentDesiredNS, &baselineSchema); err != nil {
		return out, err
	}
	currentDesired := strings.TrimSpace(currentDesiredNS.String)

	payload, payloadClass := classifyUsersSyncPayload(jobPayload)
	var agentResult usersSyncAgentResult
	resultParsed := false
	if raw := strings.TrimSpace(in.ResultJSON); raw != "" {
		if json.Unmarshal([]byte(raw), &agentResult) == nil {
			resultParsed = true
		}
	}

	if final == "succeeded" {
		capturedDesired := strings.TrimSpace(jobDesired.String)
		// An empty job desired_version (legacy) is never an acceptance
		// wildcard: it may only be accepted against the node's current
		// desired version. An empty applied_version is never accepted.
		accepted := applied != "" &&
			((capturedDesired != "" && applied == capturedDesired) || applied == currentDesired)
		if !accepted {
			if _, err := s.prepareUsersSyncTx(ctx, tx, nodeID, true, "finish_version_mismatch"); err != nil {
				return out, err
			}
			out.RepairEnqueued = true
			return out, tx.Commit()
		}
		out.Accepted = true
		if _, err := tx.ExecContext(ctx, `
UPDATE nodes SET applied_users_version = ?, updated_at = ? WHERE id = ?;
`, applied, now, nodeID); err != nil {
			return out, err
		}

		// Delta-ready marker. Only an accepted schema-1 result whose reported
		// target matches the accepted applied version proves the agent
		// persisted a versioned baseline.
		schema1Result := resultParsed && strings.TrimSpace(agentResult.Mode) != "" &&
			strings.TrimSpace(agentResult.TargetVersion) == applied
		switch {
		case payloadClass == usersSyncPayloadFull && schema1Result && agentResult.Mode == usersync.ModeFull:
			if _, err := tx.ExecContext(ctx, `
UPDATE nodes SET users_sync_baseline_schema = 1, users_sync_delta_ready_at = ? WHERE id = ?;
`, now, nodeID); err != nil {
				return out, err
			}
			out.DeltaReadyMarked = true
		case (payloadClass == usersSyncPayloadFull || payloadClass == usersSyncPayloadDelta) && !schema1Result:
			if baselineSchema >= 1 {
				// Agent capability drift: a previously delta-ready node
				// answered a schema-1 job with a legacy/malformed result.
				// Stop generating deltas and re-establish the baseline.
				log.Printf("users_sync node=%d: delta-ready agent returned legacy/malformed result for schema-1 job %d; clearing delta-ready marker", nodeID, jobID)
				if _, err := tx.ExecContext(ctx, `
UPDATE nodes SET users_sync_baseline_schema = 0, users_sync_delta_ready_at = NULL WHERE id = ?;
`, nodeID); err != nil {
					return out, err
				}
				out.DeltaReadyCleared = true
				if _, err := s.prepareUsersSyncTx(ctx, tx, nodeID, true, "baseline_backfill"); err != nil {
					return out, err
				}
				out.RepairEnqueued = true
			} else {
				// Legacy agent answered a schema-1 full backfill: accept the
				// applied version, leave the marker unset, and do not loop
				// another forced full at this same legacy agent.
				log.Printf("users_sync node=%d: legacy agent result for schema-1 job %d; node stays full-sync only", nodeID, jobID)
			}
		}

		// Cancel a stale pending delta (base no longer applied) or a
		// redundant same-version pending job — but never a protected repair.
		pending, hasPending, err := getPendingUsersSyncJobExec(ctx, tx, nodeID)
		if err != nil {
			return out, err
		}
		if hasPending {
			pPayload, pClass := classifyUsersSyncPayload(pending.PayloadJSON)
			protected := pClass == usersSyncPayloadFull && protectedUsersSyncReasons[strings.TrimSpace(pPayload.Reason)]
			switch {
			case pClass == usersSyncPayloadDelta && strings.TrimSpace(pPayload.BaseVersion) != applied:
				if err := cancelUsersSyncJobExec(ctx, tx, pending.ID, now, "stale delta base after accepted sync"); err != nil {
					return out, err
				}
				out.StaleDeltaCanceled = true
			case pending.DesiredVersion == applied && !protected:
				if err := cancelUsersSyncJobExec(ctx, tx, pending.ID, now, "redundant after accepted sync"); err != nil {
					return out, err
				}
				out.RedundantCanceled = true
			}
		}

		if applied != currentDesired {
			// Stale-but-valid success: converge to the current desired state
			// immediately instead of waiting for the next reconcile tick.
			if _, err := s.prepareUsersSyncTx(ctx, tx, nodeID, false, "reconcile"); err != nil {
				return out, err
			}
			out.FollowUpEnqueued = true
		}
		return out, tx.Commit()
	}

	// Terminal failure.
	if payloadClass == usersSyncPayloadFull && strings.TrimSpace(payload.Reason) == "baseline_backfill" {
		// A failed backfill must not strand the node: clear the marker so the
		// next startup backfill pass retries.
		if _, err := tx.ExecContext(ctx, `
UPDATE nodes SET users_sync_baseline_backfill_at = NULL WHERE id = ?;
`, nodeID); err != nil {
			return out, err
		}
	}
	if resultParsed && agentResult.NeedFullSync {
		reason := strings.TrimSpace(agentResult.Reason)
		if !repairCompatibleUsersSyncReasons[reason] {
			reason = "repair"
		}
		if _, err := s.prepareUsersSyncTx(ctx, tx, nodeID, true, reason); err != nil {
			return out, err
		}
		out.RepairEnqueued = true
	}
	return out, tx.Commit()
}

// --- rollout / backfill ---

// ListNodesNeedingUsersSyncBackfill returns enabled nodes that have never
// proven a schema-1 versioned baseline and have not been sent a backfill yet.
func (s *Store) ListNodesNeedingUsersSyncBackfill(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id FROM nodes
WHERE enabled = 1
  AND users_sync_baseline_schema < 1
  AND users_sync_baseline_backfill_at IS NULL
ORDER BY id ASC;
`)
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
	return ids, rows.Err()
}

func (s *Store) SetNodeUsersSyncBackfillAt(ctx context.Context, nodeID int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE nodes SET users_sync_baseline_backfill_at = ? WHERE id = ?;
`, at.UTC().Format(time.RFC3339), nodeID)
	return err
}

// NodeUsersSyncRollout exposes the per-node delta rollout markers for tests
// and operators.
type NodeUsersSyncRollout struct {
	BaselineSchema int
	DeltaReadyAt   *time.Time
	BackfillAt     *time.Time
}

func (s *Store) GetNodeUsersSyncRollout(ctx context.Context, nodeID int64) (NodeUsersSyncRollout, error) {
	var out NodeUsersSyncRollout
	var deltaReady, backfill sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT users_sync_baseline_schema, users_sync_delta_ready_at, users_sync_baseline_backfill_at
FROM nodes WHERE id = ?;
`, nodeID).Scan(&out.BaselineSchema, &deltaReady, &backfill)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, ErrNodeNotFound
		}
		return out, err
	}
	if deltaReady.Valid && deltaReady.String != "" {
		if t, perr := time.Parse(time.RFC3339, deltaReady.String); perr == nil {
			out.DeltaReadyAt = &t
		}
	}
	if backfill.Valid && backfill.String != "" {
		if t, perr := time.Parse(time.RFC3339, backfill.String); perr == nil {
			out.BackfillAt = &t
		}
	}
	return out, nil
}

// --- versioned agent snapshot (full repair/read path) ---

// MaterializeUsersSnapshotForAgent materializes the current canonical users
// for a node, persists the snapshot, and updates desired_users_version in one
// transaction — without enqueueing any job. It backs GET /agent/users.
func (s *Store) MaterializeUsersSnapshotForAgent(ctx context.Context, nodeID int64) (string, []usersync.Item, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var enabled int
	if err := tx.QueryRowContext(ctx, `SELECT enabled FROM nodes WHERE id = ?;`, nodeID).Scan(&enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, ErrNodeNotFound
		}
		return "", nil, err
	}
	items := make([]usersync.Item, 0)
	if enabled == 1 {
		items, err = listNodeUserSyncItemsExec(ctx, tx, nodeID)
		if err != nil {
			return "", nil, err
		}
	}
	if err := usersync.ValidateItems(items); err != nil {
		return "", nil, fmt.Errorf("canonical users for node %d invalid: %w", nodeID, err)
	}
	version := usersync.HashItems(items)
	now := time.Now().UTC().Format(time.RFC3339)
	if err := upsertNodeUserSyncSnapshotExec(ctx, tx, nodeID, version, items, now); err != nil {
		return "", nil, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE nodes SET desired_users_version = ?, updated_at = ? WHERE id = ?;
`, version, now, nodeID); err != nil {
		return "", nil, err
	}
	if err := tx.Commit(); err != nil {
		return "", nil, err
	}
	if err := s.PruneNodeUserSyncSnapshots(ctx, nodeID); err != nil {
		log.Printf("prune user sync snapshots node=%d: %v", nodeID, err)
	}
	return version, items, nil
}
