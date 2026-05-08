package hostnet

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// NetTotals holds cumulative host-level RX/TX byte counters.
type NetTotals struct {
	RX int64
	TX int64
}

// ReadFromProc reads aggregated host RX/TX totals from procPath (e.g. "/host/proc").
// It sums non-virtual host interfaces and filters loopback/container/tunnel
// interfaces so node traffic matches the visible host NIC counters.
// It prefers /proc/1/net because /proc/net is a self/net symlink and resolves to
// the container network namespace when host /proc is bind-mounted into a non-hostnet
// container.
func ReadFromProc(procPath string) (NetTotals, error) {
	procPath = strings.TrimSpace(procPath)
	if procPath == "" {
		return NetTotals{}, fmt.Errorf("empty proc path")
	}
	procPath = filepath.Clean(procPath)
	totals, _, err := ReadFromProcWithSource(procPath)
	return totals, err
}

// ReadFromProcWithSource is like ReadFromProc and also returns a stable counter
// source identifier. The source changes when the selected proc net namespace changes,
// allowing month accumulators to rebase instead of adding unrelated raw counters.
func ReadFromProcWithSource(procPath string) (NetTotals, string, error) {
	procPath = strings.TrimSpace(procPath)
	if procPath == "" {
		return NetTotals{}, "", fmt.Errorf("empty proc path")
	}
	procPath = filepath.Clean(procPath)

	candidates := []struct {
		netDir string
		source string
	}{
		{netDir: filepath.Join(procPath, "1", "net"), source: "proc:" + filepath.Join(procPath, "1", "net")},
		{netDir: filepath.Join(procPath, "net"), source: "proc:" + procPath},
	}
	var errs []string
	for _, candidate := range candidates {
		byIface, err := ParseProcNetDev(filepath.Join(candidate.netDir, "dev"))
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", candidate.netDir, err))
			continue
		}
		if len(byIface) == 0 {
			errs = append(errs, fmt.Sprintf("%s: no interfaces in proc net dev", candidate.netDir))
			continue
		}

		if totals, names, ok := SumPreferredIfaceTotalsWithNames(byIface); ok {
			return totals, CounterSource(candidate.source, names), nil
		}
		errs = append(errs, fmt.Sprintf("%s: no usable interfaces in proc net dev", candidate.netDir))
	}
	return NetTotals{}, "", fmt.Errorf("no usable interfaces in proc path %s (%s)", procPath, strings.Join(errs, "; "))
}

// ParseProcNetDev parses the contents of /proc/net/dev at path.
func ParseProcNetDev(path string) (map[string]NetTotals, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make(map[string]NetTotals)
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if lineNo <= 2 {
			continue
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		fields := strings.Fields(parts[1])
		if len(fields) < 9 {
			continue
		}
		rx, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse rx bytes for %s: %w", iface, err)
		}
		tx, err := strconv.ParseInt(fields[8], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse tx bytes for %s: %w", iface, err)
		}
		out[iface] = NetTotals{RX: rx, TX: tx}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ParseDefaultRouteIfaces parses /proc/net/route at path and returns the iface names
// for default (destination=00000000) rows, preserving order and dropping duplicates.
func ParseDefaultRouteIfaces(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(b), "\n")
	out := make([]string, 0, 2)
	seen := make(map[string]struct{})
	for i, line := range lines {
		if i == 0 {
			continue
		}
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		if fields[1] != "00000000" {
			continue
		}
		iface := strings.TrimSpace(fields[0])
		if iface == "" {
			continue
		}
		if _, ok := seen[iface]; ok {
			continue
		}
		seen[iface] = struct{}{}
		out = append(out, iface)
	}
	return out
}

// PreferredIfaces returns interface names from byIface that are not
// loopback/container/bridge/tunnel/virtual interfaces, sorted alphabetically.
func PreferredIfaces(byIface map[string]NetTotals) []string {
	names := make([]string, 0, len(byIface))
	for name := range byIface {
		if !isPreferredIface(name) {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func isPreferredIface(name string) bool {
	name = strings.TrimSpace(name)
	lower := strings.ToLower(name)
	switch {
	case lower == "", lower == "lo":
		return false
	case strings.Contains(lower, "docker"):
		return false
	case strings.HasPrefix(lower, "br"):
		return false
	case strings.Contains(lower, "veth"):
		return false
	case strings.Contains(lower, "virbr"):
		return false
	case strings.HasPrefix(lower, "podman"):
		return false
	case strings.HasPrefix(lower, "flannel"):
		return false
	case strings.HasPrefix(lower, "ifb"):
		return false
	case strings.HasPrefix(lower, "fwbr"):
		return false
	case strings.HasPrefix(lower, "fwpr"):
		return false
	case strings.Contains(lower, "tun"):
		return false
	case strings.HasPrefix(lower, "tap"):
		return false
	case strings.Contains(lower, "vmbr"):
		return false
	case strings.Contains(lower, "vnet"):
		return false
	case strings.Contains(lower, "kube"):
		return false
	case strings.HasPrefix(lower, "cni"):
		return false
	default:
		return true
	}
}

// SumPreferredIfaceTotals sums all preferred interface counters in byIface.
func SumPreferredIfaceTotals(byIface map[string]NetTotals) (NetTotals, bool) {
	totals, _, ok := SumPreferredIfaceTotalsWithNames(byIface)
	return totals, ok
}

// SumPreferredIfaceTotalsWithNames sums preferred interface counters and returns
// the sorted interface set used to build stable counter-source identifiers.
func SumPreferredIfaceTotalsWithNames(byIface map[string]NetTotals) (NetTotals, []string, bool) {
	names := PreferredIfaces(byIface)
	totals, ok := SumIfaceTotals(byIface, names)
	return totals, names, ok
}

// CounterSource returns a source id that changes when the preferred interface
// set changes. The short hash appears first so downstream length clamps still
// preserve the rebase signal.
func CounterSource(base string, names []string) string {
	base = strings.TrimSpace(base)
	copied := slices.Clone(names)
	slices.Sort(copied)
	joined := strings.Join(copied, ",")
	sum := sha1.Sum([]byte(joined))
	digest := hex.EncodeToString(sum[:8])
	if base == "" {
		base = "unknown"
	}
	return fmt.Sprintf("ifaces=%s;source=%s;names=%s", digest, base, joined)
}

// SumIfaceTotals sums the RX/TX for all names that have matching entries in byIface.
// The ok return value is false when no listed name had an entry.
func SumIfaceTotals(byIface map[string]NetTotals, names []string) (NetTotals, bool) {
	if len(names) == 0 {
		return NetTotals{}, false
	}
	var (
		out   NetTotals
		found bool
	)
	for _, name := range names {
		totals, ok := byIface[name]
		if !ok {
			continue
		}
		out.RX += totals.RX
		out.TX += totals.TX
		found = true
	}
	return out, found
}

// ClampInt64FromUint64 converts a uint64 counter into a non-negative int64,
// saturating at math.MaxInt64 instead of overflowing.
func ClampInt64FromUint64(v uint64) int64 {
	if v > uint64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(v)
}
