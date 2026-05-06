package app

import (
	"context"
	"net/http"
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

// buildOpsNodesItems is the single source of truth for the per-node row that
// the /ops dashboard renders. Both the polling endpoint and the WebSocket
// publisher call it so the two paths cannot drift apart in shape.
func (a *App) buildOpsNodesItems(ctx context.Context) ([]map[string]any, error) {
	return a.ops().BuildNodeItems(ctx)
}
