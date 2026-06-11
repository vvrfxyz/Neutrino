package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type NodeJob struct {
	ID             int64      `json:"id"`
	NodeID         int64      `json:"node_id"`
	Kind           string     `json:"kind"`
	DesiredVersion string     `json:"desired_version"`
	PayloadJSON    string     `json:"payload_json"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	Retryable      bool       `json:"retryable"`
	HTTPStatus     *int       `json:"http_status,omitempty"`
	TimeoutSec     *int       `json:"timeout_sec,omitempty"`
	CorrelationID  string     `json:"correlation_id,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	ResultJSON     string     `json:"result_json,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
}

type NodeJobSummary struct {
	NodeID             int64
	Pending            int
	RunningKind        string
	RunningDesired     string
	RunningStartedAt   string
	RunningCorrelation string
}

// EnqueueNodeJob upserts a pending job for a node+kind. Probe jobs dedupe by
// node+kind+correlation so different targets do not overwrite each other.
// If there is a running job of the same kind with the same desired_version, it returns it without enqueueing.
func (s *Store) EnqueueNodeJob(ctx context.Context, nodeID int64, kind string, desiredVersion string, payloadJSON string, timeoutSec int, correlationID string) (jobID int64, enqueued bool, err error) {
	// The dedupe is check-then-insert, so it must run inside a write
	// transaction (_txlock=immediate) or two concurrent enqueues can both miss
	// the pending SELECT and double-insert the same node+kind job.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }()
	jobID, enqueued, err = enqueueNodeJobExec(ctx, tx, nodeID, kind, desiredVersion, payloadJSON, timeoutSec, correlationID)
	if err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return jobID, enqueued, nil
}

func enqueueNodeJobExec(ctx context.Context, q execQueryContext, nodeID int64, kind, desiredVersion, payloadJSON string, timeoutSec int, correlationID string) (jobID int64, enqueued bool, err error) {
	kind = strings.TrimSpace(kind)
	payloadJSON = strings.TrimSpace(payloadJSON)
	desiredVersion = strings.TrimSpace(desiredVersion)
	correlationID = strings.TrimSpace(correlationID)
	if nodeID <= 0 || kind == "" {
		return 0, false, fmt.Errorf("invalid node job")
	}
	if payloadJSON == "" {
		payloadJSON = "{}"
	}
	if timeoutSec < 0 {
		timeoutSec = 0
	}
	dedupeByCorrelation := nodeJobDedupeByCorrelation(kind)
	dedupeCorrelationID := strings.TrimSpace(correlationID)

	// If a running job of the same kind is already running with the same desired_version, do nothing.
	var runningID int64
	var runningDesired sql.NullString
	var runningCorrelation sql.NullString
	runningArgs := []any{nodeID, kind}
	runningQuery := `
SELECT id, desired_version, correlation_id
FROM node_jobs
WHERE node_id = ? AND kind = ? AND status = 'running'
`
	if dedupeByCorrelation {
		runningQuery += ` AND COALESCE(correlation_id, '') = ?`
		runningArgs = append(runningArgs, dedupeCorrelationID)
	}
	runningQuery += `
ORDER BY id DESC
LIMIT 1;
`
	err = q.QueryRowContext(ctx, runningQuery, runningArgs...).Scan(&runningID, &runningDesired, &runningCorrelation)
	if err == nil && runningID > 0 {
		if dedupeByCorrelation && strings.TrimSpace(runningCorrelation.String) == dedupeCorrelationID {
			return runningID, false, nil
		}
		if strings.TrimSpace(runningDesired.String) == desiredVersion && desiredVersion != "" {
			return runningID, false, nil
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// If a pending job exists, overwrite it with the latest desired_version and payload.
	var pendingID int64
	pendingArgs := []any{nodeID, kind}
	pendingQuery := `
SELECT id
FROM node_jobs
WHERE node_id = ? AND kind = ? AND status = 'pending'
`
	if dedupeByCorrelation {
		pendingQuery += ` AND COALESCE(correlation_id, '') = ?`
		pendingArgs = append(pendingArgs, dedupeCorrelationID)
	}
	pendingQuery += `
ORDER BY id DESC
LIMIT 1;
`
	err = q.QueryRowContext(ctx, pendingQuery, pendingArgs...).Scan(&pendingID)
	if err == nil && pendingID > 0 {
		_, err := q.ExecContext(ctx, `
UPDATE node_jobs
SET desired_version = ?,
	payload_json = ?,
	timeout_sec = NULLIF(?, 0),
	correlation_id = NULLIF(?, ''),
	attempts = 0,
	retryable = 1,
	http_status = NULL,
	result_json = NULL,
	last_error = NULL,
	started_at = NULL,
	finished_at = NULL,
	created_at = ?
WHERE id = ?;
`, desiredVersion, payloadJSON, timeoutSec, correlationID, now, pendingID)
		if err != nil {
			return 0, false, err
		}
		return pendingID, false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}

	if err := q.QueryRowContext(ctx, `
INSERT INTO node_jobs(node_id, kind, desired_version, payload_json, status, attempts, retryable, created_at, timeout_sec, correlation_id)
VALUES (?, ?, ?, ?, 'pending', 0, 1, ?, NULLIF(?, 0), NULLIF(?, ''))
RETURNING id;
`, nodeID, kind, desiredVersion, payloadJSON, now, timeoutSec, correlationID).Scan(&jobID); err != nil {
		return 0, false, err
	}
	return jobID, true, nil
}

func nodeJobDedupeByCorrelation(kind string) bool {
	return strings.HasPrefix(strings.TrimSpace(kind), "probe_")
}

func (s *Store) HasPendingOrRunningNodeJob(ctx context.Context, nodeID int64, kind string) (bool, error) {
	if nodeID <= 0 {
		return false, fmt.Errorf("invalid node id")
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return false, fmt.Errorf("invalid kind")
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `
SELECT id
FROM node_jobs
WHERE node_id = ? AND kind = ? AND status IN ('pending','running')
ORDER BY id DESC
LIMIT 1;
`, nodeID, kind).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return id > 0, nil
}

func (s *Store) ClaimNextNodeJobForNode(ctx context.Context, nodeID int64) (NodeJob, bool, error) {
	return s.ClaimNextNodeJobForNodeKinds(ctx, nodeID, nil)
}

func (s *Store) ClaimNextNodeJobForNodeKinds(ctx context.Context, nodeID int64, allowedKinds []string) (NodeJob, bool, error) {
	if nodeID <= 0 {
		return NodeJob{}, false, fmt.Errorf("invalid node id")
	}
	filteredKinds := make([]string, 0, len(allowedKinds))
	for _, kind := range allowedKinds {
		kind = strings.TrimSpace(kind)
		if kind != "" {
			filteredKinds = append(filteredKinds, kind)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NodeJob{}, false, err
	}
	defer tx.Rollback()

	// Per-node serial: do not claim if there is a running job for this node.
	var runningID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM node_jobs WHERE node_id = ? AND status = 'running' ORDER BY id DESC LIMIT 1;`, nodeID).Scan(&runningID)
	if err == nil && runningID > 0 {
		return NodeJob{}, false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return NodeJob{}, false, err
	}

	var j NodeJob
	var createdAt string
	var startedAt sql.NullString
	var finishedAt sql.NullString
	var lastErr sql.NullString
	var resultJSON sql.NullString
	var desiredVersion sql.NullString
	var retryableInt int
	var httpStatus sql.NullInt64
	var timeoutSec sql.NullInt64
	var correlationID sql.NullString

	queryArgs := []any{nodeID}
	query := `
	SELECT id, node_id, kind, desired_version, payload_json, status, attempts, retryable, http_status, timeout_sec, correlation_id, last_error, result_json, created_at, started_at, finished_at
	FROM node_jobs
	WHERE node_id = ? AND status = 'pending'
`
	if len(filteredKinds) > 0 {
		placeholders := make([]string, 0, len(filteredKinds))
		for _, kind := range filteredKinds {
			placeholders = append(placeholders, "?")
			queryArgs = append(queryArgs, kind)
		}
		query += " AND kind IN (" + strings.Join(placeholders, ", ") + ")"
	}
	query += `
	ORDER BY CASE
		WHEN kind = 'users_sync' THEN 0
		WHEN kind IN ('xray_apply','xray_rollback') THEN 1
		ELSE 2
	END, created_at ASC, id ASC
	LIMIT 1;
	`
	err = tx.QueryRowContext(ctx, query, queryArgs...).Scan(&j.ID, &j.NodeID, &j.Kind, &desiredVersion, &j.PayloadJSON, &j.Status, &j.Attempts, &retryableInt, &httpStatus, &timeoutSec, &correlationID, &lastErr, &resultJSON, &createdAt, &startedAt, &finishedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NodeJob{}, false, nil
		}
		return NodeJob{}, false, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `
UPDATE node_jobs
SET status = 'running', attempts = attempts + 1, started_at = ?, finished_at = NULL, http_status = NULL, result_json = NULL, last_error = NULL
WHERE id = ? AND status = 'pending';
`, now, j.ID)
	if err != nil {
		return NodeJob{}, false, err
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return NodeJob{}, false, err
	}
	if aff == 0 {
		// Another claimer won the race; commit (to release the transaction cleanly) and report no claim.
		if cerr := tx.Commit(); cerr != nil {
			return NodeJob{}, false, cerr
		}
		return NodeJob{}, false, nil
	}

	j.Attempts++
	j.Status = "running"
	j.HTTPStatus = nil
	j.ResultJSON = ""
	j.LastError = ""
	j.FinishedAt = nil
	if desiredVersion.Valid {
		j.DesiredVersion = desiredVersion.String
	}
	j.Retryable = retryableInt != 0
	if timeoutSec.Valid {
		v := int(timeoutSec.Int64)
		j.TimeoutSec = &v
	}
	if correlationID.Valid {
		j.CorrelationID = correlationID.String
	}
	j.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	if startedAt.Valid && startedAt.String != "" {
		if t, parseErr := time.Parse(time.RFC3339, startedAt.String); parseErr == nil {
			j.StartedAt = &t
		}
	}
	if finishedAt.Valid && finishedAt.String != "" {
		if t, parseErr := time.Parse(time.RFC3339, finishedAt.String); parseErr == nil {
			j.FinishedAt = &t
		}
	}
	if lastErr.Valid {
		j.LastError = lastErr.String
	}
	if resultJSON.Valid {
		j.ResultJSON = resultJSON.String
	}

	if err := tx.Commit(); err != nil {
		return NodeJob{}, false, err
	}
	return j, true, nil
}

func (s *Store) GetNodeJob(ctx context.Context, jobID int64) (NodeJob, bool, error) {
	if jobID <= 0 {
		return NodeJob{}, false, fmt.Errorf("invalid job id")
	}
	var j NodeJob
	var createdAt string
	var startedAt sql.NullString
	var finishedAt sql.NullString
	var lastErr sql.NullString
	var resultJSON sql.NullString
	var desiredVersion sql.NullString
	var retryableInt int
	var httpStatus sql.NullInt64
	var timeoutSec sql.NullInt64
	var correlationID sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT id, node_id, kind, desired_version, payload_json, status, attempts, retryable, http_status, timeout_sec, correlation_id, last_error, result_json, created_at, started_at, finished_at
FROM node_jobs
WHERE id = ?;
`, jobID).Scan(&j.ID, &j.NodeID, &j.Kind, &desiredVersion, &j.PayloadJSON, &j.Status, &j.Attempts, &retryableInt, &httpStatus, &timeoutSec, &correlationID, &lastErr, &resultJSON, &createdAt, &startedAt, &finishedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NodeJob{}, false, nil
		}
		return NodeJob{}, false, err
	}
	if desiredVersion.Valid {
		j.DesiredVersion = desiredVersion.String
	}
	j.Retryable = retryableInt != 0
	if httpStatus.Valid {
		v := int(httpStatus.Int64)
		j.HTTPStatus = &v
	}
	if timeoutSec.Valid {
		v := int(timeoutSec.Int64)
		j.TimeoutSec = &v
	}
	if correlationID.Valid {
		j.CorrelationID = correlationID.String
	}
	j.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	if startedAt.Valid && startedAt.String != "" {
		if t, parseErr := time.Parse(time.RFC3339, startedAt.String); parseErr == nil {
			j.StartedAt = &t
		}
	}
	if finishedAt.Valid && finishedAt.String != "" {
		if t, parseErr := time.Parse(time.RFC3339, finishedAt.String); parseErr == nil {
			j.FinishedAt = &t
		}
	}
	if lastErr.Valid {
		j.LastError = lastErr.String
	}
	if resultJSON.Valid {
		j.ResultJSON = resultJSON.String
	}
	return j, true, nil
}

type FinishNodeJobInput struct {
	Status      string
	Retryable   bool
	HTTPStatus  *int
	ResultJSON  string
	ErrorMsg    string
	MaxAttempts int
	Attempt     int
}

func (s *Store) FinishNodeJobForNode(ctx context.Context, nodeID int64, jobID int64, in FinishNodeJobInput) (finalStatus string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	finalStatus, err = finishNodeJobExec(ctx, tx, nodeID, jobID, in)
	if err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return finalStatus, nil
}

// finishNodeJobExec runs the terminal/retry transition on the caller's
// transaction so kind-specific finish paths (users_sync) can compose
// follow-up writes atomically with the transition.
func finishNodeJobExec(ctx context.Context, tx usersSyncQuerier, nodeID int64, jobID int64, in FinishNodeJobInput) (finalStatus string, err error) {
	if nodeID <= 0 || jobID <= 0 {
		return "", fmt.Errorf("invalid node/job id")
	}
	in.Status = strings.TrimSpace(in.Status)
	if in.Status == "" {
		in.Status = "failed"
	}
	// Agents may only report a terminal outcome. Anything else (e.g. "pending"
	// or "running") would corrupt the job state machine: a non-terminal status
	// written here bypasses the claim path and its attempt accounting.
	if in.Status != "succeeded" && in.Status != "failed" {
		return "", ErrNodeJobInvalidStatus
	}
	if in.MaxAttempts <= 0 {
		in.MaxAttempts = 5
	}
	now := time.Now().UTC().Format(time.RFC3339)

	// Determine current attempts to decide requeue. Consistent inside the tx.
	// A finish request is only valid for the running attempt the agent claimed.
	var attempts int
	err = tx.QueryRowContext(ctx, `
SELECT attempts
FROM node_jobs
WHERE id = ? AND node_id = ? AND status = 'running' AND attempts = ?;
`, jobID, nodeID, in.Attempt).Scan(&attempts)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNodeJobNotRunning
		}
		return "", err
	}

	finalStatus = in.Status
	retryableInt := 0
	if in.Retryable {
		retryableInt = 1
	}
	httpStatus := sql.NullInt64{}
	if in.HTTPStatus != nil && *in.HTTPStatus > 0 {
		httpStatus = sql.NullInt64{Int64: int64(*in.HTTPStatus), Valid: true}
	}

	// Requeue if retryable and under max attempts.
	if in.Status == "failed" && in.Retryable && attempts < in.MaxAttempts {
		finalStatus = "pending"
	}

	resultJSON := strings.TrimSpace(in.ResultJSON)
	errorMsg := strings.TrimSpace(in.ErrorMsg)
	finishedAt := any(now)
	if finalStatus == "pending" {
		httpStatus = sql.NullInt64{}
		resultJSON = ""
		errorMsg = ""
		finishedAt = nil
	}

	res, err := tx.ExecContext(ctx, `
	UPDATE node_jobs
	SET status = ?, retryable = ?, http_status = ?, result_json = NULLIF(?, ''), last_error = NULLIF(?, ''), finished_at = ?
	WHERE id = ? AND node_id = ? AND status = 'running' AND attempts = ?;
	`, finalStatus, retryableInt, httpStatus, resultJSON, errorMsg, finishedAt, jobID, nodeID, in.Attempt)
	if err != nil {
		return "", err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if affected == 0 {
		return "", ErrNodeJobNotRunning
	}
	return finalStatus, nil
}

func (s *Store) ListNodeJobs(ctx context.Context, nodeID int64, limit int) ([]NodeJob, error) {
	if nodeID <= 0 {
		return nil, fmt.Errorf("invalid node id")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, node_id, kind, desired_version, payload_json, status, attempts, retryable, http_status, timeout_sec, correlation_id, last_error, result_json, created_at, started_at, finished_at
FROM node_jobs
WHERE node_id = ?
ORDER BY id DESC
LIMIT ?;
`, nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NodeJob, 0, limit)
	for rows.Next() {
		var j NodeJob
		var createdAt string
		var startedAt sql.NullString
		var finishedAt sql.NullString
		var lastErr sql.NullString
		var resultJSON sql.NullString
		var desiredVersion sql.NullString
		var retryableInt int
		var httpStatus sql.NullInt64
		var timeoutSec sql.NullInt64
		var correlationID sql.NullString
		if err := rows.Scan(&j.ID, &j.NodeID, &j.Kind, &desiredVersion, &j.PayloadJSON, &j.Status, &j.Attempts, &retryableInt, &httpStatus, &timeoutSec, &correlationID, &lastErr, &resultJSON, &createdAt, &startedAt, &finishedAt); err != nil {
			return nil, err
		}
		if desiredVersion.Valid {
			j.DesiredVersion = desiredVersion.String
		}
		j.Retryable = retryableInt != 0
		if httpStatus.Valid {
			v := int(httpStatus.Int64)
			j.HTTPStatus = &v
		}
		if timeoutSec.Valid {
			v := int(timeoutSec.Int64)
			j.TimeoutSec = &v
		}
		if correlationID.Valid {
			j.CorrelationID = correlationID.String
		}
		j.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		if startedAt.Valid && startedAt.String != "" {
			if t, parseErr := time.Parse(time.RFC3339, startedAt.String); parseErr == nil {
				j.StartedAt = &t
			}
		}
		if finishedAt.Valid && finishedAt.String != "" {
			if t, parseErr := time.Parse(time.RFC3339, finishedAt.String); parseErr == nil {
				j.FinishedAt = &t
			}
		}
		if lastErr.Valid {
			j.LastError = lastErr.String
		}
		if resultJSON.Valid {
			j.ResultJSON = resultJSON.String
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) GetNodeJobSummary(ctx context.Context, nodeID int64) (pending int, runningKind string, runningDesired string, runningStartedAt string, runningCorrelation string, err error) {
	if nodeID <= 0 {
		return 0, "", "", "", "", fmt.Errorf("invalid node id")
	}
	_ = s.db.QueryRowContext(ctx, `
SELECT COUNT(1)
FROM node_jobs
WHERE node_id = ? AND status = 'pending';
`, nodeID).Scan(&pending)

	var startedAt sql.NullString
	var corr sql.NullString
	err2 := s.db.QueryRowContext(ctx, `
SELECT kind, desired_version, started_at, correlation_id
FROM node_jobs
WHERE node_id = ? AND status = 'running'
ORDER BY id DESC
LIMIT 1;
`, nodeID).Scan(&runningKind, &runningDesired, &startedAt, &corr)
	if err2 == nil {
		if startedAt.Valid {
			runningStartedAt = startedAt.String
		}
		if corr.Valid {
			runningCorrelation = corr.String
		}
		return pending, runningKind, runningDesired, runningStartedAt, runningCorrelation, nil
	}
	if errors.Is(err2, sql.ErrNoRows) {
		return pending, "", "", "", "", nil
	}
	return pending, "", "", "", "", err2
}

func (s *Store) ListNodeJobSummaries(ctx context.Context) (map[int64]NodeJobSummary, error) {
	out := make(map[int64]NodeJobSummary)

	rows, err := s.db.QueryContext(ctx, `
SELECT node_id, COUNT(1)
FROM node_jobs
WHERE status = 'pending'
GROUP BY node_id;
`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var nodeID int64
		var pending int
		if err := rows.Scan(&nodeID, &pending); err != nil {
			rows.Close()
			return nil, err
		}
		out[nodeID] = NodeJobSummary{NodeID: nodeID, Pending: pending}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, `
SELECT j.node_id, j.kind, j.desired_version, j.started_at, j.correlation_id
FROM node_jobs AS j
JOIN (
	SELECT node_id, MAX(id) AS id
	FROM node_jobs
	WHERE status = 'running'
	GROUP BY node_id
) AS latest ON latest.id = j.id
ORDER BY j.node_id ASC;
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID int64
		var kind string
		var desired sql.NullString
		var startedAt sql.NullString
		var corr sql.NullString
		if err := rows.Scan(&nodeID, &kind, &desired, &startedAt, &corr); err != nil {
			return nil, err
		}
		summary := out[nodeID]
		summary.NodeID = nodeID
		summary.RunningKind = kind
		if desired.Valid {
			summary.RunningDesired = desired.String
		}
		if startedAt.Valid {
			summary.RunningStartedAt = startedAt.String
		}
		if corr.Valid {
			summary.RunningCorrelation = corr.String
		}
		out[nodeID] = summary
	}
	return out, rows.Err()
}

// nodeJobSweepGrace is added on top of a job's own timeout_sec when computing
// the sweep deadline. timeout_sec is the agent-side execution budget (e.g. a
// probe's 3s execution timeout), so the sweeper must give the agent headroom
// to run the job and report the result before reclaiming it.
const nodeJobSweepGrace = 60 * time.Second

// nodeJobSweepFallbackTimeout bounds jobs that have neither a per-job
// timeout_sec nor a kind default, so every claimed job eventually times out
// and can never permanently wedge a node's serial job pipeline.
const nodeJobSweepFallbackTimeout = 10 * time.Minute

// nodeJobSweepTimeout computes the effective sweep timeout for a running job:
//   - timeout_sec > 0: max(timeout_sec + grace, kind default);
//   - else: the kind default from timeoutByKind, if set;
//   - else: the global fallback.
func nodeJobSweepTimeout(timeoutSec int, kindDefault time.Duration) time.Duration {
	if timeoutSec > 0 {
		eff := time.Duration(timeoutSec)*time.Second + nodeJobSweepGrace
		if kindDefault > eff {
			eff = kindDefault
		}
		return eff
	}
	if kindDefault > 0 {
		return kindDefault
	}
	return nodeJobSweepFallbackTimeout
}

// nodeJobSweepCandidate is one running job the sweeper considers timed out.
// attempts and startedAt are the values observed during the candidate scan;
// they fence the subsequent UPDATE so that an attempt the agent finished
// and/or re-claimed between scan and update is never clobbered.
type nodeJobSweepCandidate struct {
	jobID     int64
	nodeID    int64
	kind      string
	attempts  int
	startedAt string
}

// SweepTimedOutRunningJobs marks timed-out running jobs as failed (or requeues
// them while under maxAttempts) and stamps the owning node's error state.
// The effective timeout per job is computed by nodeJobSweepTimeout, so every
// claimed job times out eventually even if its kind has no default.
func (s *Store) SweepTimedOutRunningJobs(ctx context.Context, timeoutByKind map[string]time.Duration, maxAttempts int) (int64, error) {
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	now := time.Now().UTC()

	rows, err := s.db.QueryContext(ctx, `
SELECT id, node_id, kind, attempts, started_at, timeout_sec
FROM node_jobs
WHERE status = 'running' AND started_at IS NOT NULL;
`)
	if err != nil {
		return 0, err
	}
	candidates := make([]nodeJobSweepCandidate, 0, 8)
	for rows.Next() {
		var jobID, nodeID int64
		var kind string
		var attempts int
		var startedAtStr string
		var timeoutSec sql.NullInt64
		if err := rows.Scan(&jobID, &nodeID, &kind, &attempts, &startedAtStr, &timeoutSec); err != nil {
			rows.Close()
			return 0, err
		}
		startedAt, err := time.Parse(time.RFC3339, startedAtStr)
		if err != nil {
			continue
		}
		jobTimeoutSec := 0
		if timeoutSec.Valid {
			jobTimeoutSec = int(timeoutSec.Int64)
		}
		to := nodeJobSweepTimeout(jobTimeoutSec, timeoutByKind[strings.TrimSpace(kind)])
		if now.Sub(startedAt) < to {
			continue
		}
		candidates = append(candidates, nodeJobSweepCandidate{jobID: jobID, nodeID: nodeID, kind: kind, attempts: attempts, startedAt: startedAtStr})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	affected := int64(0)
	nowStr := now.Format(time.RFC3339)
	for _, c := range candidates {
		swept, err := s.sweepTimedOutJobCandidate(ctx, c, maxAttempts, nowStr)
		if err != nil {
			return affected, err
		}
		if swept {
			affected++
		}
	}
	return affected, nil
}

// sweepTimedOutJobCandidate times out a single scanned candidate. The job
// UPDATE is fenced on the scanned attempts and started_at (mirroring
// FinishNodeJobForNode); if the agent finished or re-claimed the job since
// the scan, the UPDATE affects 0 rows and neither the job nor the node's
// error fields are touched.
func (s *Store) sweepTimedOutJobCandidate(ctx context.Context, c nodeJobSweepCandidate, maxAttempts int, nowStr string) (bool, error) {
	final := "failed"
	if c.attempts < maxAttempts {
		final = "pending"
	}
	finishedAt := any(nowStr)
	lastError := "timeout"
	if final == "pending" {
		finishedAt = nil
		lastError = ""
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
UPDATE node_jobs
SET status = ?, retryable = 1, http_status = NULL, result_json = NULL, last_error = NULLIF(?, ''), finished_at = ?
WHERE id = ? AND status = 'running' AND attempts = ? AND started_at = ?;
`, final, lastError, finishedAt, c.jobID, c.attempts, c.startedAt)
	if err != nil {
		return false, err
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if aff == 0 {
		// The scanned attempt finished or was re-claimed since the candidate
		// scan; leave the fresh state (and the node's error fields) alone.
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE nodes
SET last_error = ?, last_job_error_kind = ?, last_job_error_at = ?, updated_at = ?
WHERE id = ?;
`, nullableString("timeout"), "retryable", nowStr, nowStr, c.nodeID); err != nil {
		return false, err
	}
	// A timed-out managed Xray job may have reloaded the runtime before the
	// agent lost the finish report, wiping hot-added API users. Enqueue the
	// forced full users repair in the same sweep transaction; users_sync claim
	// priority guarantees it runs before any requeued xray attempt.
	if c.kind == "xray_apply" || c.kind == "xray_rollback" {
		if _, err := s.prepareUsersSyncTx(ctx, tx, c.nodeID, true, "xray_runtime_unknown_followup"); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
