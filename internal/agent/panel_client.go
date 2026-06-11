package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type PanelClient struct {
	baseURL string // mTLS base
	nodeID  int64
	client  *http.Client

	mu     sync.RWMutex
	report NodeReportPayload
}

type UsageEvent struct {
	UserID        int64  `json:"user_id"`
	NodeID        *int64 `json:"node_id,omitempty"`
	Direction     string `json:"direction"`
	Bytes         int64  `json:"bytes"`
	Source        string `json:"source"`
	SourceEventID string `json:"source_event_id"`
	TargetHost    string `json:"target_host,omitempty"`
	TargetIP      string `json:"target_ip,omitempty"`
	TargetPort    int64  `json:"target_port,omitempty"`
	SNI           string `json:"sni,omitempty"`
	Destination   string `json:"destination,omitempty"`
	ClientIP      string `json:"client_ip,omitempty"`
	InboundTag    string `json:"inbound_tag,omitempty"`
	At            string `json:"at,omitempty"`
}

type pushUsageResponse struct {
	Processed int                      `json:"processed"`
	Results   []map[string]interface{} `json:"results"`
}

type NodeJob struct {
	ID             int64  `json:"id"`
	NodeID         int64  `json:"node_id"`
	Kind           string `json:"kind"`
	DesiredVersion string `json:"desired_version"`
	PayloadJSON    string `json:"payload_json"`
	Attempts       int    `json:"attempts"`
	TimeoutSec     *int   `json:"timeout_sec,omitempty"`
	CorrelationID  string `json:"correlation_id,omitempty"`
}

func NewPanelClient(baseURL string, nodeID int64, caPath, certPath, keyPath string) (*PanelClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || nodeID <= 0 {
		return nil, fmt.Errorf("invalid panel client config")
	}
	if strings.TrimSpace(caPath) == "" || strings.TrimSpace(certPath) == "" || strings.TrimSpace(keyPath) == "" {
		return nil, fmt.Errorf("panel mTLS required: set PANEL_MTLS_CA_CERT_PATH, PANEL_MTLS_CLIENT_CERT_PATH, PANEL_MTLS_CLIENT_KEY_PATH")
	}

	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read PANEL_MTLS_CA_CERT_PATH: %w", err)
	}
	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM(caPEM); !ok {
		return nil, fmt.Errorf("invalid panel mTLS CA cert at PANEL_MTLS_CA_CERT_PATH")
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load panel mTLS client cert/key: %w", err)
	}

	return &PanelClient{
		baseURL: baseURL,
		nodeID:  nodeID,
		client: &http.Client{
			Timeout: 35 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					MinVersion:   tls.VersionTLS12,
					RootCAs:      pool,
					Certificates: []tls.Certificate{cert},
				},
				MaxIdleConns:        50,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}, nil
}

// UsageRejectionError reports that the panel received and processed a usage
// batch but rejected it (or returned a response that cannot match the batch).
// Unlike transport errors, these rejections are deterministic per batch
// content, so repeated rejections of the same queued batch indicate a poison
// batch that will never be accepted.
type UsageRejectionError struct {
	msg string
}

func (e *UsageRejectionError) Error() string { return e.msg }

func usageRejectionf(format string, args ...any) error {
	return &UsageRejectionError{msg: fmt.Sprintf(format, args...)}
}

func (c *PanelClient) PushUsage(ctx context.Context, events []UsageEvent) error {
	payload := map[string]any{"events": events}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/usage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("panel returned status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var out pushUsageResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("decode usage response: %w", err)
	}
	if len(out.Results) != len(events) {
		return usageRejectionf("usage response result count %d does not match event count %d", len(out.Results), len(events))
	}
	for i, result := range out.Results {
		if result == nil {
			return usageRejectionf("usage event %d missing result", i)
		}
		if v, ok := result["error"]; ok {
			msg := strings.TrimSpace(fmt.Sprint(v))
			if msg != "" {
				if isPermanentUsageRejection(msg) {
					continue
				}
				if isTransientUsageRejection(msg) {
					// Time-dependent or panel-side failures: retrying the same
					// batch can succeed later, so these must never count
					// toward poison-batch quarantine.
					return fmt.Errorf("panel rejected usage event %d: %s", i, msg)
				}
				return usageRejectionf("panel rejected usage event %d: %s", i, msg)
			}
		}
	}
	return nil
}

// isTransientUsageRejection matches per-event panel errors that are not
// deterministic for the batch content: a future-skewed timestamp becomes valid
// as time passes, and "record failed" covers transient panel-side failures
// (e.g. a busy database). See isPermanentUsageRejection for the agent↔panel
// error-string contract.
func isTransientUsageRejection(msg string) bool {
	switch strings.ToLower(strings.TrimSpace(msg)) {
	case "invalid event timestamp",
		"record failed":
		return true
	default:
		return false
	}
}

func isPermanentUsageRejection(msg string) bool {
	switch strings.ToLower(strings.TrimSpace(msg)) {
	case "source_event_id required",
		"source required",
		"user not found",
		"user not allowed on node",
		"user not active",
		"event timestamp too old":
		return true
	default:
		return false
	}
}

func (c *PanelClient) Heartbeat(ctx context.Context) error {
	url := fmt.Sprintf("%s/api/v1/nodes/%d/report", c.baseURL, c.nodeID)
	// Optional report payload (subscription-facing params).
	// Keep backward compatible: panel accepts empty body.
	body, _ := json.Marshal(c.reportPayload())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("heartbeat returned status %d", resp.StatusCode)
	}
	return nil
}

type NodeReportPayload struct {
	ReportedAt     string             `json:"reported_at,omitempty"`
	Port           *int               `json:"port,omitempty"`
	SNI            string             `json:"sni,omitempty"`
	PublicKey      string             `json:"public_key,omitempty"`
	ShortID        string             `json:"short_id,omitempty"`
	Metrics        *NodeReportMetrics `json:"metrics,omitempty"`
	StaticFacts    *NodeStaticFacts   `json:"static_facts,omitempty"`
	OnlineSnapshot *OnlineSnapshot    `json:"online_snapshot,omitempty"`
}

type OnlineSnapshot struct {
	ObservedAt string               `json:"observed_at"`
	Items      []OnlineSnapshotItem `json:"items"`
}

type OnlineSnapshotItem struct {
	UserID     int64  `json:"user_id"`
	Email      string `json:"email,omitempty"`
	ClientIP   string `json:"client_ip"`
	LastSeenAt string `json:"last_seen_at"`
}

type NodeReportMetrics struct {
	CPUPercent           float64 `json:"cpu_percent,omitempty"`
	Load1                float64 `json:"load1,omitempty"`
	Load5                float64 `json:"load5,omitempty"`
	Load15               float64 `json:"load15,omitempty"`
	MemoryBytes          int64   `json:"memory_bytes,omitempty"`
	MemoryTotalBytes     int64   `json:"memory_total_bytes,omitempty"`
	MemoryAvailableBytes int64   `json:"memory_available_bytes,omitempty"`
	SwapUsedBytes        int64   `json:"swap_used_bytes,omitempty"`
	SwapTotalBytes       int64   `json:"swap_total_bytes,omitempty"`
	InboundBPS           float64 `json:"inbound_bps,omitempty"`
	OutboundBPS          float64 `json:"outbound_bps,omitempty"`
	MonthKey             string  `json:"month_key,omitempty"`
	MonthTimezone        string  `json:"month_timezone,omitempty"`
	NetRXTotal           int64   `json:"net_rx_total_bytes,omitempty"`
	NetTXTotal           int64   `json:"net_tx_total_bytes,omitempty"`
	NetSource            string  `json:"net_counter_source,omitempty"`

	DiskTotalBytes  int64   `json:"disk_total_bytes,omitempty"`
	DiskUsedBytes   int64   `json:"disk_used_bytes,omitempty"`
	DiskFreeBytes   int64   `json:"disk_free_bytes,omitempty"`
	DiskUsedPercent float64 `json:"disk_used_percent,omitempty"`
	DiskReadBPS     float64 `json:"disk_read_bps,omitempty"`
	DiskWriteBPS    float64 `json:"disk_write_bps,omitempty"`
	TCPConnections  int     `json:"tcp_connections,omitempty"`
	UDPConnections  int     `json:"udp_connections,omitempty"`
	ProcessCount    int     `json:"process_count,omitempty"`

	UptimeSec          int64          `json:"uptime_sec,omitempty"`
	SystemUptimeSec    int64          `json:"system_uptime_sec,omitempty"`
	AgentUptimeSec     int64          `json:"agent_uptime_sec,omitempty"`
	BootTime           string         `json:"boot_time,omitempty"`
	QueueBytes         int64          `json:"queue_bytes,omitempty"`
	QueueBatches       int64          `json:"queue_batches,omitempty"`
	QuarantinedBatches int64          `json:"quarantined_batches,omitempty"`
	Goroutines         int            `json:"goroutines,omitempty"`
	AgentVersion       string         `json:"agent_version,omitempty"`
	XrayVersion        string         `json:"xray_version,omitempty"`
	XrayConfigVersion  string         `json:"xray_config_version,omitempty"`
	Details            map[string]any `json:"details,omitempty"`
}

type NodeStaticFacts struct {
	OSName           string         `json:"os_name,omitempty"`
	OSVersion        string         `json:"os_version,omitempty"`
	Kernel           string         `json:"kernel,omitempty"`
	KernelVersion    string         `json:"kernel_version,omitempty"`
	Arch             string         `json:"arch,omitempty"`
	Hostname         string         `json:"hostname,omitempty"`
	Virtualization   string         `json:"virtualization,omitempty"`
	CPUModel         string         `json:"cpu_model,omitempty"`
	CPUPhysicalCores int            `json:"cpu_physical_cores,omitempty"`
	CPULogicalCores  int            `json:"cpu_logical_cores,omitempty"`
	AgentVersion     string         `json:"agent_version,omitempty"`
	XrayVersion      string         `json:"xray_version,omitempty"`
	FactsJSON        map[string]any `json:"facts_json,omitempty"`
}

// reportPayload is filled by Agent via SetReportPayload.
func (c *PanelClient) reportPayload() NodeReportPayload {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.report
}

func (c *PanelClient) SetReportPayload(p NodeReportPayload) {
	c.mu.Lock()
	c.report = p
	c.mu.Unlock()
}

func (c *PanelClient) FetchUsers(ctx context.Context) ([]UserSyncItem, error) {
	url := fmt.Sprintf("%s/api/v1/nodes/%d/agent/users", c.baseURL, c.nodeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fetch users returned status %d", resp.StatusCode)
	}
	var result struct {
		Users []UserSyncItem `json:"users"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Users, nil
}

func (c *PanelClient) ClaimJob(ctx context.Context, waitSec int) (*NodeJob, bool, error) {
	if waitSec < 0 {
		waitSec = 0
	}
	if waitSec > 25 {
		waitSec = 25
	}
	url := fmt.Sprintf("%s/api/v1/nodes/%d/jobs/claim?wait=%d", c.baseURL, c.nodeID, waitSec)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, false, nil
	}
	if resp.StatusCode >= 400 {
		return nil, false, fmt.Errorf("claim returned status %d", resp.StatusCode)
	}
	var out struct {
		Job NodeJob `json:"job"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, false, err
	}
	return &out.Job, true, nil
}

type FinishJobRequest struct {
	Status         string `json:"status"`
	Retryable      bool   `json:"retryable"`
	HTTPStatus     *int   `json:"http_status,omitempty"`
	ResultJSON     any    `json:"result_json,omitempty"`
	Error          string `json:"error,omitempty"`
	AppliedVersion string `json:"applied_version,omitempty"`
	Attempt        int    `json:"attempt"`
}

func (c *PanelClient) FinishJob(ctx context.Context, jobID int64, reqBody FinishJobRequest) error {
	url := fmt.Sprintf("%s/api/v1/nodes/%d/jobs/%d/finish", c.baseURL, c.nodeID, jobID)
	body, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("finish returned status %d body=%s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

type EnrollResponse struct {
	CABundlePEM string `json:"ca_bundle_pem"`
	CertPEM     string `json:"cert_pem"`
	NotAfter    string `json:"not_after"`
	SPKISHA256  string `json:"spki_sha256"`
	CertSHA256  string `json:"cert_sha256"`
}

func Enroll(ctx context.Context, panelURL string, nodeID int64, enrollCode string, csrPEM string) (EnrollResponse, error) {
	panelURL = strings.TrimRight(strings.TrimSpace(panelURL), "/")
	if panelURL == "" || nodeID <= 0 {
		return EnrollResponse{}, fmt.Errorf("invalid enroll config")
	}
	body, _ := json.Marshal(map[string]any{
		"enroll_code": strings.TrimSpace(enrollCode),
		"csr_pem":     csrPEM,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/nodes/%d/enroll", panelURL, nodeID), bytes.NewReader(body))
	if err != nil {
		return EnrollResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return EnrollResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return EnrollResponse{}, fmt.Errorf("enroll status %d body=%s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return EnrollResponse{}, err
	}
	if strings.TrimSpace(out.CertPEM) == "" || strings.TrimSpace(out.CABundlePEM) == "" {
		return EnrollResponse{}, fmt.Errorf("invalid enroll response")
	}
	return out, nil
}

func (c *PanelClient) RenewCert(ctx context.Context, csrPEM string) (EnrollResponse, error) {
	body, _ := json.Marshal(map[string]any{"csr_pem": csrPEM})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/nodes/%d/cert/renew", c.baseURL, c.nodeID), bytes.NewReader(body))
	if err != nil {
		return EnrollResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return EnrollResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return EnrollResponse{}, fmt.Errorf("renew status %d body=%s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return EnrollResponse{}, err
	}
	return out, nil
}
