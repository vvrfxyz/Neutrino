package probe

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	KindPing = "probe_ping"
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
	switch strings.TrimSpace(kind) {
	case KindPing:
		target := strings.TrimSpace(p.Target)
		if err := validateHost(target); err != nil {
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
		if err := validateHost(strings.TrimSpace(p.Target)); err != nil {
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
	result := Result{Kind: strings.TrimSpace(kind), CheckedAt: checkedAt}
	timeout := time.Duration(normalizedTimeout(p.TimeoutMS)) * time.Millisecond
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch strings.TrimSpace(kind) {
	case KindPing:
		result.Target = strings.TrimSpace(p.Target)
		_, err := net.DefaultResolver.LookupIPAddr(runCtx, result.Target)
		result.LatencyMS = elapsedMS(start)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		result.Success = true
		return result
	case KindTCP:
		result.Target = fmt.Sprintf("%s:%d", strings.TrimSpace(p.Target), p.Port)
		d := net.Dialer{Timeout: timeout}
		conn, err := d.DialContext(runCtx, "tcp", result.Target)
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
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			},
		}
		resp, err := client.Do(req)
		result.LatencyMS = elapsedMS(start)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		defer resp.Body.Close()
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

func validateHost(target string) error {
	if target == "" {
		return fmt.Errorf("target required")
	}
	if len(target) > 255 {
		return fmt.Errorf("target too long")
	}
	if strings.ContainsAny(target, "/\\\x00\r\n\t ") {
		return fmt.Errorf("target must be a hostname or IP")
	}
	return nil
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
