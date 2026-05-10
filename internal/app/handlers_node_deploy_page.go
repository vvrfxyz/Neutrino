package app

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"neutrino/internal/repo"
)

type nodeDeployPageData struct {
	Node           repo.Node
	NodeID         int64
	NodeName       string
	EnrollCode     string
	EnrollExpires  string
	EnrollUsedAt   string
	PanelPublicURL string
	PanelMTLSURL   string
	ConfigError    string
	DeployDir      string
	AgentImage     string
	XrayImage      string
	Script         string
	CertPins       []repo.NodeCertPin
	Jobs           []repo.NodeJob
	ManagedXray    nodeExtraXray
	ExtraJSONError string
}

func (a *App) handleNodeRoutesPage(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/nodes/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	nodeID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || nodeID <= 0 {
		http.Error(w, "bad node id", http.StatusBadRequest)
		return
	}
	if len(parts) == 2 && parts[1] == "deploy" {
		switch r.Method {
		case http.MethodGet:
			a.handleNodeDeployPage(w, r, nodeID)
			return
		case http.MethodPost:
			a.handleNodeDeployRotate(w, r, nodeID)
			return
		}
	}
	http.NotFound(w, r)
}

func (a *App) handleNodeDeployRotate(w http.ResponseWriter, r *http.Request, nodeID int64) {
	ttlMin := a.cfg.NodeEnrollCodeTTLMin
	if ttlMin <= 0 {
		ttlMin = 10
	}
	if _, err := a.store.CreateNodeEnrollCode(r.Context(), nodeID, time.Duration(ttlMin)*time.Minute); err != nil {
		http.Error(w, "create enroll code failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/nodes/%d/deploy", nodeID), http.StatusSeeOther)
}

func (a *App) handleNodeDeployPage(w http.ResponseWriter, r *http.Request, nodeID int64) {
	node, err := a.store.GetNode(r.Context(), nodeID)
	if err != nil {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}

	code, ok, err := a.store.GetNodeEnrollCode(r.Context(), nodeID)
	if err != nil {
		http.Error(w, "get enroll code failed", http.StatusInternalServerError)
		return
	}
	if !ok || !isUsableNodeEnrollCode(code) {
		code = repo.NodeEnrollCode{NodeID: nodeID}
	}

	panelPublic, panelOK := parsePublicURL(a.cfg.SubBaseURL, false)
	panelMTLS, mtlsOK := derivePanelMTLSURL(panelPublic, a.cfg.NodeDefaultPanelMTLSURL, a.cfg.PanelAgentMTLSAddr)
	configErr := ""
	if !panelOK {
		configErr = "invalid SUB_BASE_URL: set a real public panel URL before generating deploy commands"
	} else if !mtlsOK {
		configErr = "invalid panel mTLS URL: set NODE_DEFAULT_PANEL_MTLS_URL or a valid PanelAgentMTLSAddr-bound public panel URL"
	}

	deployDir := a.cfg.NodeDefaultDeployDir
	if strings.TrimSpace(deployDir) == "" {
		deployDir = "/root/neutrino-node"
	}
	agentImage := strings.TrimSpace(a.cfg.NodeDefaultAgentImage)
	if agentImage == "" {
		agentImage = "ghcr.io/neutrino-proxy/agent:latest"
	}
	xrayImage := strings.TrimSpace(a.cfg.NodeDefaultXrayImage)
	if xrayImage == "" {
		// Prefer upstream official image with pinned tag (greenfield; breaking changes allowed).
		xrayImage = "ghcr.io/xtls/xray-core:26.2.6"
	}

	vlessPort := node.Port
	if vlessPort <= 0 {
		vlessPort = 443
	}
	script := ""
	if configErr == "" && strings.TrimSpace(code.Code) != "" {
		script = buildNodeDeployScript(deployDir, panelPublic, panelMTLS, nodeID, code.Code, agentImage, xrayImage, vlessPort)
	}

	pins, _ := a.store.ListNodeCertPins(r.Context(), nodeID, 50)
	jobs, _ := a.store.ListNodeJobs(r.Context(), nodeID, 50)

	// Parse node.extra_json for managed xray config (best-effort; keep page usable on errors).
	var managedXray nodeExtraXray
	extraErr := ""
	if node.Managed && node.CoreType == "xray" {
		if ex, err := parseNodeExtra(node.ExtraJSON); err != nil {
			extraErr = err.Error()
		} else if ex.Xray != nil {
			managedXray = *ex.Xray
		} else {
			// Defaults match buildManagedXrayApplyPayload: rely on agent-local env; rollback enabled for safety.
			managedXray = nodeExtraXray{RollbackOnFail: true, Vars: map[string]string{}}
		}
		if managedXray.Vars == nil {
			managedXray.Vars = map[string]string{}
		}
	}

	current, _ := a.currentAdmin(r)
	panelLoc := a.cfg.PanelLocation()
	deployData := nodeDeployPageData{
		Node:           node,
		NodeID:         nodeID,
		NodeName:       node.Name,
		EnrollCode:     code.Code,
		EnrollExpires:  fmtMaybeTimeIn(&code.ExpiresAt, panelLoc),
		EnrollUsedAt:   fmtMaybeTimeIn(code.UsedAt, panelLoc),
		PanelPublicURL: panelPublic,
		PanelMTLSURL:   panelMTLS,
		ConfigError:    configErr,
		DeployDir:      deployDir,
		AgentImage:     agentImage,
		XrayImage:      xrayImage,
		Script:         script,
		CertPins:       pins,
		Jobs:           jobs,
		ManagedXray:    managedXray,
		ExtraJSONError: extraErr,
	}
	data := PageData{
		CurrentAdmin: current,
		ActivePage:   "nodes",
		CSRFToken:    a.csrfTokenFor(r),
		Content:      deployData,
	}
	if err := a.pages["node_deploy"].ExecuteTemplate(w, "base.tmpl", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func isUsableNodeEnrollCode(code repo.NodeEnrollCode) bool {
	if strings.TrimSpace(code.Code) == "" || code.ExpiresAt.IsZero() {
		return false
	}
	if code.UsedAt != nil && !code.UsedAt.IsZero() {
		return false
	}
	return time.Now().UTC().Before(code.ExpiresAt)
}

func fmtMaybeTimeIn(t *time.Time, loc *time.Location) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02 15:04:05")
}

func derivePanelMTLSURL(panelPublicURL string, mtlsPublicURL string, mtlsAddr string) (string, bool) {
	if u, ok := parsePublicURL(mtlsPublicURL, true); ok {
		return u, true
	}

	u, err := url.Parse(strings.TrimSpace(panelPublicURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	port := "8443"
	mtlsAddr = strings.TrimSpace(mtlsAddr)
	if strings.HasPrefix(mtlsAddr, ":") && len(mtlsAddr) > 1 {
		port = strings.TrimPrefix(mtlsAddr, ":")
	}
	host := u.Hostname()
	if host == "" {
		return "", false
	}
	// mTLS listener is always TLS, regardless of the admin UI/public URL scheme.
	return fmt.Sprintf("https://%s:%s", host, port), true
}

func parsePublicURL(raw string, forceHTTPS bool) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	scheme := strings.TrimSpace(u.Scheme)
	if forceHTTPS || scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + u.Host, true
}

func buildNodeDeployScript(deployDir, panelPublicURL, panelMTLSURL string, nodeID int64, enrollCode string, agentImage string, xrayImage string, vlessPort int) string {
	// Keep it POSIX-sh compatible.
	return fmt.Sprintf(`# Node deploy (pull model + auto-enroll)
set -eu

DEPLOY_DIR=%q
PANEL_URL=%q
PANEL_MTLS_URL=%q
NODE_ID=%d
ENROLL_CODE=%q
AGENT_IMAGE=%q
XRAY_IMAGE=%q
DOCKER_COMPOSE_VERSION=${DOCKER_COMPOSE_VERSION:-v5.0.1}

ensure_docker() {
  if command -v docker >/dev/null 2>&1; then
    start_docker_service
    return 0
  fi

  echo "info: docker not found, trying to install Docker Engine..."
  install_docker_engine

  if ! command -v docker >/dev/null 2>&1; then
    echo "error: docker install failed" >&2
    exit 1
  fi

  start_docker_service
}

start_docker_service() {
  if docker info >/dev/null 2>&1; then
    return 0
  fi

  if command -v systemctl >/dev/null 2>&1; then
    systemctl enable --now docker >/dev/null 2>&1 || systemctl start docker >/dev/null 2>&1 || true
  elif command -v service >/dev/null 2>&1; then
    service docker start >/dev/null 2>&1 || true
  elif command -v rc-service >/dev/null 2>&1; then
    rc-update add docker default >/dev/null 2>&1 || true
    rc-service docker start >/dev/null 2>&1 || true
  fi

  if ! docker info >/dev/null 2>&1; then
    echo "error: docker daemon is not running; start docker and rerun this script" >&2
    exit 1
  fi
}

install_docker_engine() {
  if command -v apt-get >/dev/null 2>&1; then
    install_docker_engine_apt
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y dnf-plugins-core || true
    dnf config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo || true
    dnf install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin || dnf install -y docker docker-compose-plugin || dnf install -y docker
  elif command -v yum >/dev/null 2>&1; then
    yum install -y yum-utils || true
    yum-config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo || true
    yum install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin || yum install -y docker docker-compose-plugin || yum install -y docker
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache docker docker-cli-compose
  else
    echo "error: unsupported OS: install Docker Engine manually and rerun this script" >&2
    exit 1
  fi
}

install_docker_engine_apt() {
  apt_update
  DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl gnupg

  os_id=debian
  os_codename=
  if [ -r /etc/os-release ]; then
    . /etc/os-release
    case "${ID:-}" in
      debian|ubuntu)
        os_id="$ID"
        ;;
      *)
        case " ${ID_LIKE:-} " in
          *" ubuntu "*)
            os_id=ubuntu
            ;;
          *" debian "*)
            os_id=debian
            ;;
        esac
        ;;
    esac
    os_codename="${VERSION_CODENAME:-}"
    if [ -z "$os_codename" ] && [ -n "${UBUNTU_CODENAME:-}" ]; then
      os_codename="$UBUNTU_CODENAME"
    fi
  fi

  if [ -n "$os_codename" ]; then
    install -m 0755 -d /etc/apt/keyrings
    gpg_url=https://download.docker.com/linux/${os_id}/gpg
    gpg_ok=0
    if command -v curl >/dev/null 2>&1; then
      if curl -fsSL "$gpg_url" -o /etc/apt/keyrings/docker.asc; then
        gpg_ok=1
      fi
    elif command -v wget >/dev/null 2>&1; then
      if wget -qO /etc/apt/keyrings/docker.asc "$gpg_url"; then
        gpg_ok=1
      fi
    else
      echo "warn: curl or wget is required for official Docker repo setup; trying distro docker packages" >&2
    fi
    if [ "$gpg_ok" = "1" ]; then
      chmod a+r /etc/apt/keyrings/docker.asc
      dpkg_arch=$(dpkg --print-architecture)
      official_repo_file=/etc/apt/sources.list.d/docker.sources
      cat > "$official_repo_file" <<EOF
Types: deb
URIs: https://download.docker.com/linux/${os_id}
Suites: ${os_codename}
Components: stable
Architectures: ${dpkg_arch}
Signed-By: /etc/apt/keyrings/docker.asc
EOF
      if apt_update &&
        DEBIAN_FRONTEND=noninteractive apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin; then
        return 0
      fi
      rm -f "$official_repo_file"
      apt_update || true
    fi
  fi

  echo "warn: official Docker apt install failed, trying distro docker packages" >&2
  DEBIAN_FRONTEND=noninteractive apt-get install -y docker.io docker-compose-plugin || DEBIAN_FRONTEND=noninteractive apt-get install -y docker.io
}

apt_update() {
  repair_debian_bullseye_apt_sources
  DEBIAN_FRONTEND=noninteractive apt-get update
}

backup_apt_source_file() {
  apt_file=$1
  if [ ! -f "$apt_file.bak.neutrino" ]; then
    cp -p "$apt_file" "$apt_file.bak.neutrino" 2>/dev/null || cp "$apt_file" "$apt_file.bak.neutrino" || true
  fi
}

repair_debian_bullseye_apt_sources() {
  if [ ! -r /etc/os-release ]; then
    return 0
  fi

  os_id=
  os_codename=
  . /etc/os-release
  os_id=${ID:-}
  os_codename=${VERSION_CODENAME:-}
  if [ "$os_id" != "debian" ] || [ "$os_codename" != "bullseye" ]; then
    return 0
  fi

  changed=0
  for apt_file in /etc/apt/sources.list /etc/apt/sources.list.d/*.list; do
    [ -f "$apt_file" ] || continue

    if grep -Eq '^[[:space:]]*deb(-src)?[[:space:]]+https?://security\.debian\.org(/debian-security)?/?[[:space:]]+bullseye/updates([[:space:]]|$)' "$apt_file"; then
      backup_apt_source_file "$apt_file"
      sed -i -E 's#(^[[:space:]]*deb(-src)?[[:space:]]+)https?://security\.debian\.org(/debian-security)?/?[[:space:]]+bullseye/updates#\1http://security.debian.org/debian-security bullseye-security#g' "$apt_file"
      changed=1
    fi

    if grep -Eq '^[[:space:]]*deb(-src)?[[:space:]]+[^[:space:]]+[[:space:]]+bullseye-backports([[:space:]]|$)' "$apt_file"; then
      backup_apt_source_file "$apt_file"
      sed -i -E '/^[[:space:]]*deb(-src)?[[:space:]]+[^[:space:]]+[[:space:]]+bullseye-backports([[:space:]]|$)/s/^/# disabled by Neutrino node bootstrap: /' "$apt_file"
      changed=1
    fi
  done

  if [ "$changed" = "1" ]; then
    echo "info: repaired Debian bullseye apt sources for security updates/backports"
  fi
}

ensure_docker_compose() {
  ensure_docker

  if docker compose version >/dev/null 2>&1; then
    return 0
  fi
  if command -v docker-compose >/dev/null 2>&1; then
    return 0
  fi

  echo "info: docker compose not found, trying to install..."

  if command -v apt-get >/dev/null 2>&1; then
    apt_update
    DEBIAN_FRONTEND=noninteractive apt-get install -y docker-compose-plugin || true
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y docker-compose-plugin || true
  elif command -v yum >/dev/null 2>&1; then
    yum install -y docker-compose-plugin || true
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache docker-cli-compose || true
  fi

  if docker compose version >/dev/null 2>&1; then
    return 0
  fi
  if command -v docker-compose >/dev/null 2>&1; then
    return 0
  fi

  install_docker_compose_binary

  if docker compose version >/dev/null 2>&1; then
    return 0
  fi
  if command -v docker-compose >/dev/null 2>&1; then
    return 0
  fi

  echo "error: docker compose install failed" >&2
  exit 1
}

install_docker_compose_binary() {
  os_name=$(uname -s)
  arch_name=$(uname -m)

  if [ "$os_name" != "Linux" ]; then
    echo "error: automatic docker compose install only supports Linux (got $os_name)" >&2
    exit 1
  fi

  case "$arch_name" in
    x86_64|amd64)
      compose_arch=x86_64
      ;;
    aarch64|arm64)
      compose_arch=aarch64
      ;;
    armv7l|armv7)
      compose_arch=armv7
      ;;
    *)
      echo "error: unsupported architecture for docker compose install: $arch_name" >&2
      exit 1
      ;;
  esac

  plugin_dir=/usr/local/lib/docker/cli-plugins
  plugin_path=$plugin_dir/docker-compose
  download_url=https://github.com/docker/compose/releases/download/${DOCKER_COMPOSE_VERSION}/docker-compose-linux-${compose_arch}
  tmp_file=$(mktemp)
  trap 'rm -f "$tmp_file"' EXIT HUP INT TERM

  mkdir -p "$plugin_dir"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$download_url" -o "$tmp_file"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$tmp_file" "$download_url"
  else
    echo "error: curl or wget is required to install docker compose automatically" >&2
    exit 1
  fi

  install -m 0755 "$tmp_file" "$plugin_path"
  rm -f "$tmp_file"
  trap - EXIT HUP INT TERM
}

compose_cmd() {
  if docker compose version >/dev/null 2>&1; then
    docker compose "$@"
  else
    docker-compose "$@"
  fi
}

mkdir -p "$DEPLOY_DIR/data/mtls" "$DEPLOY_DIR/data/agent"
cd "$DEPLOY_DIR"
ensure_docker_compose

if [ ! -f .env ]; then
cat > .env <<'EOF'
TZ=Asia/Shanghai

# xray (node)
HOSTNET_ENABLE=true
XRAY_INBOUND_TAG=vless-reality
XRAY_VLESS_PORT=%d
XRAY_API_PORT=10085
XRAY_API_LISTEN=127.0.0.1
XRAY_REALITY_DEST=www.microsoft.com:443
XRAY_REALITY_SERVER_NAME=www.microsoft.com
XRAY_REALITY_PRIVATE_KEY=REPLACE_WITH_REALITY_PRIVATE_KEY
XRAY_REALITY_SHORT_ID=REPLACE_WITH_SHORT_ID

# agent (node)
NODE_ID=%d
PANEL_URL=%s
PANEL_MTLS_URL=%s
ENROLL_CODE=%s

PANEL_MTLS_CA_CERT_PATH=/data/mtls/ca-bundle.crt
PANEL_MTLS_CLIENT_CERT_PATH=/data/mtls/node.crt
PANEL_MTLS_CLIENT_KEY_PATH=/data/mtls/node.key

AGENT_HTTP_ADDR=127.0.0.1:9090
XRAY_API_ADDR=127.0.0.1:10085
XRAY_FLOW=xtls-rprx-vision
XRAY_ACCESS_LOG_PATH=/var/log/xray/access.log
AGENT_ACCESS_LOG_TZ=UTC
XRAY_CONFIG_PATH=/usr/local/etc/xray/config.json
XRAY_RELOAD_ARGS_JSON=["docker","restart","neutrino-xray"]
HOST_PROC_PATH=/host/proc
AGENT_MONTH_TZ=Asia/Shanghai
STATE_PATH=/data/agent/state.json
ACCESS_POLL_SEC=2
STATS_POLL_SEC=5
AGENT_RUNTIME_REPORT_SEC=2
AGENT_STATIC_FACTS_REPORT_SEC=1800

QUEUE_DIR=/data/agent/queue
QUEUE_MAX_BYTES=200000000
PUSH_BATCH_MAX_EVENTS=500
ACCESS_ZERO_BYTE_MAX_PER_MIN_PER_USER=30
ACCESS_ZERO_BYTE_MAX_PER_MIN_PER_USER_DEST=5
EOF
else
  echo "warn: .env exists, not overwriting"
fi

if [ ! -f docker-compose.yml ]; then
cat > docker-compose.yml <<EOF
services:
  xray:
    image: ${XRAY_IMAGE}
    container_name: neutrino-xray
    user: "0:0"
    env_file:
      - .env
    command: ["run","-c","/usr/local/etc/xray/config.json"]
    network_mode: host
    volumes:
      - xray-logs:/var/log/xray
      - xray-etc:/usr/local/etc/xray
    restart: unless-stopped

  agent:
    image: ${AGENT_IMAGE}
    container_name: neutrino-agent
    env_file:
      - .env
    network_mode: host
    volumes:
      - ./data:/data
      - /proc:/host/proc:ro
      - /var/run/docker.sock:/var/run/docker.sock
      - xray-logs:/var/log/xray
      - xray-etc:/usr/local/etc/xray
    restart: unless-stopped

volumes:
  xray-logs:
  xray-etc:
EOF
else
  echo "warn: docker-compose.yml exists, not overwriting"
fi

compose_cmd up -d
echo "ok: node started; agent will auto-enroll and then long-poll jobs."
`, deployDir, panelPublicURL, panelMTLSURL, nodeID, enrollCode, agentImage, xrayImage,
		vlessPort,
		nodeID, panelPublicURL, panelMTLSURL, enrollCode,
	)
}
