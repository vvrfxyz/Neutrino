#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <tag>"
  echo
  echo "env:"
  echo "  REMOTE_HOST=root@<node-host>"
  echo "  REMOTE_DIR=/root/neutrino-node"
  exit 1
fi

TAG="$1"
REMOTE_HOST="${REMOTE_HOST:-}"
REMOTE_DIR="${REMOTE_DIR:-/root/neutrino-node}"
AGENT_IMAGE_REPO="${AGENT_IMAGE_REPO:-ghcr.io/neutrino-proxy/agent}"
XRAY_IMAGE="${XRAY_IMAGE:-ghcr.io/xtls/xray-core:26.2.6}"

if [[ -z "$REMOTE_HOST" ]]; then
  echo "[deploy] ERROR: REMOTE_HOST is required (node host)"
  exit 1
fi

echo "[deploy] mode: node-only"
echo "[deploy] remote: $REMOTE_HOST"
echo "[deploy] remote dir: $REMOTE_DIR"
echo "[deploy] set xray image: $XRAY_IMAGE"
echo "[deploy] set agent image: $AGENT_IMAGE_REPO:$TAG"

tmp_upload="$(mktemp -d)"
cleanup() {
  rm -rf "$tmp_upload"
}
trap cleanup EXIT
cp docker-compose.node-only.yml "$tmp_upload/docker-compose.node-only.yml"
cp docker-compose.node-hostnet.yml "$tmp_upload/docker-compose.node-hostnet.yml"

ssh -o StrictHostKeyChecking=no "$REMOTE_HOST" "mkdir -p '$REMOTE_DIR'"
scp -o StrictHostKeyChecking=no \
  "$tmp_upload/docker-compose.node-only.yml" \
  "$REMOTE_HOST:$REMOTE_DIR/docker-compose.node-only.yml.tmp.$TAG"
scp -o StrictHostKeyChecking=no \
  "$tmp_upload/docker-compose.node-hostnet.yml" \
  "$REMOTE_HOST:$REMOTE_DIR/docker-compose.node-hostnet.yml.tmp.$TAG"

ssh -o StrictHostKeyChecking=no "$REMOTE_HOST" "REMOTE_DIR='$REMOTE_DIR' XRAY_IMAGE='$XRAY_IMAGE' AGENT_IMAGE_REPO='$AGENT_IMAGE_REPO' TAG='$TAG' bash -s" <<'REMOTE_EOF'
set -euo pipefail

cd "$REMOTE_DIR"

if [[ ! -f .env ]]; then
  echo "[deploy] missing .env in $REMOTE_DIR"
  echo "[deploy] copy .env.node.example to .env and fill required values"
  exit 1
fi

for compose_file in docker-compose.node-only.yml docker-compose.node-hostnet.yml; do
  tmp_file="${compose_file}.tmp.${TAG}"
  if [[ -f "$tmp_file" ]]; then
    if [[ -f "$compose_file" ]]; then
      cp "$compose_file" "${compose_file}.bak.${TAG}"
    fi
    mv "$tmp_file" "$compose_file"
  fi
done

fail_env() {
  echo "[deploy] ERROR: $1"
  exit 1
}

mounted_path_to_host() {
  local p="$1"
  if [[ "$p" == /data/* ]]; then
    echo ".${p}"
    return 0
  fi
  echo "$p"
}

check_env_file_exists() {
  local key="$1"
  local val
  val="$(grep -E "^${key}=" .env | tail -n 1 | cut -d= -f2-)"
  val="$(echo "$val" | sed 's/[[:space:]]*$//')"
  if [[ -z "$val" ]]; then
    return 0
  fi
  local host_path
  host_path="$(mounted_path_to_host "$val")"
  if [[ ! -f "$host_path" ]]; then
    fail_env "${key} points to missing file: ${val} (host: ${host_path})"
  fi
}

env_value() {
  local key="$1"
  local line
  line="$(grep -E "^${key}=" .env | tail -n 1 || true)"
  if [[ -z "$line" ]]; then
    return 0
  fi
  printf '%s\n' "${line#*=}" | sed 's/[[:space:]]*$//'
}

upsert_env() {
  local key="$1"
  local value="$2"
  if grep -qE "^${key}=" .env; then
    sed -i -E "s|^${key}=.*|${key}=${value}|" .env
  else
    printf '%s=%s\n' "$key" "$value" >> .env
  fi
}

# Host networking is the production default for node-only deployments so Xray
# sees the real client source IP instead of Docker bridge gateway addresses.
HOSTNET_ENABLE_VAL="$(env_value HOSTNET_ENABLE)"
if [[ -z "$HOSTNET_ENABLE_VAL" ]]; then
  cp .env .env.bak.hostnet-defaults.${TAG}
  upsert_env HOSTNET_ENABLE true
  HOSTNET_ENABLE_VAL=true
fi
if [[ "$HOSTNET_ENABLE_VAL" =~ ^(1|true|yes)$ ]]; then
  XRAY_API_PORT_VAL="$(env_value XRAY_API_PORT)"
  if [[ -z "$XRAY_API_PORT_VAL" ]]; then
    XRAY_API_PORT_VAL=10085
  fi
  cp .env .env.bak.hostnet-safety.${TAG}
  upsert_env XRAY_API_ADDR "127.0.0.1:${XRAY_API_PORT_VAL}"
  upsert_env XRAY_API_LISTEN 127.0.0.1
  upsert_env AGENT_HTTP_ADDR 127.0.0.1:9090
  upsert_env AGENT_ACCESS_LOG_TZ UTC
fi

PANEL_URL_VAL="$(grep -E '^PANEL_URL=' .env | tail -n 1 | cut -d= -f2- | sed 's/[[:space:]]*$//')"
if [[ -z "$PANEL_URL_VAL" ]]; then
  fail_env "PANEL_URL is required (used only for first-time enroll)."
fi
if [[ ! "$PANEL_URL_VAL" =~ ^https:// ]]; then
  fail_env "PANEL_URL must start with https://"
fi

PANEL_MTLS_URL_VAL="$(grep -E '^PANEL_MTLS_URL=' .env | tail -n 1 | cut -d= -f2- | sed 's/[[:space:]]*$//')"
if [[ -z "$PANEL_MTLS_URL_VAL" ]]; then
  fail_env "PANEL_MTLS_URL is required (agent -> panel mTLS URL)."
fi
if [[ ! "$PANEL_MTLS_URL_VAL" =~ ^https:// ]]; then
  fail_env "PANEL_MTLS_URL must start with https://"
fi

ENROLL_CODE_VAL="$(grep -E '^ENROLL_CODE=' .env | tail -n 1 | cut -d= -f2- | sed 's/[[:space:]]*$//')"
if [[ -z "$ENROLL_CODE_VAL" ]]; then
  fail_env "ENROLL_CODE is required for first-time node enroll (get it from /nodes/{id}/deploy)."
fi
if [[ "$ENROLL_CODE_VAL" == REPLACE_WITH_* ]]; then
  fail_env "ENROLL_CODE is placeholder. Set a real one-time code from panel."
fi

if [[ -f docker-compose.release.yml ]]; then
  cp docker-compose.release.yml docker-compose.release.yml.bak.${TAG}
fi

cat > docker-compose.release.yml <<EOF
services:
  xray:
    image: ${XRAY_IMAGE}
  agent:
    image: ${AGENT_IMAGE_REPO}:${TAG}
EOF

extra_args=()
if grep -qE '^\s*HOSTNET_ENABLE\s*=\s*(1|true|yes)\s*$' .env; then
  if [[ ! -f docker-compose.node-hostnet.yml ]]; then
    echo "[deploy] ERROR: HOSTNET_ENABLE is true but docker-compose.node-hostnet.yml is missing."
    echo "[deploy]        run scripts/release/bootstrap_node_remote.sh or re-run this deploy after syncing templates."
    exit 1
  fi
  echo "[deploy] hostnet enabled via HOSTNET_ENABLE; applying docker-compose.node-hostnet.yml"
  extra_args=(-f docker-compose.node-hostnet.yml)

  # In host network mode, service DNS won't resolve; agent must reach xray via loopback.
  XRAY_API_PORT_VAL="$(env_value XRAY_API_PORT)"
  if [[ -z "$XRAY_API_PORT_VAL" ]]; then
    XRAY_API_PORT_VAL=10085
  fi
  if ! grep -qE "^\\s*XRAY_API_ADDR\\s*=\\s*127\\.0\\.0\\.1:${XRAY_API_PORT_VAL}\\s*$" .env; then
    echo "[deploy] ERROR: hostnet enabled but XRAY_API_ADDR is not 127.0.0.1:${XRAY_API_PORT_VAL}."
    echo "[deploy]        set XRAY_API_ADDR=127.0.0.1:${XRAY_API_PORT_VAL} in .env and re-deploy."
    exit 1
  fi
  if grep -qE '^\s*XRAY_API_LISTEN\s*=\s*0\\.0\\.0\\.0\s*$' .env; then
    echo "[deploy] ERROR: hostnet enabled but XRAY_API_LISTEN is 0.0.0.0."
    echo "[deploy]        set XRAY_API_LISTEN=127.0.0.1 to keep the Xray API private."
    exit 1
  fi

  existing_config_dir="$(docker inspect neutrino-xray --format '{{range .Mounts}}{{if eq .Destination "/usr/local/etc/xray"}}{{.Source}}{{end}}{{end}}' 2>/dev/null || true)"
  existing_config_path="${existing_config_dir%/}/config.json"
  if [[ -n "$existing_config_dir" && -f "$existing_config_path" ]]; then
    if ! command -v python3 >/dev/null 2>&1; then
      echo "[deploy] ERROR: python3 is required to sanitize existing Xray config before hostnet switch."
      exit 1
    fi
    python3 - "$existing_config_path" <<'PY'
import json
import os
import shutil
import sys
import tempfile

path = sys.argv[1]
with open(path, "r", encoding="utf-8") as f:
    cfg = json.load(f)

changed = False
for inbound in cfg.get("inbounds", []):
    if inbound.get("tag") == "api-in" and inbound.get("listen") != "127.0.0.1":
        inbound["listen"] = "127.0.0.1"
        changed = True

if changed:
    shutil.copy2(path, f"{path}.bak.hostnet-safe.{os.environ.get('TAG', 'unknown')}")
    fd, tmp = tempfile.mkstemp(prefix=".config.", suffix=".tmp", dir=os.path.dirname(path))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(cfg, f, indent=2)
            f.write("\n")
        os.chmod(tmp, 0o600)
        os.replace(tmp, path)
    except Exception:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise
    print("[deploy] sanitized existing Xray api-in listen to 127.0.0.1 before hostnet switch")
else:
    print("[deploy] existing Xray api-in listen already hostnet-safe")
PY
  fi
fi

docker compose -f docker-compose.node-only.yml -f docker-compose.release.yml "${extra_args[@]}" pull xray agent
docker compose -f docker-compose.node-only.yml -f docker-compose.release.yml "${extra_args[@]}" up -d --no-build xray agent
docker compose -f docker-compose.node-only.yml -f docker-compose.release.yml "${extra_args[@]}" ps
REMOTE_EOF

echo
echo "[deploy] done: node-only $TAG"
