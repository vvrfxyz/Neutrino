package agent

import (
	"context"

	"neutrino/internal/hostnet"
)

func (a *Agent) readNetTotals(ctx context.Context) (int64, int64, string, error) {
	procPath := ""
	if a != nil {
		procPath = a.cfg.HostProcPath
	}
	totals, source, err := hostnet.ReadTotals(ctx, procPath)
	if err != nil {
		return 0, 0, "", err
	}
	return totals.RX, totals.TX, source, nil
}
