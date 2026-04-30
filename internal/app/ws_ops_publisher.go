package app

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"neutrino/internal/repo"
)

const opsSnapshotInterval = 5 * time.Second

// startOpsSnapshotPublisher emits an "ops_snapshot" event over the wsHub on
// a fixed cadence. It only runs the (potentially expensive) DB queries when
// at least one subscriber is connected, so an idle panel pays nothing.
func (a *App) startOpsSnapshotPublisher(ctx context.Context) {
	t := time.NewTicker(opsSnapshotInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if a.wsHub.subscriberCount() == 0 {
				continue
			}
			snap, err := a.buildOpsSnapshot(ctx)
			if err != nil {
				log.Printf("ops snapshot build error: %v", err)
				continue
			}
			payload, err := json.Marshal(snap)
			if err != nil {
				log.Printf("ops snapshot marshal error: %v", err)
				continue
			}
			a.wsHub.publish(wsEvent{
				Kind: "ops_snapshot",
				At:   time.Now().UTC(),
				Data: payload,
			})
		}
	}
}

// opsSnapshot is the payload shape pushed to /api/v1/stream subscribers.
//
// IMPORTANT: each sub-shape must stay in lock-step with the corresponding
// polling endpoint so the /ops dashboard renders identical fields whether
// it's reading from WebSocket pushes or its 5s polling fallback:
//
//   host    → /api/v1/metrics/host (latest item + month)
//   online  → /api/v1/online-users (items)
//   nodes   → /api/v1/ops/nodes    (items, via buildOpsNodesItems)
type opsSnapshot struct {
	Host   *opsHostSnapshot   `json:"host,omitempty"`
	Online []repo.OnlineUser  `json:"online"`
	Nodes  []map[string]any   `json:"nodes"`
}

type opsHostSnapshot struct {
	CPUPercent  float64           `json:"cpu_percent"`
	MemoryBytes uint64            `json:"memory_bytes"`
	InboundBPS  float64           `json:"inbound_bps"`
	OutboundBPS float64           `json:"outbound_bps"`
	Month       *opsMonthSnapshot `json:"month,omitempty"`
}

type opsMonthSnapshot struct {
	RXBytes    int64 `json:"rx_bytes"`
	TXBytes    int64 `json:"tx_bytes"`
	TotalBytes int64 `json:"total_bytes"`
}

func (a *App) buildOpsSnapshot(ctx context.Context) (opsSnapshot, error) {
	out := opsSnapshot{
		Online: []repo.OnlineUser{},
		Nodes:  []map[string]any{},
	}

	if a.hostMonitor != nil {
		items := a.hostMonitor.Query("1h")
		if n := len(items); n > 0 {
			last := items[n-1]
			out.Host = &opsHostSnapshot{
				CPUPercent:  last.CPUPercent,
				MemoryBytes: last.MemoryBytes,
				InboundBPS:  last.InboundBPS,
				OutboundBPS: last.OutboundBPS,
			}
		}
	}

	if mu, ok, err := a.store.GetHostNetMonthlyUsage(ctx, time.Now()); err == nil && ok {
		if out.Host == nil {
			out.Host = &opsHostSnapshot{}
		}
		out.Host.Month = &opsMonthSnapshot{
			RXBytes:    mu.RXBytes,
			TXBytes:    mu.TXBytes,
			TotalBytes: mu.RXBytes + mu.TXBytes,
		}
	}

	if onlines, err := a.store.ListOnlineUsers(ctx, a.cfg.OnlineDisplayWindowSec); err == nil && len(onlines) > 0 {
		out.Online = onlines
	}

	nodes, err := a.buildOpsNodesItems(ctx)
	if err != nil {
		return out, err
	}
	if len(nodes) > 0 {
		out.Nodes = nodes
	}
	return out, nil
}
