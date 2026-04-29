package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHeartbeatMetrics_UsesHostProcPathForNetTelemetry(t *testing.T) {
	procDir := t.TempDir()
	writeProcFile(t, procDir, "net/route", `Iface	Destination	Gateway	Flags	RefCnt	Use	Metric	Mask	MTU	Window	IRTT
eth0	00000000	01010101	0003	0	0	0	00000000	0	0	0
`)
	writeProcFile(t, procDir, "net/dev", `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
  eth0: 1000 0 0 0 0 0 0 0 2000 0 0 0 0 0 0 0
`)

	statePath := filepath.Join(t.TempDir(), "state.json")
	ag := &Agent{
		cfg: Config{
			HostProcPath: procDir,
			StatePath:    statePath,
		},
		state: NewStateStore(statePath),
	}

	first := ag.heartbeatMetrics(time.Date(2026, 4, 9, 9, 0, 0, 0, time.UTC))
	if first.NetRXTotal != 1000 || first.NetTXTotal != 2000 {
		t.Fatalf("expected first sample to report raw totals, got rx=%d tx=%d", first.NetRXTotal, first.NetTXTotal)
	}
	if first.MonthKey != "2026-04" || first.NetSource == "" {
		t.Fatalf("missing month metadata/source: %+v", first)
	}

	writeProcFile(t, procDir, "net/dev", `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
  eth0: 2500 0 0 0 0 0 0 0 5000 0 0 0 0 0 0 0
`)

	second := ag.heartbeatMetrics(time.Date(2026, 4, 9, 9, 0, 30, 0, time.UTC))
	if second.NetRXTotal != 2500 || second.NetTXTotal != 5000 {
		t.Fatalf("unexpected raw totals rx=%d tx=%d", second.NetRXTotal, second.NetTXTotal)
	}

	if got := int64(second.InboundBPS); got != 50 {
		t.Fatalf("unexpected inbound bps=%v", second.InboundBPS)
	}
	if got := int64(second.OutboundBPS); got != 100 {
		t.Fatalf("unexpected outbound bps=%v", second.OutboundBPS)
	}

	rx, tx, src, err := ag.readNetTotals(context.Background())
	if err != nil {
		t.Fatalf("readNetTotals: %v", err)
	}
	if rx != 2500 || tx != 5000 {
		t.Fatalf("unexpected fallback net totals rx=%d tx=%d", rx, tx)
	}
	if !strings.Contains(src, "source=proc:"+procDir) || !strings.Contains(src, "names=eth0") {
		t.Fatalf("expected proc source, got %q", src)
	}
}

func writeProcFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
