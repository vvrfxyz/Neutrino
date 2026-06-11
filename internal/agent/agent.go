package agent

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"neutrino/internal/xrayapi"
)

type Agent struct {
	cfg            Config
	xray           xrayRuntimeClient
	panel          *PanelClient
	panelMu        sync.RWMutex
	state          *StateStore
	queue          *DiskQueue
	sampler        *AccessSampler
	xrayConfigPath string
	xrayTestArgs   []string
	xrayReloadArgs []string
	xrayUsersMu    sync.Mutex

	realityPath string
	reality     *RealityConfig

	startedAt time.Time
	mu        sync.RWMutex

	// reloadPendingMem mirrors State.XrayReloadPending when no StateStore is
	// attached (unit tests build Agent structs directly).
	reloadPendingMem bool

	// usageRejections counts consecutive panel rejections per queued batch
	// path; reaching the quarantine threshold moves the batch aside.
	usageRejections map[string]usageRejectionState

	lastNetInTotal     int64
	lastNetOutTotal    int64
	lastNetAt          time.Time
	hasLastNet         bool
	lastDiskReadBytes  int64
	lastDiskWriteBytes int64
	lastDiskAt         time.Time
	hasLastDisk        bool

	lastIfaceTotals  map[string]ifaceTotals
	lastIfaceAt      time.Time
	lastDiskDevices  map[string]diskDeviceTotals
	lastDiskDeviceAt time.Time
}

type StatEntry struct {
	Uplink   int64 `json:"uplink"`
	Downlink int64 `json:"downlink"`
}

type xrayRuntimeClient interface {
	userSyncApplier
	PullUserTraffic(ctx context.Context, email string) (uplink, downlink int64, err error)
	PullOnlineIPs(ctx context.Context, email string) ([]xrayapi.OnlineIP, error)
}

func New(cfg Config) (*Agent, error) {
	if cfg.NodeID <= 0 {
		return nil, fmt.Errorf("NODE_ID is required")
	}
	if strings.TrimSpace(cfg.PanelMTLSURL) == "" {
		return nil, fmt.Errorf("PANEL_MTLS_URL is required")
	}
	if strings.TrimSpace(cfg.PanelMTLSCACertPath) == "" || strings.TrimSpace(cfg.PanelMTLSClientCertPath) == "" || strings.TrimSpace(cfg.PanelMTLSClientKeyPath) == "" {
		return nil, fmt.Errorf("panel mTLS client paths are required")
	}

	st := NewStateStore(cfg.StatePath)
	if err := st.Load(); err != nil {
		// A corrupt or unreadable state file must stop the agent: continuing
		// with zeroed AckedStats/Access offsets would re-report usage that the
		// panel cannot fully dedupe (stats event ids change once the epoch is
		// recomputed). Operators must repair or explicitly remove the file.
		return nil, fmt.Errorf("load state %s failed (repair or remove the file to reset usage ack state): %w", cfg.StatePath, err)
	}

	// Auto-enroll if cert/key/ca are missing.
	if err := ensureEnrolled(cfg); err != nil {
		return nil, err
	}

	panel, err := NewPanelClient(cfg.PanelMTLSURL, cfg.NodeID, cfg.PanelMTLSCACertPath, cfg.PanelMTLSClientCertPath, cfg.PanelMTLSClientKeyPath)
	if err != nil {
		return nil, err
	}

	xray := xrayapi.NewRaw(cfg.XrayAPIAddr, cfg.XrayInboundTag, cfg.XrayFlow)
	q := NewDiskQueue(cfg.QueueDir, cfg.QueueMaxBytes)

	testArgs, err := parseArgsJSON(cfg.XrayTestArgsJSON)
	if err != nil {
		return nil, fmt.Errorf("invalid XRAY_TEST_ARGS_JSON: %w", err)
	}
	reloadArgs, err := parseArgsJSON(cfg.XrayReloadArgsJSON)
	if err != nil {
		return nil, fmt.Errorf("invalid XRAY_RELOAD_ARGS_JSON: %w", err)
	}
	if strings.TrimSpace(cfg.XrayConfigPath) == "" {
		// Match upstream Xray container default confdir (/usr/local/etc/xray).
		cfg.XrayConfigPath = "/usr/local/etc/xray/config.json"
	}
	if !filepath.IsAbs(cfg.XrayConfigPath) {
		return nil, fmt.Errorf("XRAY_CONFIG_PATH must be an absolute path")
	}

	a := &Agent{
		cfg:            cfg,
		xray:           xray,
		state:          st,
		queue:          q,
		sampler:        NewAccessSampler(cfg.AccessZeroByteMaxPerMinPerUser, cfg.AccessZeroByteMaxPerMinPerUserDest),
		xrayConfigPath: filepath.Clean(cfg.XrayConfigPath),
		xrayTestArgs:   testArgs,
		xrayReloadArgs: reloadArgs,
		realityPath:    filepath.Join(filepath.Dir(cfg.StatePath), "reality.json"),
		startedAt:      time.Now().UTC(),
	}
	a.setPanelClient(panel)

	// Ensure REALITY key material exists locally (private key stays on node; panel only receives public params).
	if rc, err := ensureRealityConfig(a.realityPath); err == nil {
		a.reality = &rc
		// Best-effort: populate report payload used by Heartbeat.
		if p := a.buildNodeReportPayload(); p != nil {
			panel.SetReportPayload(*p)
		}
	} else {
		log.Printf("warn: ensure reality config failed: %v", err)
	}

	// Best-effort bootstrap: if xray config is missing, render a minimal config from the embedded template
	// and restart xray. This avoids a deadlock where xray can't start without config but agent depends on it.
	a.bootstrapXrayConfigIfMissing(context.Background())

	return a, nil
}

func (a *Agent) panelClient() *PanelClient {
	a.panelMu.RLock()
	defer a.panelMu.RUnlock()
	return a.panel
}

func (a *Agent) setPanelClient(p *PanelClient) {
	// Keep report payload in sync across panel client reloads (cert renew rotates TLS material).
	if p != nil {
		if rp := a.buildNodeReportPayload(); rp != nil {
			p.SetReportPayload(*rp)
		}
	}
	a.panelMu.Lock()
	a.panel = p
	a.panelMu.Unlock()
}

func (a *Agent) Run(ctx context.Context) error {
	if strings.TrimSpace(a.cfg.AgentHTTPAddr) != "" {
		go a.startHealthServer(ctx)
	}
	log.Printf("[agent] access-log timezone = %s (AGENT_ACCESS_LOG_TZ=%q)", a.cfg.AccessLogLocation().String(), a.cfg.AccessLogTZ)
	log.Printf("agent: monthly-net location=%s", a.cfg.MonthLocation().String())
	go a.startCertRenewer(ctx)
	go a.startXrayRuntimeGuard(ctx)
	go a.startJobRunner(ctx)
	go a.startUsageLoop(ctx)
	go a.startHeartbeat(ctx)
	<-ctx.Done()
	return nil
}

func (a *Agent) startHealthServer(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: a.cfg.AgentHTTPAddr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("agent health listening on %s", a.cfg.AgentHTTPAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("health server error: %v", err)
	}
}
