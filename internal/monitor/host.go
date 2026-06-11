package monitor

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"

	"neutrino/internal/hostnet"
)

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

// readNetTotals is a seam for tests; production always uses hostnet.ReadTotals.
var readNetTotals = hostnet.ReadTotals

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
			snap, netReadOK := m.sample(ctx, now, fetch, lastNetIn, lastNetOut, lastTime, hasLast)
			m.mu.Lock()
			m.samples = append(m.samples, snap)
			if len(m.samples) > m.maxItems {
				m.samples = m.samples[len(m.samples)-m.maxItems:]
			}
			m.mu.Unlock()
			// On a transient hostnet read failure the snapshot carries the
			// previous totals; only a successful read advances the counters,
			// otherwise the next delta would be computed against zero and
			// record a huge false BPS spike.
			if netReadOK {
				lastNetIn, lastNetOut, lastTime, hasLast = snap.InboundTotal, snap.OutboundTotal, now, true
			}
		}
	}
}

func (m *HostMonitor) sample(ctx context.Context, now time.Time, fetch TrafficFetcher, lastNetIn, lastNetOut int64, lastTime time.Time, hasLast bool) (Snapshot, bool) {
	proxyInTotal, proxyOutTotal := int64(0), int64(0)
	if fetch != nil {
		if in, out, err := fetch(ctx); err == nil {
			proxyInTotal, proxyOutTotal = in, out
		}
	}
	netInTotal, netOutTotal := lastNetIn, lastNetOut
	netReadOK := false
	if totals, _, err := readNetTotals(ctx, m.hostProcPath); err == nil {
		netInTotal, netOutTotal = totals.RX, totals.TX
		netReadOK = true
	}
	inBPS, outBPS := float64(0), float64(0)
	if netReadOK && hasLast {
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
	return Snapshot{
		Timestamp:     now,
		CPUPercent:    cpuPercent,
		MemoryBytes:   memBytes,
		InboundTotal:  netInTotal,
		OutboundTotal: netOutTotal,
		InboundBPS:    inBPS,
		OutboundBPS:   outBPS,
		ProxyInTotal:  proxyInTotal,
		ProxyOutTotal: proxyOutTotal,
	}, netReadOK
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

func (m *HostMonitor) Latest() (Snapshot, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.samples) == 0 {
		return Snapshot{}, false
	}
	return m.samples[len(m.samples)-1], true
}
