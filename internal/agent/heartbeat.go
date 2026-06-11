package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"neutrino/internal/hostnet"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	gnet "github.com/shirou/gopsutil/v3/net"
	"github.com/shirou/gopsutil/v3/process"
)

type ifaceTotals struct {
	RXBytes int64
	TXBytes int64
}

type diskDeviceTotals struct {
	ReadBytes  int64
	WriteBytes int64
}

func (a *Agent) startHeartbeat(ctx context.Context) {
	interval := time.Duration(a.cfg.RuntimeReportSec) * time.Second
	if interval < time.Second {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	staticInterval := time.Duration(a.cfg.StaticFactsReportSec) * time.Second
	if staticInterval < 5*time.Minute {
		staticInterval = 30 * time.Minute
	}
	lastStaticAt := time.Time{}
	// Online-IP collection walks every active user through the xray gRPC API,
	// so it runs on its own (slower) cadence than the heartbeat itself.
	onlineInterval := time.Duration(a.cfg.OnlineSnapshotSec) * time.Second
	if onlineInterval < interval {
		onlineInterval = interval
	}
	lastOnlineAt := time.Time{}
	sendOnce := func() {
		pc := a.panelClient()
		if pc == nil {
			return
		}
		reportedAt := time.Now().UTC()
		rp := a.buildNodeReportPayload()
		if rp == nil {
			rp = &NodeReportPayload{}
		}
		rp.ReportedAt = reportedAt.Format(time.RFC3339)
		rp.Metrics = a.heartbeatMetrics(reportedAt)
		if lastOnlineAt.IsZero() || reportedAt.Sub(lastOnlineAt) >= onlineInterval {
			if snapshot, err := a.buildOnlineSnapshot(ctx, reportedAt); err != nil {
				log.Printf("online snapshot collection failed: %v", err)
			} else {
				rp.OnlineSnapshot = snapshot
				lastOnlineAt = reportedAt
			}
		}
		if lastStaticAt.IsZero() || reportedAt.Sub(lastStaticAt) >= staticInterval {
			rp.StaticFacts = a.staticFacts()
			lastStaticAt = reportedAt
		}
		pc.SetReportPayload(*rp)
		if err := pc.Heartbeat(ctx); err != nil {
			log.Printf("heartbeat failed: %v", err)
		}
	}
	sendOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendOnce()
		}
	}
}

func (a *Agent) buildOnlineSnapshot(ctx context.Context, observedAt time.Time) (*OnlineSnapshot, error) {
	if a == nil || a.state == nil || a.xray == nil {
		return nil, nil
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	} else {
		observedAt = observedAt.UTC()
	}

	users := effectiveSyncedUsers(a.state.Snapshot())
	if len(users) == 0 {
		return &OnlineSnapshot{
			ObservedAt: observedAt.Format(time.RFC3339),
			Items:      []OnlineSnapshotItem{},
		}, nil
	}

	items := make([]OnlineSnapshotItem, 0, len(users))
	for _, u := range users {
		email := strings.TrimSpace(u.Email)
		if u.UserID <= 0 || email == "" || u.Status != "active" {
			continue
		}
		online, err := a.xray.PullOnlineIPs(ctx, email)
		if err != nil {
			return nil, fmt.Errorf("pull online ips user_id=%d email=%s: %w", u.UserID, email, err)
		}
		for _, ip := range online {
			lastSeenAt := ip.LastSeenAt
			if lastSeenAt.IsZero() {
				lastSeenAt = observedAt
			}
			items = append(items, OnlineSnapshotItem{
				UserID:     u.UserID,
				Email:      email,
				ClientIP:   strings.TrimSpace(ip.IP),
				LastSeenAt: lastSeenAt.UTC().Format(time.RFC3339),
			})
		}
	}
	return &OnlineSnapshot{
		ObservedAt: observedAt.Format(time.RFC3339),
		Items:      items,
	}, nil
}

func (a *Agent) heartbeatMetrics(reportedAt time.Time) *NodeReportMetrics {
	if a == nil {
		return nil
	}
	if reportedAt.IsZero() {
		reportedAt = time.Now().UTC()
	}
	m := &NodeReportMetrics{
		Goroutines:        runtime.NumGoroutine(),
		AgentVersion:      agentVersion(),
		XrayVersion:       xrayVersionMarker(),
		XrayConfigVersion: a.xrayConfigVersionMarker(),
		Details:           map[string]any{},
	}

	// Best-effort host/container metrics for node monitoring.
	if cpuList, err := cpu.PercentWithContext(context.Background(), 0, false); err == nil && len(cpuList) > 0 {
		v := cpuList[0]
		if v < 0 {
			v = 0
		}
		if v > 1000 {
			v = 1000
		}
		m.CPUPercent = v
	}
	if perCore, err := cpu.PercentWithContext(context.Background(), 0, true); err == nil && len(perCore) > 0 {
		cores := make([]map[string]any, 0, len(perCore))
		for i, v := range perCore {
			if v < 0 {
				v = 0
			}
			if v > 1000 {
				v = 1000
			}
			cores = append(cores, map[string]any{"index": i, "usage_percent": v})
		}
		m.Details["cpu_cores"] = cores
	}
	if avg, err := load.AvgWithContext(context.Background()); err == nil && avg != nil {
		if avg.Load1 > 0 {
			m.Load1 = avg.Load1
		}
		if avg.Load5 > 0 {
			m.Load5 = avg.Load5
		}
		if avg.Load15 > 0 {
			m.Load15 = avg.Load15
		}
	}
	if vm, err := mem.VirtualMemoryWithContext(context.Background()); err == nil && vm != nil {
		// Match panel host monitor: report used bytes.
		m.MemoryBytes = hostnet.ClampInt64FromUint64(vm.Used)
		m.MemoryTotalBytes = hostnet.ClampInt64FromUint64(vm.Total)
		m.MemoryAvailableBytes = hostnet.ClampInt64FromUint64(vm.Available)
	}
	if sm, err := mem.SwapMemoryWithContext(context.Background()); err == nil && sm != nil {
		m.SwapUsedBytes = hostnet.ClampInt64FromUint64(sm.Used)
		m.SwapTotalBytes = hostnet.ClampInt64FromUint64(sm.Total)
	}
	if inTotal, outTotal, netSource, err := a.readNetTotals(context.Background()); err == nil {
		if a.hasLastNet && !a.lastNetAt.IsZero() {
			dt := reportedAt.Sub(a.lastNetAt).Seconds()
			if dt > 0 {
				inDelta := inTotal - a.lastNetInTotal
				outDelta := outTotal - a.lastNetOutTotal
				if inDelta >= 0 {
					m.InboundBPS = float64(inDelta) / dt
				}
				if outDelta >= 0 {
					m.OutboundBPS = float64(outDelta) / dt
				}
			}
		}
		a.lastNetInTotal, a.lastNetOutTotal, a.lastNetAt, a.hasLastNet = inTotal, outTotal, reportedAt, true
		monthLoc := a.cfg.MonthLocation()
		m.MonthKey = localMonthKey(reportedAt, monthLoc)
		m.MonthTimezone = localTimezoneName(reportedAt, monthLoc)
		m.NetRXTotal = inTotal
		m.NetTXTotal = outTotal
		m.NetSource = netSource
	}
	a.addInterfaceDetails(context.Background(), reportedAt, m)
	if du, err := disk.UsageWithContext(context.Background(), a.diskUsagePath()); err == nil && du != nil {
		m.DiskTotalBytes = hostnet.ClampInt64FromUint64(du.Total)
		m.DiskUsedBytes = hostnet.ClampInt64FromUint64(du.Used)
		m.DiskFreeBytes = hostnet.ClampInt64FromUint64(du.Free)
		if du.UsedPercent >= 0 {
			m.DiskUsedPercent = du.UsedPercent
		}
	}
	if counters, err := disk.IOCountersWithContext(context.Background()); err == nil {
		var readBytes, writeBytes int64
		deviceDetails := make([]map[string]any, 0, len(counters))
		nextDevices := make(map[string]diskDeviceTotals, len(counters))
		deviceDt := reportedAt.Sub(a.lastDiskDeviceAt).Seconds()
		for _, c := range counters {
			read := hostnet.ClampInt64FromUint64(c.ReadBytes)
			write := hostnet.ClampInt64FromUint64(c.WriteBytes)
			readBytes += read
			writeBytes += write
			detail := map[string]any{
				"name":        c.Name,
				"read_bytes":  read,
				"write_bytes": write,
			}
			if prev, ok := a.lastDiskDevices[c.Name]; ok && deviceDt > 0 {
				if delta := read - prev.ReadBytes; delta >= 0 {
					detail["read_bps"] = float64(delta) / deviceDt
				}
				if delta := write - prev.WriteBytes; delta >= 0 {
					detail["write_bps"] = float64(delta) / deviceDt
				}
			}
			deviceDetails = append(deviceDetails, detail)
			nextDevices[c.Name] = diskDeviceTotals{ReadBytes: read, WriteBytes: write}
		}
		if a.hasLastDisk && !a.lastDiskAt.IsZero() {
			dt := reportedAt.Sub(a.lastDiskAt).Seconds()
			if dt > 0 {
				if delta := readBytes - a.lastDiskReadBytes; delta >= 0 {
					m.DiskReadBPS = float64(delta) / dt
				}
				if delta := writeBytes - a.lastDiskWriteBytes; delta >= 0 {
					m.DiskWriteBPS = float64(delta) / dt
				}
			}
		}
		a.lastDiskReadBytes, a.lastDiskWriteBytes, a.lastDiskAt, a.hasLastDisk = readBytes, writeBytes, reportedAt, true
		a.lastDiskDevices, a.lastDiskDeviceAt = nextDevices, reportedAt
		if len(deviceDetails) > 0 {
			m.Details["disk_io_devices"] = deviceDetails
		}
	}
	a.addDiskUsageDetails(context.Background(), m)
	if info, err := host.InfoWithContext(context.Background()); err == nil && info != nil {
		if info.Uptime > 0 {
			m.SystemUptimeSec = int64(info.Uptime)
		}
		if info.BootTime > 0 {
			m.BootTime = time.Unix(int64(info.BootTime), 0).UTC().Format(time.RFC3339)
		}
	}
	if conns, err := gnet.ConnectionsWithContext(context.Background(), "all"); err == nil {
		for _, c := range conns {
			switch int(c.Type) {
			case syscall.SOCK_STREAM:
				m.TCPConnections++
			case syscall.SOCK_DGRAM:
				m.UDPConnections++
			}
		}
	}
	if pids, err := process.PidsWithContext(context.Background()); err == nil {
		m.ProcessCount = len(pids)
	}

	if !a.startedAt.IsZero() {
		up := time.Since(a.startedAt).Seconds()
		if up > 0 {
			m.UptimeSec = int64(up)
			m.AgentUptimeSec = int64(up)
		}
	}
	if a.queue != nil {
		m.QueueBytes = a.queue.ApproxBytes()
		m.QueueBatches = a.queue.ApproxBatches()
		m.QuarantinedBatches = a.queue.QuarantinedBatches()
	}
	if len(m.Details) == 0 {
		m.Details = nil
	}
	return m
}

func (a *Agent) addInterfaceDetails(ctx context.Context, reportedAt time.Time, m *NodeReportMetrics) {
	if a == nil || m == nil || m.Details == nil {
		return
	}
	counters, err := gnet.IOCountersWithContext(ctx, true)
	if err != nil || len(counters) == 0 {
		return
	}
	if a.lastIfaceTotals == nil {
		a.lastIfaceTotals = map[string]ifaceTotals{}
	}
	next := make(map[string]ifaceTotals, len(counters))
	dt := reportedAt.Sub(a.lastIfaceAt).Seconds()
	items := make([]map[string]any, 0, len(counters))
	for _, c := range counters {
		rx := hostnet.ClampInt64FromUint64(c.BytesRecv)
		tx := hostnet.ClampInt64FromUint64(c.BytesSent)
		item := map[string]any{
			"name":     c.Name,
			"rx_bytes": rx,
			"tx_bytes": tx,
		}
		if prev, ok := a.lastIfaceTotals[c.Name]; ok && dt > 0 {
			if delta := rx - prev.RXBytes; delta >= 0 {
				item["rx_bps"] = float64(delta) / dt
			}
			if delta := tx - prev.TXBytes; delta >= 0 {
				item["tx_bps"] = float64(delta) / dt
			}
		}
		next[c.Name] = ifaceTotals{RXBytes: rx, TXBytes: tx}
		items = append(items, item)
	}
	a.lastIfaceTotals, a.lastIfaceAt = next, reportedAt
	m.Details["interfaces"] = items
}

func (a *Agent) addDiskUsageDetails(ctx context.Context, m *NodeReportMetrics) {
	if a == nil || m == nil || m.Details == nil {
		return
	}
	partitions, err := disk.PartitionsWithContext(ctx, false)
	if err != nil || len(partitions) == 0 {
		return
	}
	items := make([]map[string]any, 0, len(partitions))
	for _, p := range partitions {
		usage, err := disk.UsageWithContext(ctx, p.Mountpoint)
		if err != nil || usage == nil {
			continue
		}
		items = append(items, map[string]any{
			"device":       p.Device,
			"mountpoint":   p.Mountpoint,
			"fstype":       p.Fstype,
			"total_bytes":  hostnet.ClampInt64FromUint64(usage.Total),
			"used_bytes":   hostnet.ClampInt64FromUint64(usage.Used),
			"free_bytes":   hostnet.ClampInt64FromUint64(usage.Free),
			"used_percent": usage.UsedPercent,
		})
	}
	if len(items) > 0 {
		m.Details["disks"] = items
	}
}

func (a *Agent) staticFacts() *NodeStaticFacts {
	facts := &NodeStaticFacts{
		Arch:         runtime.GOARCH,
		AgentVersion: agentVersion(),
		FactsJSON:    map[string]any{},
	}
	if h, err := os.Hostname(); err == nil {
		facts.Hostname = h
	}
	if info, err := host.InfoWithContext(context.Background()); err == nil && info != nil {
		facts.OSName = info.Platform
		facts.OSVersion = strings.TrimSpace(info.PlatformVersion)
		facts.Kernel = info.OS
		facts.KernelVersion = strings.TrimSpace(info.KernelVersion)
		facts.Virtualization = strings.TrimSpace(info.VirtualizationSystem)
		facts.FactsJSON["host_id"] = info.HostID
		facts.FactsJSON["virtualization_role"] = info.VirtualizationRole
	}
	if infos, err := cpu.InfoWithContext(context.Background()); err == nil && len(infos) > 0 {
		facts.CPUModel = strings.TrimSpace(infos[0].ModelName)
		if cores, err := cpu.CountsWithContext(context.Background(), false); err == nil && cores > 0 {
			facts.CPUPhysicalCores = cores
		}
		if cores, err := cpu.CountsWithContext(context.Background(), true); err == nil && cores > 0 {
			facts.CPULogicalCores = cores
		}
	}
	facts.XrayVersion = detectXrayVersion()
	a.applyHostProcStaticFacts(facts)
	return facts
}

func (a *Agent) applyHostProcStaticFacts(facts *NodeStaticFacts) {
	if a == nil || facts == nil {
		return
	}
	procPath := strings.TrimSpace(a.cfg.HostProcPath)
	if procPath == "" {
		return
	}
	if facts.FactsJSON == nil {
		facts.FactsJSON = map[string]any{}
	}
	if hostname := readTrimmedFile(filepath.Join(procPath, "sys/kernel/hostname")); hostname != "" {
		facts.Hostname = hostname
	}
	if kernel := readTrimmedFile(filepath.Join(procPath, "sys/kernel/ostype")); kernel != "" {
		facts.Kernel = strings.ToLower(kernel)
	}
	if kernelVersion := readTrimmedFile(filepath.Join(procPath, "sys/kernel/osrelease")); kernelVersion != "" {
		facts.KernelVersion = kernelVersion
	}
	if osName, osVersion := readHostOSRelease(procPath); osName != "" {
		facts.OSName = osName
		facts.OSVersion = osVersion
	}
	if virt := readHostVirtualizationLabel(procPath); virt != "" {
		facts.Virtualization = virt
	} else if strings.EqualFold(strings.TrimSpace(facts.Virtualization), "docker") {
		facts.Virtualization = ""
	}
}

func readHostOSRelease(procPath string) (string, string) {
	paths := make([]string, 0, 4)
	if filepath.Base(procPath) == "proc" {
		hostRoot := filepath.Dir(procPath)
		paths = append(paths,
			filepath.Join(hostRoot, "etc/os-release"),
			filepath.Join(hostRoot, "usr/lib/os-release"),
		)
	}
	paths = append(paths,
		filepath.Join(procPath, "1/root/etc/os-release"),
		filepath.Join(procPath, "1/root/usr/lib/os-release"),
	)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		fields := parseOSRelease(string(raw))
		name := firstNonEmpty(fields["NAME"], fields["ID"], fields["PRETTY_NAME"])
		version := firstNonEmpty(fields["VERSION_ID"], fields["VERSION"])
		if name == fields["PRETTY_NAME"] {
			version = ""
		}
		return name, version
	}
	return "", ""
}

func parseOSRelease(raw string) map[string]string {
	out := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') {
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			} else {
				value = strings.Trim(value, `"'`)
			}
		}
		if key != "" {
			out[key] = value
		}
	}
	return out
}

func readHostVirtualizationLabel(procPath string) string {
	for _, root := range hostDMIPaths(procPath) {
		vendor := readTrimmedFile(filepath.Join(root, "sys_vendor"))
		product := readTrimmedFile(filepath.Join(root, "product_name"))
		if label := virtualizationLabel(vendor, product); label != "" {
			return label
		}
	}
	return ""
}

func hostDMIPaths(procPath string) []string {
	paths := make([]string, 0, 3)
	if filepath.Base(procPath) == "proc" {
		paths = append(paths, filepath.Join(filepath.Dir(procPath), "sys/class/dmi/id"))
	}
	paths = append(paths,
		filepath.Join(procPath, "1/root/sys/class/dmi/id"),
		"/sys/class/dmi/id",
	)
	return paths
}

func virtualizationLabel(vendor, product string) string {
	raw := strings.ToLower(strings.TrimSpace(vendor + " " + product))
	switch {
	case raw == "":
		return ""
	case strings.Contains(raw, "kvm"):
		return "kvm"
	case strings.Contains(raw, "vmware"):
		return "vmware"
	case strings.Contains(raw, "virtualbox"):
		return "virtualbox"
	case strings.Contains(raw, "amazon ec2"):
		return "amazon-ec2"
	case strings.Contains(raw, "google"):
		return "gce"
	case strings.Contains(raw, "microsoft") || strings.Contains(raw, "hyper-v"):
		return "hyper-v"
	}
	return firstNonEmpty(product, vendor)
}

func readTrimmedFile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if out := strings.TrimSpace(value); out != "" {
			return out
		}
	}
	return ""
}

func agentVersion() string {
	v := strings.TrimSpace(os.Getenv("NEUTRINO_AGENT_VERSION"))
	if v == "" {
		return "dev"
	}
	return v
}

func xrayVersionMarker() string {
	if v := strings.TrimSpace(os.Getenv("XRAY_VERSION")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("NODE_DEFAULT_XRAY_IMAGE")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("XRAY_IMAGE"))
}

func detectXrayVersion() string {
	if v := xrayVersionMarker(); v != "" {
		return v
	}
	for _, args := range [][]string{
		{"exec", "neutrino-xray", "/usr/local/bin/xray", "version"},
		{"exec", "neutrino-xray", "xray", "version"},
	} {
		out := runDockerCommand(2*time.Second, args...)
		if version := parseXrayVersionOutput(out); version != "" {
			return version
		}
	}
	image := strings.TrimSpace(runDockerCommand(2*time.Second, "inspect", "neutrino-xray", "--format", "{{.Config.Image}}"))
	if tag := imageTag(image); tag != "" {
		return tag
	}
	return image
}

func runDockerCommand(timeout time.Duration, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func parseXrayVersionOutput(raw string) string {
	line := strings.TrimSpace(strings.Split(raw, "\n")[0])
	if line == "" {
		return ""
	}
	matches := regexp.MustCompile(`(?i)^Xray\s+([^\s]+)`).FindStringSubmatch(line)
	if len(matches) == 2 {
		return matches[1]
	}
	return ""
}

func imageTag(image string) string {
	image = strings.TrimSpace(image)
	if image == "" {
		return ""
	}
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon > slash && colon < len(image)-1 {
		return image[colon+1:]
	}
	return ""
}

func (a *Agent) xrayConfigVersionMarker() string {
	if a == nil || strings.TrimSpace(a.xrayConfigPath) == "" {
		return ""
	}
	b, err := os.ReadFile(a.xrayConfigPath)
	if err != nil || len(b) == 0 {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

func (a *Agent) diskUsagePath() string {
	if a == nil {
		return "/"
	}
	p := strings.TrimSpace(a.cfg.StatePath)
	if p != "" && filepath.IsAbs(p) {
		dir := filepath.Clean(filepath.Dir(p))
		if dir != "" {
			return dir
		}
	}
	return "/"
}
