package app

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"
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

// opsSnapshot mirrors the shape the /ops dashboard expects so the client
// can update its DOM from a single push without issuing the three polling
// requests.
type opsSnapshot struct {
	Host   *opsHostSnapshot         `json:"host,omitempty"`
	Online []map[string]any         `json:"online"`
	Nodes  []map[string]any         `json:"nodes"`
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
		Online: []map[string]any{},
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

	if onlines, err := a.store.ListOnlineUsers(ctx, a.cfg.OnlineDisplayWindowSec); err == nil {
		for _, o := range onlines {
			out.Online = append(out.Online, map[string]any{
				"user_id":     o.UserID,
				"username":    o.Username,
				"client_ip":   o.ClientIP,
				"last_seen":   fmtMaybeTime(&o.LastSeen),
				"first_seen":  fmtMaybeTime(&o.FirstSeen),
			})
		}
	}

	nodes, err := a.store.ListNodes(ctx)
	if err != nil {
		return out, err
	}
	metricsByNode, _ := a.store.ListNodeRuntimeMetrics(ctx)
	staleAfter := 120 * time.Second
	for _, n := range nodes {
		health := "unknown"
		switch {
		case !n.Enabled:
			health = "disabled"
		case n.LastSeenAt != nil && time.Since(*n.LastSeenAt) <= staleAfter:
			health = "online"
		case n.LastSeenAt != nil:
			health = "stale"
		}
		item := map[string]any{
			"id":           n.ID,
			"name":         n.Name,
			"enabled":      n.Enabled,
			"managed":      n.Managed,
			"health":       health,
			"last_seen_at": fmtMaybeTime(n.LastSeenAt),
			"last_error":   strings.TrimSpace(n.LastError),
		}
		if m, ok := metricsByNode[n.ID]; ok {
			item["agent_metrics"] = map[string]any{
				"cpu_percent":       m.CPUPercent,
				"memory_bytes":      m.MemoryBytes,
				"inbound_bps":       m.InboundBPS,
				"outbound_bps":      m.OutboundBPS,
				"disk_used_percent": m.DiskUsedPercent,
				"uptime_sec":        m.UptimeSec,
				"queue_batches":     m.QueueBatches,
			}
			item["agent_metrics_at"] = fmtMaybeTime(&m.UpdatedAt)
		}
		out.Nodes = append(out.Nodes, item)
	}
	return out, nil
}
