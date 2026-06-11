package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

func (a *App) handleAPIOpsNodesV1(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	items, err := a.buildOpsNodesItems(r.Context())
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"count": len(items), "items": items})
}

func (a *App) handleAPIOpsConfigV1(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snapshot := a.cfg.OpsSnapshotInterval()
	a.writeJSON(w, http.StatusOK, map[string]any{
		"ops_snapshot_interval_sec": int(snapshot / time.Second),
		"poll_interval_ms":          int(snapshot / time.Millisecond),
		"enrich_interval_ms":        30000,
	})
}

func (a *App) handleAPIOpsAlertsV1(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		status := strings.TrimSpace(r.URL.Query().Get("status"))
		items, err := a.alerts().List(r.Context(), status, 200)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		a.writeJSON(w, http.StatusOK, map[string]any{"count": len(items), "items": items})
	case http.MethodPost:
		var req struct {
			Action    string `json:"action"`
			NodeID    *int64 `json:"node_id"`
			Kind      string `json:"kind"`
			Severity  string `json:"severity"`
			Message   string `json:"message"`
			DedupeKey string `json:"dedupe_key"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Action) == "resolve" {
			ok, err := a.alerts().Resolve(r.Context(), req.DedupeKey, time.Now().UTC())
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			a.writeJSON(w, http.StatusOK, map[string]any{"ok": ok})
			return
		}
		id, err := a.alerts().Upsert(r.Context(), req.NodeID, req.Kind, req.Severity, req.Message, req.DedupeKey, time.Now().UTC())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		a.writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "id": id})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// buildOpsNodesItems is the single source of truth for the per-node row that
// the /ops dashboard renders. Both the polling endpoint and the WebSocket
// publisher call it so the two paths cannot drift apart in shape.
func (a *App) buildOpsNodesItems(ctx context.Context) ([]map[string]any, error) {
	return a.ops().BuildNodeItems(ctx)
}
