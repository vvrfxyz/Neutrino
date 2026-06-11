package app

import (
	"context"
	"strings"
)

func (a *App) dispatchPendingAlerts(ctx context.Context) error {
	to := make([]string, 0, 4)
	for _, p := range strings.Split(a.cfg.SMTPTo, ",") {
		if v := strings.TrimSpace(p); v != "" {
			to = append(to, v)
		}
	}
	return a.alerts().DispatchPending(ctx, a.notifier, to)
}
