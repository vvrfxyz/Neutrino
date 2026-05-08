package probe

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const (
	KindDNS  = "probe_dns"
	KindPing = "probe_ping" // Legacy alias for probe_dns.
	KindTCP  = "probe_tcp"
	KindHTTP = "probe_http"
)

type Payload struct {
	Target       string `json:"target,omitempty"`
	Port         int    `json:"port,omitempty"`
	URL          string `json:"url,omitempty"`
	Method       string `json:"method,omitempty"`
	TimeoutMS    int    `json:"timeout_ms,omitempty"`
	Count        int    `json:"count,omitempty"`
	AllowPrivate bool   `json:"allow_private,omitempty"`
	ExpectStatus []int  `json:"expect_status,omitempty"`
}

type Result struct {
	Kind       string  `json:"kind"`
	Target     string  `json:"target"`
	Success    bool    `json:"success"`
	LatencyMS  float64 `json:"latency_ms"`
	StatusCode *int    `json:"status_code,omitempty"`
	Error      string  `json:"error,omitempty"`
	CheckedAt  string  `json:"checked_at"`
}

func ParsePayload(kind string, raw string) (Payload, error) {
	var p Payload
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return Payload{}, fmt.Errorf("invalid payload_json")
	}
	if err := Validate(kind, p); err != nil {
		return Payload{}, err
	}
	return p, nil
}

func Validate(kind string, p Payload) error {
	switch NormalizeKind(kind) {
	case KindDNS:
		target := strings.TrimSpace(p.Target)
		if err := validateHost(target, p.AllowPrivate); err != nil {
			return err
		}
		if normalizedTimeout(p.TimeoutMS) <= 0 {
			return fmt.Errorf("invalid timeout_ms")
		}
		count := p.Count
		if count == 0 {
			count = 3
		}
		if count < 1 || count > 5 {
			return fmt.Errorf("count must be 1..5")
		}
	case KindTCP:
		if err := validateHost(strings.TrimSpace(p.Target), p.AllowPrivate); err != nil {
			return err
		}
		if p.Port < 1 || p.Port > 65535 {
			return fmt.Errorf("port must be 1..65535")
		}
	case KindHTTP:
		u, err := url.Parse(strings.TrimSpace(p.URL))
		if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || strings.TrimSpace(u.Hostname()) == "" {
			return fmt.Errorf("url must be http or https")
		}
		if err := validateHost(u.Hostname(), p.AllowPrivate); err != nil {
			return err
		}
		method := strings.ToUpper(strings.TrimSpace(p.Method))
		if method == "" {
			method = http.MethodGet
		}
		if method != http.MethodGet && method != http.MethodHead {
			return fmt.Errorf("method must be GET or HEAD")
		}
		for _, status := range p.ExpectStatus {
			if status < 100 || status > 599 {
				return fmt.Errorf("expect_status must contain HTTP status codes")
			}
		}
	default:
		return fmt.Errorf("unsupported probe kind")
	}
	return nil
}

func Run(ctx context.Context, kind string, p Payload) Result {
	start := time.Now()
	checkedAt := start.UTC().Format(time.RFC3339)
	kind = NormalizeKind(kind)
	result := Result{Kind: kind, CheckedAt: checkedAt}
	timeout := time.Duration(normalizedTimeout(p.TimeoutMS)) * time.Millisecond
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch kind {
	case KindDNS:
		result.Target = strings.TrimSpace(p.Target)
		_, err := resolveAllowedIPs(runCtx, result.Target, p.AllowPrivate)
		result.LatencyMS = elapsedMS(start)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		result.Success = true
		return result
	case KindTCP:
		result.Target = TargetString(kind, p)
		conn, err := dialTCP(runCtx, strings.TrimSpace(p.Target), p.Port, timeout, p.AllowPrivate)
		result.LatencyMS = elapsedMS(start)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		_ = conn.Close()
		result.Success = true
		return result
	case KindHTTP:
		method := strings.ToUpper(strings.TrimSpace(p.Method))
		if method == "" {
			method = http.MethodGet
		}
		result.Target = strings.TrimSpace(p.URL)
		req, err := http.NewRequestWithContext(runCtx, method, result.Target, nil)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		client := &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
				DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					host, portText, err := net.SplitHostPort(address)
					if err != nil {
						return nil, err
					}
					port, err := net.LookupPort("tcp", portText)
					if err != nil {
						return nil, err
					}
					return dialTCP(ctx, host, port, timeout, p.AllowPrivate)
				},
			},
		}
		resp, err := client.Do(req)
		result.LatencyMS = elapsedMS(start)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		status := resp.StatusCode
		result.StatusCode = &status
		result.Success = expectedStatus(status, p.ExpectStatus)
		if !result.Success {
			result.Error = fmt.Sprintf("unexpected status %d", status)
		}
		return result
	default:
		result.Error = "unsupported probe kind"
		return result
	}
}

func normalizedTimeout(ms int) int {
	if ms <= 0 {
		return 3000
	}
	if ms < 100 {
		return 100
	}
	if ms > 30000 {
		return 30000
	}
	return ms
}

func NormalizeKind(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == KindPing {
		return KindDNS
	}
	return kind
}

func TargetString(kind string, p Payload) string {
	switch NormalizeKind(kind) {
	case KindHTTP:
		return strings.TrimSpace(p.URL)
	case KindTCP:
		return net.JoinHostPort(strings.TrimSpace(p.Target), fmt.Sprintf("%d", p.Port))
	case KindDNS:
		return strings.TrimSpace(p.Target)
	default:
		return strings.TrimSpace(p.Target)
	}
}

func CorrelationID(kind string, p Payload) string {
	return NormalizeKind(kind) + ":" + TargetString(kind, p)
}

func validateHost(target string, allowPrivate bool) error {
	if target == "" {
		return fmt.Errorf("target required")
	}
	if len(target) > 255 {
		return fmt.Errorf("target too long")
	}
	if strings.ContainsAny(target, "/\\\x00\r\n\t ") {
		return fmt.Errorf("target must be a hostname or IP")
	}
	if strings.Contains(target, ":") && net.ParseIP(target) == nil {
		return fmt.Errorf("target must not include a port")
	}
	lower := strings.TrimSuffix(strings.ToLower(target), ".")
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") || lower == "metadata.google.internal" || lower == "metadata" {
		return fmt.Errorf("target is not allowed")
	}
	if ip := net.ParseIP(target); ip != nil {
		if err := validateAllowedIP(ip, allowPrivate); err != nil {
			return err
		}
	}
	return nil
}

func resolveAllowedIPs(ctx context.Context, host string, allowPrivate bool) ([]net.IP, error) {
	host = strings.TrimSpace(host)
	if ip := net.ParseIP(host); ip != nil {
		if err := validateAllowedIP(ip, allowPrivate); err != nil {
			return nil, err
		}
		return []net.IP{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no addresses resolved")
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if err := validateAllowedIP(addr.IP, allowPrivate); err != nil {
			return nil, err
		}
		ips = append(ips, addr.IP)
	}
	return ips, nil
}

func dialTCP(ctx context.Context, host string, port int, timeout time.Duration, allowPrivate bool) (net.Conn, error) {
	ips, err := resolveAllowedIPs(ctx, host, allowPrivate)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: timeout}
	var lastErr error
	for _, ip := range ips {
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), fmt.Sprintf("%d", port)))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("connect failed")
}

func validateAllowedIP(ip net.IP, allowPrivate bool) error {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return fmt.Errorf("target address is not allowed")
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() || isCloudMetadataAddr(addr) {
		return fmt.Errorf("target address is not allowed")
	}
	if addr.IsPrivate() && !allowPrivate {
		return fmt.Errorf("private target address requires allow_private")
	}
	return nil
}

func isCloudMetadataAddr(addr netip.Addr) bool {
	switch addr {
	case netip.MustParseAddr("169.254.169.254"),
		netip.MustParseAddr("169.254.169.253"),
		netip.MustParseAddr("100.100.100.200"),
		netip.MustParseAddr("fd00:ec2::254"):
		return true
	default:
		return false
	}
}

func expectedStatus(status int, allowed []int) bool {
	if len(allowed) == 0 {
		return status >= 200 && status < 300
	}
	for _, v := range allowed {
		if status == v {
			return true
		}
	}
	return false
}

func elapsedMS(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}
