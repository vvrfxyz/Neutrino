package monitor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
	gnet "github.com/shirou/gopsutil/v3/net"

	"neutrino/internal/hostnet"
)

func readNetTotals(ctx context.Context, hostProcPath string) (int64, int64, string, error) {
	hostProcPath = strings.TrimSpace(hostProcPath)
	if hostProcPath != "" {
		totals, source, err := hostnet.ReadFromProcWithSource(hostProcPath)
		if err == nil {
			return totals.RX, totals.TX, source, nil
		}
	}
	ioCounters, err := gnet.IOCountersWithContext(ctx, true)
	if err != nil {
		return 0, 0, "", err
	}
	if len(ioCounters) == 0 {
		return 0, 0, "", fmt.Errorf("no network counters")
	}
	byIface := make(map[string]hostnet.NetTotals, len(ioCounters))
	for _, counter := range ioCounters {
		byIface[counter.Name] = hostnet.NetTotals{
			RX: hostnet.ClampInt64FromUint64(counter.BytesRecv),
			TX: hostnet.ClampInt64FromUint64(counter.BytesSent),
		}
	}
	totals, names, ok := hostnet.SumPreferredIfaceTotalsWithNames(byIface)
	if !ok {
		return 0, 0, "", fmt.Errorf("no usable network counters")
	}
	return totals.RX, totals.TX, hostnet.CounterSource("gopsutil:pernic", names), nil
}

type Snapshot struct {
	Timestamp     time.Time `json:"timestamp"`
	CPUPercent    float64   `json:"cpu_percent"`
	MemoryBytes   uint64    `json:"memory_bytes"`
	InboundTotal  int64     `json:"inbound_total"`
	OutboundTotal int64     `json:"outbound_total"`
	InboundBPS    float64   `json:"inbound_bps"`
	OutboundBPS   float64   `json:"outbound_bps"`
	ProxyInTotal  int64     `json:"proxy_in_total"`
	ProxyOutTotal int64     `json:"proxy_out_total"`
}

type TrafficFetcher func(context.Context) (int64, int64, error)

type HostMonitor struct {
	hostProcPath string
	mu           sync.RWMutex
	samples      []Snapshot
	maxItems     int
}

func NewHostMonitor(maxItems int, hostProcPath string) *HostMonitor {
	if maxItems <= 0 {
		maxItems = 720
	}
	return &HostMonitor{hostProcPath: strings.TrimSpace(hostProcPath), maxItems: maxItems}
}

func (m *HostMonitor) Start(ctx context.Context, interval time.Duration, fetch TrafficFetcher) {
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var (
		lastNetIn  int64
		lastNetOut int64
		hasLast    bool
		lastTime   time.Time
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UTC()
			proxyInTotal, proxyOutTotal := int64(0), int64(0)
			if fetch != nil {
				if in, out, err := fetch(ctx); err == nil {
					proxyInTotal, proxyOutTotal = in, out
				}
			}
			netInTotal, netOutTotal := int64(0), int64(0)
			if rx, tx, _, err := readNetTotals(ctx, m.hostProcPath); err == nil {
				netInTotal = rx
				netOutTotal = tx
			}
			inBPS, outBPS := float64(0), float64(0)
			if hasLast {
				dt := now.Sub(lastTime).Seconds()
				if dt > 0 {
					inBPS = float64(netInTotal-lastNetIn) / dt
					outBPS = float64(netOutTotal-lastNetOut) / dt
					if inBPS < 0 {
						inBPS = 0
					}
					if outBPS < 0 {
						outBPS = 0
					}
				}
			}
			memBytes := uint64(0)
			if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
				memBytes = vm.Used
			}
			cpuPercent := float64(0)
			if cpuList, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(cpuList) > 0 {
				cpuPercent = cpuList[0]
			}
			snap := Snapshot{
				Timestamp:     now,
				CPUPercent:    cpuPercent,
				MemoryBytes:   memBytes,
				InboundTotal:  netInTotal,
				OutboundTotal: netOutTotal,
				InboundBPS:    inBPS,
				OutboundBPS:   outBPS,
				ProxyInTotal:  proxyInTotal,
				ProxyOutTotal: proxyOutTotal,
			}
			m.mu.Lock()
			m.samples = append(m.samples, snap)
			if len(m.samples) > m.maxItems {
				m.samples = m.samples[len(m.samples)-m.maxItems:]
			}
			m.mu.Unlock()
			lastNetIn, lastNetOut, lastTime, hasLast = netInTotal, netOutTotal, now, true
		}
	}
}

func (m *HostMonitor) Query(rangeName string) []Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.samples) == 0 {
		return nil
	}
	now := time.Now().UTC()
	cutoff := now.Add(-1 * time.Hour)
	switch strings.TrimSpace(rangeName) {
	case "6h":
		cutoff = now.Add(-6 * time.Hour)
	case "24h":
		cutoff = now.Add(-24 * time.Hour)
	case "1h", "":
		cutoff = now.Add(-1 * time.Hour)
	}
	out := make([]Snapshot, 0, len(m.samples))
	for _, item := range m.samples {
		if item.Timestamp.After(cutoff) {
			out = append(out, item)
		}
	}
	return out
}
