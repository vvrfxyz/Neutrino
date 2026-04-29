package agent

import (
	"context"
	"fmt"
	"strings"

	gnet "github.com/shirou/gopsutil/v3/net"

	"neutrino/internal/hostnet"
)

func (a *Agent) readNetTotals(ctx context.Context) (int64, int64, string, error) {
	if a != nil {
		if procPath := strings.TrimSpace(a.cfg.HostProcPath); procPath != "" {
			if totals, source, err := hostnet.ReadFromProcWithSource(procPath); err == nil {
				return totals.RX, totals.TX, source, nil
			}
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
