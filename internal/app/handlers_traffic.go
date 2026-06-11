package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"neutrino/internal/repo"
)

type UsageEventInput struct {
	UserID        int64  `json:"user_id"`
	NodeID        *int64 `json:"node_id,omitempty"`
	Direction     string `json:"direction"`
	Bytes         int64  `json:"bytes"`
	Source        string `json:"source"`
	SourceEventID string `json:"source_event_id"`
	TargetHost    string `json:"target_host,omitempty"`
	TargetIP      string `json:"target_ip,omitempty"`
	TargetPort    int64  `json:"target_port,omitempty"`
	SNI           string `json:"sni,omitempty"`
	Destination   string `json:"destination,omitempty"`
	ClientIP      string `json:"client_ip,omitempty"`
	InboundTag    string `json:"inbound_tag,omitempty"`
	At            string `json:"at,omitempty"`
}

type UsageRequest struct {
	Events []UsageEventInput `json:"events"`
}

// usageErrorResult builds a per-event error result. The legacy "error" string
// is the wire contract with old agents and must not be reworded; "code" and
// "permanent" are the machine-readable fields new agents prefer (permanent
// means deterministic for the event content — retrying can never succeed).
func usageErrorResult(userID int64, code string, permanent bool, legacyMsg string) map[string]any {
	return map[string]any{
		"user_id":   userID,
		"error":     legacyMsg,
		"code":      code,
		"permanent": permanent,
	}
}

func (a *App) handleAPIUsageV1(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req UsageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if len(req.Events) == 0 {
		http.Error(w, "events required", http.StatusBadRequest)
		return
	}

	if err := a.users().RefreshLifecycleState(r.Context()); err != nil {
		log.Printf("api usage lifecycle refresh failed: %v", err)
		http.Error(w, "record failed", http.StatusInternalServerError)
		return
	}

	apiKeyCtx := getAPIKeyContext(r.Context())
	var forceNodeID *int64
	if apiKeyCtx != nil && apiKeyCtx.NodeID != nil {
		forceNodeID = apiKeyCtx.NodeID
	}

	inputs := make([]repo.UsageInput, len(req.Events))
	ready := make([]bool, len(req.Events))
	results := make([]map[string]any, len(req.Events))

	for i, evt := range req.Events {
		if evt.SourceEventID == "" {
			results[i] = usageErrorResult(evt.UserID, "missing_source_event_id", true, "source_event_id required")
			continue
		}
		if strings.TrimSpace(evt.Source) == "" {
			results[i] = usageErrorResult(evt.UserID, "missing_source", true, "source required")
			continue
		}

		nodeID := evt.NodeID
		if forceNodeID != nil {
			nodeID = forceNodeID
		}

		eventAt := time.Now().UTC()
		if evt.At != "" {
			parsed, err := time.Parse(time.RFC3339, evt.At)
			if err != nil {
				results[i] = usageErrorResult(evt.UserID, "bad_timestamp_format", true, "invalid event timestamp")
				continue
			}
			eventAt = parsed.UTC()
		}

		inputs[i] = repo.UsageInput{
			UserID:        evt.UserID,
			NodeID:        nodeID,
			Direction:     evt.Direction,
			Bytes:         evt.Bytes,
			At:            eventAt,
			Source:        evt.Source,
			SourceEventID: evt.SourceEventID,
			TargetHost:    evt.TargetHost,
			TargetIP:      evt.TargetIP,
			TargetPort:    evt.TargetPort,
			SNI:           evt.SNI,
			Destination:   evt.Destination,
			ClientIP:      evt.ClientIP,
			InboundTag:    evt.InboundTag,
		}
		ready[i] = true
	}

	batch := make([]repo.UsageInput, 0, len(req.Events))
	indexes := make([]int, 0, len(req.Events))
	for i := range ready {
		if ready[i] {
			batch = append(batch, inputs[i])
			indexes = append(indexes, i)
		}
	}

	if len(batch) > 0 {
		batchResults, err := a.usage().RecordBatch(r.Context(), batch)
		if err != nil {
			log.Printf("api usage record batch failed events=%d: %v", len(batch), err)
			http.Error(w, "record failed", http.StatusInternalServerError)
			return
		}
		for i, br := range batchResults {
			idx := indexes[i]
			if br.Err != nil {
				if errors.Is(br.Err, repo.ErrUserNotFound) {
					results[idx] = usageErrorResult(req.Events[idx].UserID, "user_not_found", true, "user not found")
					continue
				}
				if errors.Is(br.Err, repo.ErrUserNotAllowedOnNode) {
					results[idx] = usageErrorResult(req.Events[idx].UserID, "user_not_allowed_on_node", true, "user not allowed on node")
					continue
				}
				if errors.Is(br.Err, repo.ErrUserInactive) {
					results[idx] = usageErrorResult(req.Events[idx].UserID, "user_inactive", true, "user not active")
					continue
				}
				if errors.Is(br.Err, repo.ErrUsageTimestampTooOld) {
					results[idx] = usageErrorResult(req.Events[idx].UserID, "timestamp_too_old", true, "event timestamp too old")
					continue
				}
				if errors.Is(br.Err, repo.ErrUsageTimestampSkew) {
					// Future-skewed timestamps become valid as the clock advances.
					results[idx] = usageErrorResult(req.Events[idx].UserID, "timestamp_skew", false, "invalid event timestamp")
					continue
				}
				results[idx] = usageErrorResult(req.Events[idx].UserID, "record_failed", false, "record failed")
				continue
			}
			if br.Duplicate {
				results[idx] = map[string]any{
					"user_id":      req.Events[idx].UserID,
					"status":       "duplicate",
					"deduplicated": true,
				}
				continue
			}
			if br.User == nil {
				results[idx] = usageErrorResult(req.Events[idx].UserID, "record_failed", false, "record failed")
				continue
			}
			results[idx] = map[string]any{
				"user_id":          br.User.ID,
				"status":           br.User.Status,
				"window_effective": br.User.WindowEffective,
			}
		}
	}
	for i := range results {
		if results[i] == nil {
			results[i] = usageErrorResult(req.Events[i].UserID, "record_failed", false, "record failed")
		}
	}

	a.writeJSON(w, http.StatusOK, map[string]any{
		"processed": len(results),
		"results":   results,
	})
}

func (a *App) handleAPITrafficSummaryV1(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rangeParam := r.URL.Query().Get("range")
	if rangeParam == "" {
		rangeParam = "24h"
	}

	var nodeID *int64
	if nodeIDStr := r.URL.Query().Get("node_id"); nodeIDStr != "" {
		if id, err := strconv.ParseInt(nodeIDStr, 10, 64); err == nil && id > 0 {
			nodeID = &id
		}
	}
	var userID *int64
	if userIDStr := r.URL.Query().Get("user_id"); userIDStr != "" {
		if id, err := strconv.ParseInt(userIDStr, 10, 64); err == nil && id > 0 {
			userID = &id
		}
	}

	summary, err := a.store.GetTrafficSummary(r.Context(), rangeParam, nodeID, userID)
	if err != nil {
		http.Error(w, fmt.Sprintf("query failed: %v", err), http.StatusInternalServerError)
		return
	}

	a.writeJSON(w, http.StatusOK, summary)
}
