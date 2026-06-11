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

func (a *App) handleAPINodeReport(w http.ResponseWriter, r *http.Request, nodeID int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.ensureServices()
	// Node-agent control-plane is mTLS only (no bearer tokens).
	if mtlsID, ok := nodeIDFromMTLS(r); !ok || mtlsID != nodeID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Best-effort observed IP and reported proxy params (for subscription rendering).
	if ip := a.clientIPFromRequest(r); ip != "" {
		_ = a.store.UpdateNodeObservedIP(r.Context(), nodeID, ip)
	}
	var rep repo.NodeReportInput
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
	} else {
		// If no JSON, keep backward compatibility (heartbeat with empty body).
		_ = r.Body.Close()
	}
	reportedAt, err := a.store.ApplyNodeReportLatest(r.Context(), nodeID, rep)
	if err != nil {
		http.Error(w, "apply node report failed", http.StatusInternalServerError)
		return
	}
	if rep.Metrics != nil {
		_ = a.metricHistoryQueue.Enqueue(nodeID, reportedAt, *rep.Metrics)
	}

	receivedAt := time.Now().UTC()
	_ = a.store.UpdateNodeAgentStatus(r.Context(), nodeID, &receivedAt, "")
	_ = a.ops().RefreshNode(r.Context(), nodeID)
	a.writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"timestamp":    receivedAt.Format(time.RFC3339),
		"last_seen_at": receivedAt.Format(time.RFC3339),
	})
}

func (a *App) handleAPINodeAgentUsers(w http.ResponseWriter, r *http.Request, nodeID int64) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Node-agent control-plane is mTLS only (no bearer tokens).
	if mtlsID, ok := nodeIDFromMTLS(r); !ok || mtlsID != nodeID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := a.users().RefreshLifecycleState(r.Context()); err != nil {
		http.Error(w, "refresh users failed", http.StatusInternalServerError)
		return
	}

	items, err := a.nodes().UsersForAgent(r.Context(), nodeID)
	if err != nil {
		http.Error(w, "list users failed", http.StatusInternalServerError)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"users": items})
}

func (a *App) handleAPINodeJobsClaim(w http.ResponseWriter, r *http.Request, nodeID int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Node-agent control-plane is mTLS only (no bearer tokens).
	if mtlsID, ok := nodeIDFromMTLS(r); !ok || mtlsID != nodeID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if ip := a.clientIPFromRequest(r); ip != "" {
		_ = a.store.UpdateNodeObservedIP(r.Context(), nodeID, ip)
	}
	// Treat claim polling as heartbeat even when no job is returned.
	now := time.Now().UTC()
	_ = a.store.UpdateNodeAgentStatus(r.Context(), nodeID, &now, "")
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
		node, err := a.store.GetNode(r.Context(), nodeID)
		if err != nil {
			http.Error(w, "claim failed", http.StatusInternalServerError)
			return
		}
		var allowedKinds []string
		if !node.Enabled {
			allowedKinds = []string{"users_sync"}
		}
		job, ok, err := a.store.ClaimNextNodeJobForNodeKinds(r.Context(), nodeID, allowedKinds)
		if err != nil {
			http.Error(w, "claim failed", http.StatusInternalServerError)
			return
		}
		if ok {
			now = time.Now().UTC()
			_ = a.store.UpdateNodeAgentStatus(r.Context(), nodeID, &now, "")
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

func (a *App) handleAPINodeJobFinish(w http.ResponseWriter, r *http.Request, nodeID int64, jobID int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Node-agent control-plane is mTLS only (no bearer tokens).
	if mtlsID, ok := nodeIDFromMTLS(r); !ok || mtlsID != nodeID {
		http.Error(w, "forbidden", http.StatusForbidden)
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
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Attempt <= 0 {
		http.Error(w, "attempt required", http.StatusBadRequest)
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
	final, err := a.store.FinishNodeJobForNode(r.Context(), nodeID, jobID, repo.FinishNodeJobInput{
		Status:      status,
		Retryable:   req.Retryable,
		HTTPStatus:  req.HTTPStatus,
		ResultJSON:  resultJSON,
		ErrorMsg:    req.Error,
		MaxAttempts: a.cfg.NodeJobMaxAttempts,
		Attempt:     req.Attempt,
	})
	if err != nil {
		if errors.Is(err, repo.ErrNodeJobNotRunning) {
			http.Error(w, "job is not running for this attempt", http.StatusConflict)
			return
		}
		if errors.Is(err, repo.ErrNodeJobInvalidStatus) {
			http.Error(w, "status must be succeeded or failed", http.StatusBadRequest)
			return
		}
		http.Error(w, "finish failed", http.StatusInternalServerError)
		return
	}

	job, ok, _ := a.store.GetNodeJob(r.Context(), jobID)
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
				_, _ = a.store.InsertNodeProbeResult(r.Context(), nodeID, repo.InsertNodeProbeResultInput{
					Kind:        kind,
					Target:      target,
					Success:     success,
					LatencyMS:   result.LatencyMS,
					StatusCode:  result.StatusCode,
					Error:       defaultString(result.Error, req.Error),
					CheckedAt:   checkedAt,
					SourceJobID: &sourceJobID,
				})
				_ = a.syncProbeOpsAlert(r.Context(), nodeID, kind, target, success, defaultString(result.Error, req.Error), checkedAt)
			}
		}
		if status == "succeeded" && strings.TrimSpace(req.AppliedVersion) != "" {
			switch job.Kind {
			case "users_sync":
				_ = a.store.SetNodeAppliedUsersVersion(r.Context(), nodeID, req.AppliedVersion)
			case "xray_apply":
				_ = a.store.SetNodeAppliedXrayVersion(r.Context(), nodeID, req.AppliedVersion)
				if managedXraySuccessNeedsUsersRepair(job.Kind, req.ResultJSON) {
					if err := a.nodes().EnqueueFullUsersSyncForNode(r.Context(), nodeID, "xray_apply_followup"); err != nil {
						log.Printf("enqueue users_sync xray_apply follow-up node=%d: %v", nodeID, err)
					}
				}
			case "xray_rollback":
				_ = a.store.MarkNodeXrayRolledBack(r.Context(), nodeID, req.AppliedVersion)
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
			_ = a.syncNodeJobOpsAlert(r.Context(), nodeID, job.Kind, "succeeded", "", time.Now().UTC())
		} else {
			kind := "retryable"
			if !req.Retryable {
				kind = "permanent"
			}
			_ = a.store.SetNodeJobError(r.Context(), nodeID, job.Kind, kind, summarizeNodeJobFailure(req.Error, req.ResultJSON))
			_ = a.syncNodeJobOpsAlert(r.Context(), nodeID, job.Kind, final, summarizeNodeJobFailure(req.Error, req.ResultJSON), time.Now().UTC())
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
		item, ok, err := a.store.GetNodeMetadata(r.Context(), nodeID)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
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
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if err := a.store.UpsertNodeMetadata(r.Context(), nodeID, in); err != nil {
			http.Error(w, "metadata update failed", http.StatusBadRequest)
			return
		}
		_ = a.ops().RefreshNode(r.Context(), nodeID)
		item, _, _ := a.store.GetNodeMetadata(r.Context(), nodeID)
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "item": item})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *App) handleAPINodeProbes(w http.ResponseWriter, r *http.Request, nodeID int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Kind string `json:"kind"`
		probe.Payload
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	kind := probe.NormalizeKind(req.Kind)
	if err := probe.Validate(kind, req.Payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body, _ := json.Marshal(req.Payload)
	jobID, enqueued, err := a.store.EnqueueNodeJob(r.Context(), nodeID, kind, "", string(body), probeTimeoutSec(req.Payload.TimeoutMS), probe.CorrelationID(kind, req.Payload))
	if err != nil {
		http.Error(w, "enqueue probe failed", http.StatusBadRequest)
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

func (a *App) handleAPINodeByIDV1Extended(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1/nodes/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	nodeID, err := parseInt64Path(parts[0])
	if err != nil {
		http.Error(w, "bad node id", http.StatusBadRequest)
		return
	}

	// Public enrollment (no auth, validated by enroll_code).
	if len(parts) == 2 && parts[1] == "enroll" && r.Method == http.MethodPost {
		a.handleAPINodeEnroll(w, r, nodeID)
		return
	}

	// Admin actions.
	if len(parts) == 3 && parts[1] == "cert" && parts[2] == "revoke" && r.Method == http.MethodPost {
		a.handleAPINodeCertRevoke(w, r, nodeID)
		return
	}
	if len(parts) == 2 && parts[1] == "enable" && r.Method == http.MethodPost {
		if err := a.nodes().SetEnabled(r.Context(), nodeID, true); err != nil {
			a.writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if act := actorFromRequest(a, r); act.typ != "" {
			_ = a.store.InsertAuditLog(r.Context(), act.typ, act.id, "node.enable", "node", fmt.Sprintf("%d", nodeID), "", a.clientIPFromRequest(r), r.UserAgent(), requestIDFromContext(r.Context()))
		}
		_ = a.ops().RefreshNode(r.Context(), nodeID)
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if len(parts) == 2 && parts[1] == "disable" && r.Method == http.MethodPost {
		if err := a.nodes().SetEnabled(r.Context(), nodeID, false); err != nil {
			a.writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if act := actorFromRequest(a, r); act.typ != "" {
			_ = a.store.InsertAuditLog(r.Context(), act.typ, act.id, "node.disable", "node", fmt.Sprintf("%d", nodeID), "", a.clientIPFromRequest(r), r.UserAgent(), requestIDFromContext(r.Context()))
		}
		_ = a.ops().RefreshNode(r.Context(), nodeID)
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	// Existing routes.
	if len(parts) == 2 && parts[1] == "report" {
		a.handleAPINodeReport(w, r, nodeID)
		return
	}
	if len(parts) == 2 && parts[1] == "jobs" && r.Method == http.MethodGet {
		limit := 50
		if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
			if parsed, err := parseInt64Path(v); err == nil {
				limit = int(parsed)
			}
		}
		items, err := a.nodes().ListJobs(r.Context(), nodeID, limit)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		a.writeJSON(w, http.StatusOK, map[string]any{"count": len(items), "items": items})
		return
	}
	if len(parts) == 2 && parts[1] == "metadata" {
		a.handleAPINodeMetadata(w, r, nodeID)
		return
	}
	if len(parts) == 2 && parts[1] == "probes" {
		a.handleAPINodeProbes(w, r, nodeID)
		return
	}
	if len(parts) == 2 && parts[1] == "probe-results" && r.Method == http.MethodGet {
		items, err := a.store.ListNodeProbeResults(r.Context(), nodeID, 100)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		a.writeJSON(w, http.StatusOK, map[string]any{"node_id": nodeID, "count": len(items), "items": items})
		return
	}
	if len(parts) == 2 && parts[1] == "metrics" && r.Method == http.MethodGet {
		rangeName := strings.TrimSpace(r.URL.Query().Get("range"))
		step := strings.TrimSpace(r.URL.Query().Get("step"))
		items, err := a.store.ListNodeMetricSeries(r.Context(), nodeID, rangeName, step, time.Now().UTC())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		a.writeJSON(w, http.StatusOK, map[string]any{
			"node_id": nodeID,
			"range":   defaultString(rangeName, "1h"),
			"step":    defaultString(step, "raw"),
			"count":   len(items),
			"items":   items,
		})
		return
	}
	if len(parts) == 3 && parts[1] == "metric-details" && parts[2] == "latest" && r.Method == http.MethodGet {
		item, ok, err := a.store.GetLatestNodeMetricDetails(r.Context(), nodeID)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		if !ok {
			a.writeJSON(w, http.StatusOK, map[string]any{"node_id": nodeID, "item": nil})
			return
		}
		a.writeJSON(w, http.StatusOK, map[string]any{"node_id": nodeID, "item": item})
		return
	}
	if len(parts) == 3 && parts[1] == "static-facts" && parts[2] == "latest" && r.Method == http.MethodGet {
		item, ok, err := a.store.GetLatestNodeStaticFacts(r.Context(), nodeID)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		if !ok {
			a.writeJSON(w, http.StatusOK, map[string]any{"node_id": nodeID, "item": nil})
			return
		}
		a.writeJSON(w, http.StatusOK, map[string]any{"node_id": nodeID, "item": item})
		return
	}
	if len(parts) == 3 && parts[1] == "static-facts" && parts[2] == "history" && r.Method == http.MethodGet {
		items, err := a.store.ListNodeStaticFacts(r.Context(), nodeID, 100)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		a.writeJSON(w, http.StatusOK, map[string]any{"node_id": nodeID, "count": len(items), "items": items})
		return
	}
	if len(parts) >= 2 && parts[1] == "agent" {
		if len(parts) == 3 && parts[2] == "users" {
			a.handleAPINodeAgentUsers(w, r, nodeID)
			return
		}
	}

	// Managed config deployment endpoints (panel-managed config -> desired state + job).
	if len(parts) == 4 && parts[1] == "managed" && parts[2] == "xray" && parts[3] == "deploy" && r.Method == http.MethodPost {
		result, err := a.nodes().DeployManagedXray(r.Context(), nodeID)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, repo.ErrNodeNotFound) {
				status = http.StatusNotFound
			} else if errors.Is(err, service.ErrInvalidManagedXrayConfig) || errors.Is(err, service.ErrNodeNotManaged) || errors.Is(err, service.ErrNodeCoreTypeNotXray) {
				status = http.StatusBadRequest
			}
			http.Error(w, err.Error(), status)
			return
		}
		if act := actorFromRequest(a, r); act.typ != "" {
			detail, _ := json.Marshal(map[string]any{"desired_version": result.DesiredVersion, "job_id": result.JobID, "enqueued": result.Enqueued})
			_ = a.store.InsertAuditLog(r.Context(), act.typ, act.id, "node.xray.deploy", "node", fmt.Sprintf("%d", nodeID), string(detail), a.clientIPFromRequest(r), r.UserAgent(), requestIDFromContext(r.Context()))
		}
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job_id": result.JobID, "enqueued": result.Enqueued, "desired_version": result.DesiredVersion})
		return
	}
	if len(parts) == 4 && parts[1] == "managed" && parts[2] == "xray" && parts[3] == "rollback" && r.Method == http.MethodPost {
		var req struct {
			BackupName string `json:"backup_name"`
			// Backward-compat: accept backup_path but only use its basename.
			BackupPath string `json:"backup_path"`
		}
		body, err := readAllLimit(r.Body, 64*1024)
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(string(body)) != "" {
			dec := json.NewDecoder(bytes.NewReader(body))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&req); err != nil {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}
		}
		backupName := strings.TrimSpace(req.BackupName)
		if backupName == "" && strings.TrimSpace(req.BackupPath) != "" {
			backupName = strings.TrimSpace(filepath.Base(req.BackupPath))
		}
		result, err := a.nodes().RollbackManagedXray(r.Context(), nodeID, backupName)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, repo.ErrNodeNotFound) {
				status = http.StatusNotFound
			} else if errors.Is(err, service.ErrInvalidManagedXrayConfig) || errors.Is(err, service.ErrNodeNotManaged) || errors.Is(err, service.ErrNodeCoreTypeNotXray) {
				status = http.StatusBadRequest
			}
			http.Error(w, err.Error(), status)
			return
		}
		if act := actorFromRequest(a, r); act.typ != "" {
			detail, _ := json.Marshal(map[string]any{"job_id": result.JobID, "enqueued": result.Enqueued})
			_ = a.store.InsertAuditLog(r.Context(), act.typ, act.id, "node.xray.rollback", "node", fmt.Sprintf("%d", nodeID), string(detail), a.clientIPFromRequest(r), r.UserAgent(), requestIDFromContext(r.Context()))
		}
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job_id": result.JobID, "enqueued": result.Enqueued})
		return
	}

	// Fall back to original handler for basic CRUD
	a.handleAPINodeByIDV1(w, r)
}

// handleAgentNodeRoutes is served on the dedicated agent mTLS listener. Keep this surface area minimal.
func (a *App) handleAgentNodeRoutes(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1/nodes/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) < 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	nodeID, err := parseInt64Path(parts[0])
	if err != nil || nodeID <= 0 {
		http.Error(w, "bad node id", http.StatusBadRequest)
		return
	}

	if len(parts) == 2 && parts[1] == "report" {
		a.handleAPINodeReport(w, r, nodeID)
		return
	}
	if len(parts) == 3 && parts[1] == "agent" && parts[2] == "users" {
		a.handleAPINodeAgentUsers(w, r, nodeID)
		return
	}
	if len(parts) == 3 && parts[1] == "jobs" && parts[2] == "claim" {
		a.handleAPINodeJobsClaim(w, r, nodeID)
		return
	}
	if len(parts) == 4 && parts[1] == "jobs" && parts[3] == "finish" {
		jobID, err := parseInt64Path(parts[2])
		if err != nil {
			http.Error(w, "bad job id", http.StatusBadRequest)
			return
		}
		a.handleAPINodeJobFinish(w, r, nodeID, jobID)
		return
	}
	if len(parts) == 3 && parts[1] == "cert" && parts[2] == "renew" {
		a.handleAPINodeCertRenew(w, r, nodeID)
		return
	}

	http.NotFound(w, r)
}
