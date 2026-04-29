package agent

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type RealityConfig struct {
	PrivateKey string `json:"private_key"`
	PublicKey  string `json:"public_key"`
	ShortID    string `json:"short_id"`
	CreatedAt  string `json:"created_at"`
}

func isPlaceholder(v string) bool {
	v = strings.TrimSpace(v)
	return v == "" || strings.HasPrefix(v, "REPLACE_WITH_")
}

func decodeX25519KeyBase64(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty key")
	}
	// xray x25519 outputs URL-safe base64 without padding (base64.RawURLEncoding).
	// Accept both URL-safe and standard alphabets (+/ vs -_) and with/without padding.
	if b, err := base64.RawURLEncoding.DecodeString(raw); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(raw); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(raw); err == nil {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("invalid base64")
}

func x25519PublicKeyFromPrivate(privB64 string) (string, error) {
	privBytes, err := decodeX25519KeyBase64(privB64)
	if err != nil {
		return "", err
	}
	if len(privBytes) != 32 {
		return "", fmt.Errorf("invalid private key length: %d", len(privBytes))
	}
	curve := ecdh.X25519()
	priv, err := curve.NewPrivateKey(privBytes)
	if err != nil {
		return "", err
	}
	pub := priv.PublicKey().Bytes()
	// Match xray's "x25519" output encoding (URL-safe, no padding).
	return base64.RawURLEncoding.EncodeToString(pub), nil
}

func randomShortID() (string, error) {
	// xray reality shortId is hex; 8 chars is a common default.
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func generateRealityConfig() (RealityConfig, error) {
	curve := ecdh.X25519()
	priv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return RealityConfig{}, err
	}
	// Match xray's "x25519" output encoding (URL-safe, no padding).
	privB64 := base64.RawURLEncoding.EncodeToString(priv.Bytes())
	pubB64 := base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes())
	sid, err := randomShortID()
	if err != nil {
		return RealityConfig{}, err
	}
	return RealityConfig{
		PrivateKey: privB64,
		PublicKey:  pubB64,
		ShortID:    sid,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func loadRealityConfig(path string) (RealityConfig, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return RealityConfig{}, false, nil
		}
		return RealityConfig{}, false, err
	}
	var rc RealityConfig
	if err := json.Unmarshal(b, &rc); err != nil {
		return RealityConfig{}, false, err
	}
	rc.PrivateKey = strings.TrimSpace(rc.PrivateKey)
	rc.PublicKey = strings.TrimSpace(rc.PublicKey)
	rc.ShortID = strings.TrimSpace(rc.ShortID)
	if rc.PrivateKey == "" || rc.PublicKey == "" || rc.ShortID == "" {
		return RealityConfig{}, false, fmt.Errorf("reality config missing fields")
	}
	// Validate keypair consistency (best-effort).
	if pub, err := x25519PublicKeyFromPrivate(rc.PrivateKey); err == nil && pub != "" && rc.PublicKey != pub {
		return RealityConfig{}, false, fmt.Errorf("reality keypair mismatch")
	}
	return rc, true, nil
}

func writeFileAtomic0600(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ensureRealityConfig resolves effective REALITY params with this precedence:
// 1) env (if not placeholders)
// 2) persisted file
// 3) generated and persisted
func ensureRealityConfig(path string) (RealityConfig, error) {
	// Load persisted (if any).
	persisted, ok, loadErr := loadRealityConfig(path)
	if loadErr != nil {
		log.Printf("warn: load reality config failed (%s): %v", path, loadErr)
	}

	priv := strings.TrimSpace(os.Getenv("XRAY_REALITY_PRIVATE_KEY"))
	sid := strings.TrimSpace(os.Getenv("XRAY_REALITY_SHORT_ID"))
	serverName := strings.TrimSpace(os.Getenv("XRAY_REALITY_SERVER_NAME"))
	_ = serverName // not persisted; only used for report

	usePriv := ""
	useSID := ""
	switch {
	case !isPlaceholder(priv):
		usePriv = priv
	case ok && !isPlaceholder(persisted.PrivateKey):
		usePriv = persisted.PrivateKey
	}
	switch {
	case !isPlaceholder(sid):
		useSID = sid
	case ok && strings.TrimSpace(persisted.ShortID) != "":
		useSID = persisted.ShortID
	}

	if usePriv == "" {
		gen, err := generateRealityConfig()
		if err != nil {
			return RealityConfig{}, err
		}
		usePriv = gen.PrivateKey
		if useSID == "" {
			useSID = gen.ShortID
		}
	}
	if useSID == "" {
		newSID, err := randomShortID()
		if err != nil {
			return RealityConfig{}, err
		}
		useSID = newSID
	}

	pub, err := x25519PublicKeyFromPrivate(usePriv)
	if err != nil {
		return RealityConfig{}, fmt.Errorf("derive public key: %w", err)
	}
	out := RealityConfig{
		PrivateKey: usePriv,
		PublicKey:  pub,
		ShortID:    useSID,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}

	// Persist (so agent can operate even if env stays placeholders).
	b, _ := json.MarshalIndent(out, "", "  ")
	if err := writeFileAtomic0600(path, b); err != nil {
		return out, err
	}
	return out, nil
}

func (a *Agent) lookupLocalTemplateVar(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	a.mu.RLock()
	rc := a.reality
	a.mu.RUnlock()

	switch k {
	case "XRAY_REALITY_PRIVATE_KEY":
		if rc != nil && !isPlaceholder(rc.PrivateKey) {
			return rc.PrivateKey
		}
	case "XRAY_REALITY_SHORT_ID":
		if rc != nil && strings.TrimSpace(rc.ShortID) != "" {
			return rc.ShortID
		}
	case "XRAY_VLESS_PORT":
		// Fixed default for vless+reality to keep firewall simple.
		v := strings.TrimSpace(os.Getenv("XRAY_VLESS_PORT"))
		if v == "" || isPlaceholder(v) {
			return "443"
		}
		if _, err := strconv.Atoi(v); err == nil {
			return v
		}
		return "443"
	}
	return ""
}

func (a *Agent) buildNodeReportPayload() *NodeReportPayload {
	a.mu.RLock()
	rc := a.reality
	a.mu.RUnlock()
	if rc == nil {
		return nil
	}

	// Prefer env for port and SNI; fall back to fixed defaults.
	port := 443
	if v := strings.TrimSpace(os.Getenv("XRAY_VLESS_PORT")); v != "" && !isPlaceholder(v) {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			port = p
		}
	}
	sni := strings.TrimSpace(os.Getenv("XRAY_REALITY_SERVER_NAME"))
	if isPlaceholder(sni) {
		sni = ""
	}

	return &NodeReportPayload{
		Port:      &port,
		SNI:       sni,
		PublicKey: strings.TrimSpace(rc.PublicKey),
		ShortID:   strings.TrimSpace(rc.ShortID),
	}
}
