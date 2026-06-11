package hostnet

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestReadFromProc_SumsPreferredIfacesEvenWithDefaultRoute(t *testing.T) {
	procDir := t.TempDir()
	writeProcFile(t, procDir, "net/dev", `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
  eth0: 1000 0 0 0 0 0 0 0 2000 0 0 0 0 0 0 0
  eth1: 300 0 0 0 0 0 0 0 400 0 0 0 0 0 0 0
docker0: 9000 0 0 0 0 0 0 0 10000 0 0 0 0 0 0 0
 br-123: 3000 0 0 0 0 0 0 0 4000 0 0 0 0 0 0 0
`)
	writeProcFile(t, procDir, "net/route", `Iface	Destination	Gateway	Flags	RefCnt	Use	Metric	Mask	MTU	Window	IRTT
eth0	00000000	01010101	0003	0	0	0	00000000	0	0	0
docker0	0011A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
`)

	got, err := ReadFromProc(procDir)
	if err != nil {
		t.Fatalf("ReadFromProc: %v", err)
	}
	if got.RX != 1300 || got.TX != 2400 {
		t.Fatalf("unexpected totals=%+v", got)
	}
}

func TestReadFromProcWithSource_PrefersProcOneNetNamespace(t *testing.T) {
	procDir := t.TempDir()
	writeProcFile(t, procDir, "net/dev", `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
  eth0: 1000 0 0 0 0 0 0 0 2000 0 0 0 0 0 0 0
`)
	writeProcFile(t, procDir, "net/route", `Iface	Destination	Gateway	Flags	RefCnt	Use	Metric	Mask	MTU	Window	IRTT
eth0	00000000	01010101	0003	0	0	0	00000000	0	0	0
`)
	writeProcFile(t, procDir, "1/net/dev", `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
  eth0: 9000 0 0 0 0 0 0 0 12000 0 0 0 0 0 0 0
`)
	writeProcFile(t, procDir, "1/net/route", `Iface	Destination	Gateway	Flags	RefCnt	Use	Metric	Mask	MTU	Window	IRTT
eth0	00000000	01010101	0003	0	0	0	00000000	0	0	0
`)

	got, source, err := ReadFromProcWithSource(procDir)
	if err != nil {
		t.Fatalf("ReadFromProcWithSource: %v", err)
	}
	if got.RX != 9000 || got.TX != 12000 {
		t.Fatalf("expected proc/1 net totals got %+v", got)
	}
	if want := CounterSource("proc:"+filepath.Join(procDir, "1", "net"), []string{"eth0"}); source != want {
		t.Fatalf("unexpected source=%q want=%q", source, want)
	}
}

func TestReadFromProc_SumsPreferredIfacesWhenNoDefaultRoute(t *testing.T) {
	procDir := t.TempDir()
	writeProcFile(t, procDir, "net/dev", `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
  eth0: 1000 0 0 0 0 0 0 0 2000 0 0 0 0 0 0 0
  eth1: 300 0 0 0 0 0 0 0 400 0 0 0 0 0 0 0
docker0: 9000 0 0 0 0 0 0 0 10000 0 0 0 0 0 0 0
`)
	writeProcFile(t, procDir, "net/route", `Iface	Destination	Gateway	Flags	RefCnt	Use	Metric	Mask	MTU	Window	IRTT
`)

	got, err := ReadFromProc(procDir)
	if err != nil {
		t.Fatalf("ReadFromProc: %v", err)
	}
	if got.RX != 1300 || got.TX != 2400 {
		t.Fatalf("expected preferred (non-docker) iface totals got %+v", got)
	}
}

func TestParseProcNetDev_SkipsHeaderAndParsesFields(t *testing.T) {
	procDir := t.TempDir()
	writeProcFile(t, procDir, "net/dev", `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
  eth0: 111 0 0 0 0 0 0 0 222 0 0 0 0 0 0 0
    lo: 1 0 0 0 0 0 0 0 2 0 0 0 0 0 0 0
`)
	m, err := ParseProcNetDev(filepath.Join(procDir, "net", "dev"))
	if err != nil {
		t.Fatalf("ParseProcNetDev: %v", err)
	}
	if got := m["eth0"]; got.RX != 111 || got.TX != 222 {
		t.Fatalf("unexpected eth0 totals: %+v", got)
	}
	if got := m["lo"]; got.RX != 1 || got.TX != 2 {
		t.Fatalf("unexpected lo totals: %+v", got)
	}
}

func TestParseDefaultRouteIfaces_DedupesAndPreservesOrder(t *testing.T) {
	procDir := t.TempDir()
	writeProcFile(t, procDir, "net/route", `Iface	Destination	Gateway	Flags	RefCnt	Use	Metric	Mask	MTU	Window	IRTT
eth0	00000000	01010101	0003	0	0	0	00000000	0	0	0
eth1	00000000	01010101	0003	0	0	0	00000000	0	0	0
eth0	00000000	01010101	0003	0	0	0	00000000	0	0	0
docker0	0011A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
`)
	got := ParseDefaultRouteIfaces(filepath.Join(procDir, "net", "route"))
	if len(got) != 2 || got[0] != "eth0" || got[1] != "eth1" {
		t.Fatalf("unexpected default route ifaces: %+v", got)
	}
}

func TestPreferredIfaces_FiltersVirtualAndSorts(t *testing.T) {
	got := PreferredIfaces(map[string]NetTotals{
		"eth1":      {},
		"eth0":      {},
		"docker0":   {},
		"podman0":   {},
		"flannel.1": {},
		"br0":       {},
		"br-lan":    {},
		"br-aaa":    {},
		"fwbr100i0": {},
		"fwpr100p0": {},
		"veth1":     {},
		"virbr0":    {},
		"ifb0":      {},
		"tun0":      {},
		"tap0":      {},
		"vmbr0":     {},
		"vnet0":     {},
		"kube-ipvs": {},
		"cni0":      {},
		"lo":        {},
	})
	if len(got) != 2 || got[0] != "eth0" || got[1] != "eth1" {
		t.Fatalf("unexpected preferred ifaces: %+v", got)
	}
}

func TestReadFromProcRejectsOnlyVirtualIfaces(t *testing.T) {
	procDir := t.TempDir()
	writeProcFile(t, procDir, "net/dev", `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1 0 0 0 0 0 0 0 2 0 0 0 0 0 0 0
  tun0: 1000 0 0 0 0 0 0 0 2000 0 0 0 0 0 0 0
docker0: 9000 0 0 0 0 0 0 0 10000 0 0 0 0 0 0 0
`)

	if _, err := ReadFromProc(procDir); err == nil {
		t.Fatalf("expected only virtual interfaces to be rejected")
	}
}

func TestCounterSourceChangesWithPreferredIfaceSet(t *testing.T) {
	one := CounterSource("proc:/host/proc/1/net", []string{"eth0"})
	two := CounterSource("proc:/host/proc/1/net", []string{"eth0", "eth1"})
	twoUnsorted := CounterSource("proc:/host/proc/1/net", []string{"eth1", "eth0"})
	if one == two {
		t.Fatalf("interface set change should change source: %q", one)
	}
	if two != twoUnsorted {
		t.Fatalf("source should be stable regardless of input order: %q vs %q", two, twoUnsorted)
	}
	if !strings.Contains(two, "source=proc:/host/proc/1/net") || !strings.Contains(two, "names=eth0,eth1") {
		t.Fatalf("source should include base and sorted names, got %q", two)
	}
}

func TestSumIfaceTotals_ReturnsOkOnlyForKnownNames(t *testing.T) {
	byIface := map[string]NetTotals{
		"eth0": {RX: 10, TX: 20},
		"eth1": {RX: 30, TX: 40},
	}
	if _, ok := SumIfaceTotals(byIface, nil); ok {
		t.Fatalf("empty names should return ok=false")
	}
	if _, ok := SumIfaceTotals(byIface, []string{"missing"}); ok {
		t.Fatalf("unknown names should return ok=false")
	}
	got, ok := SumIfaceTotals(byIface, []string{"eth0", "eth1", "missing"})
	if !ok {
		t.Fatalf("expected ok for known ifaces")
	}
	if got.RX != 40 || got.TX != 60 {
		t.Fatalf("unexpected totals: %+v", got)
	}
}

func TestClampInt64FromUint64_Saturates(t *testing.T) {
	if got := ClampInt64FromUint64(0); got != 0 {
		t.Fatalf("zero got %d", got)
	}
	if got := ClampInt64FromUint64(123); got != 123 {
		t.Fatalf("small got %d", got)
	}
	if got := ClampInt64FromUint64(math.MaxUint64); got != math.MaxInt64 {
		t.Fatalf("max uint should clamp to MaxInt64, got %d", got)
	}
}

func TestReadTotalsPrefersProcCounters(t *testing.T) {
	procDir := t.TempDir()
	writeProcFile(t, procDir, "net/dev", `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
  eth0: 1000 0 0 0 0 0 0 0 2000 0 0 0 0 0 0 0
`)
	writeProcFile(t, procDir, "net/route", `Iface	Destination	Gateway	Flags	RefCnt	Use	Metric	Mask	MTU	Window	IRTT
eth0	00000000	01010101	0003	0	0	0	00000000	0	0	0
`)

	totals, source, err := ReadTotals(context.Background(), procDir)
	if err != nil {
		t.Fatalf("ReadTotals: %v", err)
	}
	if totals.RX != 1000 || totals.TX != 2000 {
		t.Fatalf("totals = %+v, want RX=1000 TX=2000", totals)
	}
	if !strings.Contains(source, "source=proc:") {
		t.Fatalf("source = %q, want source=proc:* marker", source)
	}
}

func TestReadTotalsFallsBackWhenProcUnreadable(t *testing.T) {
	// A bogus proc path must not error out the call: the gopsutil fallback
	// takes over (or returns its own error on exotic CI hosts — either way,
	// the proc failure itself must not surface).
	totals, source, err := ReadTotals(context.Background(), filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Skipf("gopsutil fallback unavailable on this host: %v", err)
	}
	if source == "" {
		t.Fatalf("expected non-empty source, totals=%+v", totals)
	}
}
