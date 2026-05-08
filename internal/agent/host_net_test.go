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
			HostProcPath:         procDir,
			StatePath:            statePath,
			StaticFactsReportSec: 1800,
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

func TestStaticFacts_UsesHostProcPathForOSFacts(t *testing.T) {
	t.Setenv("XRAY_VERSION", "test-xray")
	procDir := t.TempDir()
	writeProcFile(t, procDir, "sys/kernel/hostname", "host-node-01\n")
	writeProcFile(t, procDir, "sys/kernel/ostype", "Linux\n")
	writeProcFile(t, procDir, "sys/kernel/osrelease", "6.8.0-64-generic\n")
	writeProcFile(t, procDir, "1/root/etc/os-release", `NAME="Debian GNU/Linux"
VERSION_ID="12"
PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"
`)
	writeProcFile(t, procDir, "1/root/sys/class/dmi/id/sys_vendor", "Amazon EC2\n")
	writeProcFile(t, procDir, "1/root/sys/class/dmi/id/product_name", "r7i.large\n")

	ag := &Agent{cfg: Config{HostProcPath: procDir}}
	facts := ag.staticFacts()
	if facts.OSName != "Debian GNU/Linux" || facts.OSVersion != "12" {
		t.Fatalf("expected host os-release facts, got os=%q version=%q", facts.OSName, facts.OSVersion)
	}
	if facts.Hostname != "host-node-01" {
		t.Fatalf("expected host hostname, got %q", facts.Hostname)
	}
	if facts.Kernel != "linux" || facts.KernelVersion != "6.8.0-64-generic" {
		t.Fatalf("expected host kernel facts, got kernel=%q version=%q", facts.Kernel, facts.KernelVersion)
	}
	if facts.Virtualization != "amazon-ec2" {
		t.Fatalf("expected host virtualization label, got %q", facts.Virtualization)
	}
}

func TestStaticFacts_UsesMountedHostOSRelease(t *testing.T) {
	t.Setenv("XRAY_VERSION", "test-xray")
	hostRoot := t.TempDir()
	procDir := filepath.Join(hostRoot, "proc")
	writeProcFile(t, procDir, "sys/kernel/hostname", "host-node-02\n")
	writeProcFile(t, procDir, "sys/kernel/ostype", "Linux\n")
	writeProcFile(t, procDir, "sys/kernel/osrelease", "6.8.0-65-generic\n")
	writeProcFile(t, hostRoot, "etc/os-release", `NAME="Ubuntu"
VERSION_ID="24.04"
`)
	writeProcFile(t, hostRoot, "sys/class/dmi/id/sys_vendor", "Red Hat\n")
	writeProcFile(t, hostRoot, "sys/class/dmi/id/product_name", "KVM\n")

	ag := &Agent{cfg: Config{HostProcPath: procDir}}
	facts := ag.staticFacts()
	if facts.OSName != "Ubuntu" || facts.OSVersion != "24.04" {
		t.Fatalf("expected mounted host os-release facts, got os=%q version=%q", facts.OSName, facts.OSVersion)
	}
	if facts.Virtualization != "kvm" {
		t.Fatalf("expected mounted host dmi virtualization, got %q", facts.Virtualization)
	}
}

func TestParseXrayVersionOutput(t *testing.T) {
	got := parseXrayVersionOutput("Xray 26.2.6 (Xray, Penetrates Everything.) Custom (go1.25.7 linux/amd64)\n")
	if got != "26.2.6" {
		t.Fatalf("expected parsed xray version, got %q", got)
	}
}

func TestImageTag(t *testing.T) {
	if got := imageTag("ghcr.io/xtls/xray-core:26.2.6"); got != "26.2.6" {
		t.Fatalf("expected image tag, got %q", got)
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
