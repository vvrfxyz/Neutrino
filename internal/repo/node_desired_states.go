package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type NodeDesiredState struct {
	NodeID         int64
	Kind           string
	DesiredVersion string
	PayloadJSON    string
	UpdatedAt      time.Time
}

func (s *Store) UpsertNodeDesiredState(ctx context.Context, nodeID int64, kind string, desiredVersion string, payloadJSON string) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return fmt.Errorf("invalid kind")
	}
	desiredVersion = strings.TrimSpace(desiredVersion)
	payloadJSON = strings.TrimSpace(payloadJSON)
	if payloadJSON == "" {
		payloadJSON = "{}"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	return upsertNodeDesiredStateExec(ctx, s.db, nodeID, kind, desiredVersion, payloadJSON, now)
}

// execContext is the minimal subset of *sql.DB / *sql.Tx we need in shared helpers.
type execContext interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type execQueryContext interface {
	execContext
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func upsertNodeDesiredStateExec(ctx context.Context, q execContext, nodeID int64, kind, desiredVersion, payloadJSON, now string) error {
	_, err := q.ExecContext(ctx, `
INSERT INTO node_desired_states(node_id, kind, desired_version, payload_json, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(node_id) DO UPDATE SET
	kind = excluded.kind,
	desired_version = excluded.desired_version,
	payload_json = excluded.payload_json,
	updated_at = excluded.updated_at;
`, nodeID, kind, desiredVersion, payloadJSON, now)
	return err
}

func setNodeDesiredXrayVersionExec(ctx context.Context, q execContext, nodeID int64, version, now string) error {
	_, err := q.ExecContext(ctx, `
UPDATE nodes
SET desired_xray_version = ?, updated_at = ?
WHERE id = ?;
`, version, now, nodeID)
	return err
}

func (s *Store) GetNodeDesiredState(ctx context.Context, nodeID int64) (NodeDesiredState, bool, error) {
	if nodeID <= 0 {
		return NodeDesiredState{}, false, fmt.Errorf("invalid node id")
	}
	var st NodeDesiredState
	var updatedAt string
	err := s.db.QueryRowContext(ctx, `
SELECT node_id, kind, desired_version, payload_json, updated_at
FROM node_desired_states
WHERE node_id = ?;
`, nodeID).Scan(&st.NodeID, &st.Kind, &st.DesiredVersion, &st.PayloadJSON, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NodeDesiredState{}, false, nil
		}
		return NodeDesiredState{}, false, err
	}
	st.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return st, true, nil
}

func (s *Store) SetNodeDesiredUsersVersion(ctx context.Context, nodeID int64, version string) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE nodes
SET desired_users_version = ?, updated_at = ?
WHERE id = ?;
`, strings.TrimSpace(version), time.Now().UTC().Format(time.RFC3339), nodeID)
	return err
}

func (s *Store) SetNodeAppliedUsersVersion(ctx context.Context, nodeID int64, version string) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE nodes
SET applied_users_version = ?, last_error = NULL, last_job_error_kind = NULL, last_job_error_at = NULL, updated_at = ?
WHERE id = ?;
`, strings.TrimSpace(version), time.Now().UTC().Format(time.RFC3339), nodeID)
	return err
}

func (s *Store) SetNodeDesiredXrayVersion(ctx context.Context, nodeID int64, version string) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	return setNodeDesiredXrayVersionExec(ctx, s.db, nodeID, strings.TrimSpace(version), time.Now().UTC().Format(time.RFC3339))
}

func (s *Store) SetNodeAppliedXrayVersion(ctx context.Context, nodeID int64, version string) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE nodes
SET applied_xray_version = ?, last_error = NULL, last_job_error_kind = NULL, last_job_error_at = NULL, updated_at = ?
WHERE id = ?;
`, strings.TrimSpace(version), time.Now().UTC().Format(time.RFC3339), nodeID)
	return err
}

func (s *Store) MarkNodeXrayRolledBack(ctx context.Context, nodeID int64, appliedVersion string) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	appliedVersion = strings.TrimSpace(appliedVersion)
	if appliedVersion == "" {
		appliedVersion = "rollback"
	}
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM node_desired_states WHERE node_id = ?;`, nodeID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `
UPDATE nodes
SET desired_xray_version = '',
	applied_xray_version = ?,
	last_error = NULL,
	last_job_error_kind = NULL,
	last_job_error_at = NULL,
	updated_at = ?
WHERE id = ?;
`, appliedVersion, now, nodeID)
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

// DeployManagedXray atomically upserts the managed-xray desired state,
// bumps the node's desired_xray_version, and enqueues an xray_apply job.
// Either all three persist together or none do.
func (s *Store) DeployManagedXray(ctx context.Context, nodeID int64, payloadJSON, desiredVersion string, timeoutSec int, correlationID string) (jobID int64, enqueued bool, err error) {
	if nodeID <= 0 {
		return 0, false, fmt.Errorf("invalid node id")
	}
	payloadJSON = strings.TrimSpace(payloadJSON)
	if payloadJSON == "" {
		payloadJSON = "{}"
	}
	desiredVersion = strings.TrimSpace(desiredVersion)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	if err := upsertNodeDesiredStateExec(ctx, tx, nodeID, "xray_apply", desiredVersion, payloadJSON, now); err != nil {
		return 0, false, err
	}
	if err := setNodeDesiredXrayVersionExec(ctx, tx, nodeID, desiredVersion, now); err != nil {
		return 0, false, err
	}
	jobID, enqueued, err = enqueueNodeJobExec(ctx, tx, nodeID, "xray_apply", desiredVersion, payloadJSON, timeoutSec, correlationID)
	if err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return jobID, enqueued, nil
}

func (s *Store) SetNodeJobError(ctx context.Context, nodeID int64, kind string, errKind string, errMsg string) error {
	if nodeID <= 0 {
		return fmt.Errorf("invalid node id")
	}
	errKind = strings.TrimSpace(errKind)
	if errKind == "" {
		errKind = "retryable"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
UPDATE nodes
SET last_error = ?, last_job_error_kind = ?, last_job_error_at = ?, updated_at = ?
WHERE id = ?;
`, nullableString(strings.TrimSpace(errMsg)), errKind, now, now, nodeID)
	return err
}
