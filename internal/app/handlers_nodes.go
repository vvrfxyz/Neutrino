package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"neutrino/internal/probe"
	"neutrino/internal/repo"
	"neutrino/internal/service"
)

func sha256Hex(s string) string {
	return service.SHA256Hex(s)
}

func defaultString(v string, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// handleAPINodeReport ingests an agent heartbeat/report. Registered as
// POST-only on both listeners; the method is enforced by the route pattern.
func (a *App) handleAPINodeReport(w http.ResponseWriter, r *http.Request, nodeID int64) {
	a.ensureServices()
	// Node-agent control-plane is mTLS only (no bearer tokens).
	if mtlsID, ok := nodeIDFromMTLS(r); !ok || mtlsID != nodeID {
		a.apiError(w, http.StatusForbidden, "forbidden")
		return
	}

	// Best-effort observed IP and reported proxy params (for subscription rendering).
	if ip := a.clientIPFromRequest(r); ip != "" {
		_ = a.nodes().UpdateObservedIP(r.Context(), nodeID, ip)
	}
	var rep repo.NodeReportInput
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			a.apiError(w, http.StatusBadRequest, "invalid json")
			return
		}
	} else {
		// If no JSON, keep backward compatibility (heartbeat with empty body).
		_ = r.Body.Close()
	}
	reportedAt, err := a.nodes().ApplyReport(r.Context(), nodeID, rep)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "apply node report failed")
		return
	}
	if rep.Metrics != nil {
		_ = a.metricHistoryQueue.Enqueue(nodeID, reportedAt, *rep.Metrics)
	}

	receivedAt := time.Now().UTC()
	_ = a.nodes().UpdateAgentStatus(r.Context(), nodeID, &receivedAt, "")
	_ = a.ops().RefreshNode(r.Context(), nodeID)
	a.writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"timestamp":    receivedAt.Format(time.RFC3339),
		"last_seen_at": receivedAt.Format(time.RFC3339),
	})
}

func (a *App) handleAPINodeAgentUsers(w http.ResponseWriter, r *http.Request, nodeID int64) {
	// Node-agent control-plane is mTLS only (no bearer tokens).
	if mtlsID, ok := nodeIDFromMTLS(r); !ok || mtlsID != nodeID {
		a.apiError(w, http.StatusForbidden, "forbidden")
		return
	}
	// Lifecycle refresh must not request users sync from inside this full
	// snapshot path; if it changed global state, the other nodes are notified
	// through the app's coalesced (unthrottled) sync signal afterwards.
	lifecycle, err := a.users().RefreshLifecycleStateNoSync(r.Context())
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "refresh users failed")
		return
	}

	version, items, err := a.nodes().MaterializeUsersForAgent(r.Context(), nodeID)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "list users failed")
		return
	}
	if lifecycle.Changed() {
		a.requestUsersSyncNow(r.Context())
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"schema":  1,
		"version": version,
		"users":   items,
	})
}

func (a *App) handleAPINodeJobsClaim(w http.ResponseWriter, r *http.Request, nodeID int64) {
	// Node-agent control-plane is mTLS only (no bearer tokens).
	if mtlsID, ok := nodeIDFromMTLS(r); !ok || mtlsID != nodeID {
		a.apiError(w, http.StatusForbidden, "forbidden")
		return
	}
	if ip := a.clientIPFromRequest(r); ip != "" {
		_ = a.nodes().UpdateObservedIP(r.Context(), nodeID, ip)
	}
	// Treat claim polling as heartbeat even when no job is returned.
	now := time.Now().UTC()
	_ = a.nodes().UpdateAgentStatus(r.Context(), nodeID, &now, "")
	waitSec := int64(25)
	if v := strings.TrimSpace(r.URL.Query().Get("wait")); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			waitSec = parsed
		}
	}
	if waitSec < 0 {
		waitSec = 0
	}
	if waitSec > 25 {
		waitSec = 25
	}

	deadline := time.Now().Add(time.Duration(waitSec) * time.Second)
	for {
		node, err := a.nodes().Get(r.Context(), nodeID)
		if err != nil {
			a.apiError(w, http.StatusInternalServerError, "claim failed")
			return
		}
		var allowedKinds []string
		if !node.Enabled {
			allowedKinds = []string{"users_sync"}
		}
		job, ok, err := a.nodes().ClaimNextJob(r.Context(), nodeID, allowedKinds)
		if err != nil {
			a.apiError(w, http.StatusInternalServerError, "claim failed")
			return
		}
		if ok {
			now = time.Now().UTC()
			_ = a.nodes().UpdateAgentStatus(r.Context(), nodeID, &now, "")
			_ = a.ops().RefreshNode(r.Context(), nodeID)
			auditAction(a, r, "node.job.claim", "node_job", fmt.Sprintf("%d", job.ID), map[string]any{
				"node_id":         nodeID,
				"kind":            job.Kind,
				"desired_version": job.DesiredVersion,
			})
			a.writeJSON(w, http.StatusOK, map[string]any{"job": job})
			return
		}
		if time.Now().After(deadline) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(400 * time.Millisecond):
		}
	}
}

func summarizeNodeJobFailure(errMsg string, result any) string {
	parts := make([]string, 0, 2)
	if msg := strings.TrimSpace(errMsg); msg != "" {
		parts = append(parts, msg)
	}
	if m, ok := result.(map[string]any); ok {
		if failed, ok := m["failed"]; ok {
			parts = append(parts, fmt.Sprintf("failed=%v", failed))
		}
		if failures, ok := m["failures"].([]any); ok && len(failures) > 0 {
			limit := len(failures)
			if limit > 3 {
				limit = 3
			}
			details := make([]string, 0, limit)
			for _, item := range failures[:limit] {
				if s := strings.TrimSpace(fmt.Sprint(item)); s != "" {
					details = append(details, s)
				}
			}
			if len(details) > 0 {
				parts = append(parts, strings.Join(details, "; "))
			}
		}
	}
	return strings.Join(parts, " | ")
}

func resultBool(result any, key string) (bool, bool) {
	m, ok := result.(map[string]any)
	if !ok || m == nil {
		return false, false
	}
	v, ok := m[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func managedXraySuccessNeedsUsersRepair(kind string, result any) bool {
	switch strings.TrimSpace(kind) {
	case "xray_apply":
		if skipped, ok := resultBool(result, "skipped"); ok && skipped {
			return false
		}
		if reloaded, ok := resultBool(result, "runtime_reloaded"); ok && !reloaded {
			return false
		}
		return true
	case "xray_rollback":
		return true
	default:
		return false
	}
}

func managedXrayFailureUsersRepairReason(kind string, result any) (string, bool) {
	switch strings.TrimSpace(kind) {
	case "xray_apply":
		if rollbackApplied, ok := resultBool(result, "rollback_applied"); ok && rollbackApplied {
			return "xray_apply_rollback_followup", true
		}
		if runtimeUnknown, ok := resultBool(result, "runtime_state_unknown"); ok && runtimeUnknown {
			return "xray_runtime_unknown_followup", true
		}
		if runtimeReloaded, ok := resultBool(result, "runtime_reloaded"); ok && runtimeReloaded {
			return "xray_runtime_unknown_followup", true
		}
	case "xray_rollback":
		if runtimeUnknown, ok := resultBool(result, "runtime_state_unknown"); ok && runtimeUnknown {
			return "xray_runtime_unknown_followup", true
		}
		if runtimeReloaded, ok := resultBool(result, "runtime_reloaded"); ok && runtimeReloaded {
			return "xray_runtime_unknown_followup", true
		}
		if restored, ok := resultBool(result, "rollback_restore_applied"); ok && restored {
			return "xray_runtime_unknown_followup", true
		}
	}
	return "", false
}

// handleAPINodeJobFinishRoute adapts the {job} path wildcard for the agent
// mTLS listener's job-finish route.
func (a *App) handleAPINodeJobFinishRoute(w http.ResponseWriter, r *http.Request, nodeID int64) {
	jobID, err := parseInt64Path(r.PathValue("job"))
	if err != nil {
		a.apiError(w, http.StatusBadRequest, "bad job id")
		return
	}
	a.handleAPINodeJobFinish(w, r, nodeID, jobID)
}

func (a *App) handleAPINodeJobFinish(w http.ResponseWriter, r *http.Request, nodeID int64, jobID int64) {
	// Node-agent control-plane is mTLS only (no bearer tokens).
	if mtlsID, ok := nodeIDFromMTLS(r); !ok || mtlsID != nodeID {
		a.apiError(w, http.StatusForbidden, "forbidden")
		return
	}
	var req struct {
		Status         string `json:"status"`
		Retryable      bool   `json:"retryable"`
		HTTPStatus     *int   `json:"http_status,omitempty"`
		ResultJSON     any    `json:"result_json,omitempty"`
		Error          string `json:"error,omitempty"`
		AppliedVersion string `json:"applied_version,omitempty"`
		Attempt        int    `json:"attempt,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.apiError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Attempt <= 0 {
		a.apiError(w, http.StatusBadRequest, "attempt required")
		return
	}
	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = "failed"
	}
	resultJSON := ""
	if req.ResultJSON != nil {
		if b, err := json.Marshal(req.ResultJSON); err == nil {
			resultJSON = string(b)
		}
	}
	finishInput := repo.FinishNodeJobInput{
		Status:      status,
		Retryable:   req.Retryable,
		HTTPStatus:  req.HTTPStatus,
		ResultJSON:  resultJSON,
		ErrorMsg:    req.Error,
		MaxAttempts: a.cfg.NodeJobMaxAttempts,
		Attempt:     req.Attempt,
	}
	preJob, preOK, preErr := a.nodes().GetJob(r.Context(), jobID)
	if preErr != nil {
		// A transient lookup error must not silently route a users_sync finish
		// through the generic path, skipping applied-version validation and the
		// delta-ready marker. The agent retries the finish.
		a.apiError(w, http.StatusInternalServerError, "finish failed")
		return
	}
	var final string
	var err error
	if preOK && preJob.Kind == "users_sync" {
		// users_sync finishes run the terminal transition and every follow-up
		// (version validation, delta-ready marker, stale-delta cleanup,
		// forced repair) in one repo transaction.
		res, ferr := a.nodes().FinishUsersSyncJob(r.Context(), nodeID, jobID, finishInput, req.AppliedVersion)
		final, err = res.FinalStatus, ferr
	} else {
		final, err = a.nodes().FinishJob(r.Context(), nodeID, jobID, finishInput)
	}
	if err != nil {
		if errors.Is(err, repo.ErrNodeJobNotRunning) {
			a.apiError(w, http.StatusConflict, "job is not running for this attempt")
			return
		}
		if errors.Is(err, repo.ErrNodeJobInvalidStatus) {
			a.apiError(w, http.StatusBadRequest, "status must be succeeded or failed")
			return
		}
		a.apiError(w, http.StatusInternalServerError, "finish failed")
		return
	}

	// Post-finish enrichment (ops cache, probe results, alerts, applied-version
	// markers) is best-effort: the job's terminal transition above is already
	// committed, so failures here must not turn the finish into an error —
	// the reconciler/sweeper converges the derived state on its next pass.
	job, ok, _ := a.nodes().GetJob(r.Context(), jobID)
	if ok {
		_ = a.ops().RefreshNode(r.Context(), nodeID)
		if strings.HasPrefix(job.Kind, "probe_") {
			var result struct {
				Kind       string  `json:"kind"`
				Target     string  `json:"target"`
				Success    bool    `json:"success"`
				LatencyMS  float64 `json:"latency_ms"`
				StatusCode *int    `json:"status_code"`
				Error      string  `json:"error"`
				CheckedAt  string  `json:"checked_at"`
			}
			if strings.TrimSpace(resultJSON) != "" {
				_ = json.Unmarshal([]byte(resultJSON), &result)
			}
			checkedAt := time.Now().UTC()
			if t, err := time.Parse(time.RFC3339, result.CheckedAt); err == nil {
				checkedAt = t
			}
			kind := defaultString(probe.NormalizeKind(result.Kind), probe.NormalizeKind(job.Kind))
			target := strings.TrimSpace(result.Target)
			if target == "" {
				var payload probe.Payload
				if json.Unmarshal([]byte(defaultString(job.PayloadJSON, "{}")), &payload) == nil {
					target = probe.TargetString(kind, payload)
				}
			}
			if target != "" {
				success := result.Success
				if strings.TrimSpace(resultJSON) == "" {
					success = status == "succeeded"
				}
				sourceJobID := jobID
				_, _ = a.nodes().InsertProbeResult(r.Context(), nodeID, repo.InsertNodeProbeResultInput{
					Kind:        kind,
					Target:      target,
					Success:     success,
					LatencyMS:   result.LatencyMS,
					StatusCode:  result.StatusCode,
					Error:       defaultString(result.Error, req.Error),
					CheckedAt:   checkedAt,
					SourceJobID: &sourceJobID,
				})
				_ = a.alerts().SyncProbeAlert(r.Context(), nodeID, kind, target, success, defaultString(result.Error, req.Error), checkedAt)
			}
		}
		if status == "succeeded" && strings.TrimSpace(req.AppliedVersion) != "" {
			switch job.Kind {
			// users_sync applied-version validation and follow-ups already ran
			// inside FinishUsersSyncJob's transaction.
			case "xray_apply":
				_ = a.nodes().SetAppliedXrayVersion(r.Context(), nodeID, req.AppliedVersion)
				if managedXraySuccessNeedsUsersRepair(job.Kind, req.ResultJSON) {
					if err := a.nodes().EnqueueFullUsersSyncForNode(r.Context(), nodeID, "xray_apply_followup"); err != nil {
						log.Printf("enqueue users_sync xray_apply follow-up node=%d: %v", nodeID, err)
					}
				}
			case "xray_rollback":
				_ = a.nodes().MarkXrayRolledBack(r.Context(), nodeID, req.AppliedVersion)
				if managedXraySuccessNeedsUsersRepair(job.Kind, req.ResultJSON) {
					if err := a.nodes().EnqueueFullUsersSyncForNode(r.Context(), nodeID, "xray_rollback_followup"); err != nil {
						log.Printf("enqueue users_sync xray_rollback follow-up node=%d: %v", nodeID, err)
					}
				}
			}
		}
		if status != "succeeded" {
			if reason, ok := managedXrayFailureUsersRepairReason(job.Kind, req.ResultJSON); ok {
				if err := a.nodes().EnqueueFullUsersSyncForNode(r.Context(), nodeID, reason); err != nil {
					log.Printf("enqueue users_sync managed xray repair node=%d kind=%s reason=%s: %v", nodeID, job.Kind, reason, err)
				}
			}
		}
		if status == "succeeded" {
			_ = a.alerts().SyncNodeJobAlert(r.Context(), nodeID, job.Kind, "succeeded", "", time.Now().UTC())
		} else {
			kind := "retryable"
			if !req.Retryable {
				kind = "permanent"
			}
			_ = a.nodes().SetJobError(r.Context(), nodeID, job.Kind, kind, summarizeNodeJobFailure(req.Error, req.ResultJSON))
			_ = a.alerts().SyncNodeJobAlert(r.Context(), nodeID, job.Kind, final, summarizeNodeJobFailure(req.Error, req.ResultJSON), time.Now().UTC())
		}
	}

	auditAction(a, r, "node.job.finish", "node_job", fmt.Sprintf("%d", jobID), map[string]any{
		"node_id":         nodeID,
		"status":          status,
		"final_status":    final,
		"retryable":       req.Retryable,
		"error":           strings.TrimSpace(req.Error),
		"applied_version": strings.TrimSpace(req.AppliedVersion),
	})
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "final_status": final})
}

func (a *App) handleAPINodeMetadata(w http.ResponseWriter, r *http.Request, nodeID int64) {
	switch r.Method {
	case http.MethodGet:
		item, ok, err := a.nodes().GetMetadata(r.Context(), nodeID)
		if err != nil {
			a.apiError(w, http.StatusInternalServerError, "query failed")
			return
		}
		if !ok {
			a.writeJSON(w, http.StatusOK, map[string]any{"node_id": nodeID, "item": nil})
			return
		}
		a.writeJSON(w, http.StatusOK, map[string]any{"node_id": nodeID, "item": item})
	case http.MethodPut, http.MethodPost:
		var in repo.UpsertNodeMetadataInput
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			a.apiError(w, http.StatusBadRequest, "invalid json")
			return
		}
		if err := a.nodes().UpsertMetadata(r.Context(), nodeID, in); err != nil {
			a.apiError(w, http.StatusBadRequest, "metadata update failed")
			return
		}
		// Best-effort ops-cache refresh and re-read for the response.
		_ = a.ops().RefreshNode(r.Context(), nodeID)
		item, _, _ := a.nodes().GetMetadata(r.Context(), nodeID)
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "item": item})
	default:
		a.apiError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *App) handleAPINodeProbes(w http.ResponseWriter, r *http.Request, nodeID int64) {
	var req struct {
		Kind string `json:"kind"`
		probe.Payload
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		a.apiError(w, http.StatusBadRequest, "invalid json")
		return
	}
	kind := probe.NormalizeKind(req.Kind)
	if err := probe.Validate(kind, req.Payload); err != nil {
		a.apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	body, _ := json.Marshal(req.Payload)
	jobID, enqueued, err := a.nodes().EnqueueProbeJob(r.Context(), nodeID, kind, string(body), probeTimeoutSec(req.Payload.TimeoutMS), probe.CorrelationID(kind, req.Payload))
	if err != nil {
		a.apiError(w, http.StatusBadRequest, "enqueue probe failed")
		return
	}
	a.writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "job_id": jobID, "enqueued": enqueued})
}

func probeTimeoutSec(timeoutMS int) int {
	if timeoutMS <= 0 {
		timeoutMS = 3000
	}
	sec := timeoutMS / 1000
	if timeoutMS%1000 != 0 {
		sec++
	}
	if sec < 1 {
		sec = 1
	}
	if sec > 35 {
		sec = 35
	}
	return sec
}

func (a *App) handleAPINodeEnable(w http.ResponseWriter, r *http.Request, nodeID int64) {
	a.setNodeEnabled(w, r, nodeID, true)
}

func (a *App) handleAPINodeDisable(w http.ResponseWriter, r *http.Request, nodeID int64) {
	a.setNodeEnabled(w, r, nodeID, false)
}

func (a *App) setNodeEnabled(w http.ResponseWriter, r *http.Request, nodeID int64, enabled bool) {
	action := "node.disable"
	if enabled {
		action = "node.enable"
	}
	if err := a.nodes().SetEnabled(r.Context(), nodeID, enabled); err != nil {
		// Keep the {"ok":false,"error":...} shape: the nodes SSR page's HTMX
		// buttons predate the {"error":...} envelope.
		a.writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	auditAction(a, r, action, "node", fmt.Sprintf("%d", nodeID), nil)
	_ = a.ops().RefreshNode(r.Context(), nodeID)
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleAPINodeJobsV1(w http.ResponseWriter, r *http.Request, nodeID int64) {
	limit := 50
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if parsed, err := parseInt64Path(v); err == nil {
			limit = int(parsed)
		}
	}
	items, err := a.nodes().ListJobs(r.Context(), nodeID, limit)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"count": len(items), "items": items})
}

func (a *App) handleAPINodeProbeResultsV1(w http.ResponseWriter, r *http.Request, nodeID int64) {
	items, err := a.nodes().ListProbeResults(r.Context(), nodeID, 100)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"node_id": nodeID, "count": len(items), "items": items})
}

func (a *App) handleAPINodeMetricsV1(w http.ResponseWriter, r *http.Request, nodeID int64) {
	rangeName := strings.TrimSpace(r.URL.Query().Get("range"))
	step := strings.TrimSpace(r.URL.Query().Get("step"))
	items, err := a.nodes().ListMetricSeries(r.Context(), nodeID, rangeName, step, time.Now().UTC())
	if err != nil {
		a.apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"node_id": nodeID,
		"range":   defaultString(rangeName, "1h"),
		"step":    defaultString(step, "raw"),
		"count":   len(items),
		"items":   items,
	})
}

func (a *App) handleAPINodeMetricDetailsLatestV1(w http.ResponseWriter, r *http.Request, nodeID int64) {
	item, ok, err := a.nodes().GetLatestMetricDetails(r.Context(), nodeID)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if !ok {
		a.writeJSON(w, http.StatusOK, map[string]any{"node_id": nodeID, "item": nil})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"node_id": nodeID, "item": item})
}

func (a *App) handleAPINodeStaticFactsLatestV1(w http.ResponseWriter, r *http.Request, nodeID int64) {
	item, ok, err := a.nodes().GetLatestStaticFacts(r.Context(), nodeID)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	if !ok {
		a.writeJSON(w, http.StatusOK, map[string]any{"node_id": nodeID, "item": nil})
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"node_id": nodeID, "item": item})
}

func (a *App) handleAPINodeStaticFactsHistoryV1(w http.ResponseWriter, r *http.Request, nodeID int64) {
	items, err := a.nodes().ListStaticFacts(r.Context(), nodeID, 100)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "query failed")
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"node_id": nodeID, "count": len(items), "items": items})
}

// managedXrayErrorStatus maps managed-xray service errors to HTTP status.
func managedXrayErrorStatus(err error) int {
	switch {
	case errors.Is(err, repo.ErrNodeNotFound):
		return http.StatusNotFound
	case errors.Is(err, service.ErrInvalidManagedXrayConfig),
		errors.Is(err, service.ErrNodeNotManaged),
		errors.Is(err, service.ErrNodeCoreTypeNotXray):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func (a *App) handleAPINodeManagedXrayPreview(w http.ResponseWriter, r *http.Request, nodeID int64) {
	var req struct {
		// Optional draft extra_json so the UI can preview unsaved edits.
		ExtraJSON *string `json:"extra_json"`
	}
	body, err := readAllLimit(r.Body, 256*1024)
	if err != nil {
		a.apiError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if strings.TrimSpace(string(body)) != "" {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			a.apiError(w, http.StatusBadRequest, "invalid json")
			return
		}
	}
	preview, err := a.nodes().PreviewManagedXray(r.Context(), nodeID, req.ExtraJSON)
	if err != nil {
		a.apiError(w, managedXrayErrorStatus(err), err.Error())
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "preview": string(preview)})
}

func (a *App) handleAPINodeManagedXrayDeploy(w http.ResponseWriter, r *http.Request, nodeID int64) {
	result, err := a.nodes().DeployManagedXray(r.Context(), nodeID)
	if err != nil {
		a.apiError(w, managedXrayErrorStatus(err), err.Error())
		return
	}
	auditAction(a, r, "node.xray.deploy", "node", fmt.Sprintf("%d", nodeID), map[string]any{"desired_version": result.DesiredVersion, "job_id": result.JobID, "enqueued": result.Enqueued})
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job_id": result.JobID, "enqueued": result.Enqueued, "desired_version": result.DesiredVersion})
}

func (a *App) handleAPINodeManagedXrayRollback(w http.ResponseWriter, r *http.Request, nodeID int64) {
	var req struct {
		BackupName string `json:"backup_name"`
		// Backward-compat: accept backup_path but only use its basename.
		BackupPath string `json:"backup_path"`
	}
	body, err := readAllLimit(r.Body, 64*1024)
	if err != nil {
		a.apiError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if strings.TrimSpace(string(body)) != "" {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			a.apiError(w, http.StatusBadRequest, "invalid json")
			return
		}
	}
	backupName := strings.TrimSpace(req.BackupName)
	if backupName == "" && strings.TrimSpace(req.BackupPath) != "" {
		backupName = strings.TrimSpace(filepath.Base(req.BackupPath))
	}
	result, err := a.nodes().RollbackManagedXray(r.Context(), nodeID, backupName)
	if err != nil {
		a.apiError(w, managedXrayErrorStatus(err), err.Error())
		return
	}
	auditAction(a, r, "node.xray.rollback", "node", fmt.Sprintf("%d", nodeID), map[string]any{"job_id": result.JobID, "enqueued": result.Enqueued})
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job_id": result.JobID, "enqueued": result.Enqueued})
}
