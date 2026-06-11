package app

import (
	"context"
	"time"
)

// syncOpsAlerts derives generated per-node ops alerts from the same snapshot
// rows the /ops dashboard renders. The rules live in service.AlertService.
func (a *App) syncOpsAlerts(ctx context.Context, now time.Time) error {
	items, err := a.buildOpsNodesItems(ctx)
	if err != nil {
		return err
	}
	return a.alerts().SyncFromNodeItems(ctx, items, now)
}
