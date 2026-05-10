package agent

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"

	"neutrino/internal/templates"
)

func (a *Agent) bootstrapXrayConfigIfMissing(ctx context.Context) {
	if a == nil {
		return
	}
	cfgPath := strings.TrimSpace(a.xrayConfigPath)
	if cfgPath == "" {
		return
	}
	if _, err := os.Stat(cfgPath); err == nil {
		return
	} else if !os.IsNotExist(err) {
		log.Printf("warn: stat xray config failed (%s): %v", cfgPath, err)
		return
	}

	tpl := strings.TrimSpace(templates.XrayConfigTemplate)
	if tpl == "" {
		log.Printf("warn: embedded xray template is empty; cannot bootstrap config")
		return
	}

	rendered := os.Expand(tpl, func(k string) string {
		// Prefer explicit environment values unless they are placeholders.
		if v := os.Getenv(k); !isPlaceholder(v) {
			return v
		}
		// Agent-local fallbacks: REALITY key material and fixed port defaults.
		if v := a.lookupLocalTemplateVar(k); v != "" {
			return v
		}
		// Safe defaults for bootstrapping.
		switch k {
		case "XRAY_INBOUND_TAG":
			return "vless-reality"
		case "XRAY_API_PORT":
			return "10085"
		case "XRAY_API_LISTEN":
			return "127.0.0.1"
		case "XRAY_REALITY_DEST":
			return "www.microsoft.com:443"
		case "XRAY_REALITY_SERVER_NAME":
			return "www.microsoft.com"
		default:
			// Last resort: return env (even if placeholder) so failures are explicit.
			return os.Getenv(k)
		}
	})
	b := []byte(rendered)
	if !json.Valid(b) {
		log.Printf("warn: bootstrap xray config is not valid json; skipping")
		return
	}

	if err := writeFileAtomic0600(cfgPath, b); err != nil {
		log.Printf("warn: write bootstrap xray config failed (%s): %v", cfgPath, err)
		return
	}
	log.Printf("info: bootstrapped xray config at %s", cfgPath)

	// Best-effort restart so xray picks up the new config. Ignore errors: xray container might not exist yet.
	if len(a.xrayReloadArgs) > 0 {
		if ctx == nil {
			ctx = context.Background()
		}
		reloadCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		if err := runArgs(reloadCtx, expandArgs(a.xrayReloadArgs, cfgPath, cfgPath), cfgPath, cfgPath); err != nil {
			log.Printf("warn: xray reload after bootstrap failed (args=%v): %v", a.xrayReloadArgs, err)
		}
	}
}
