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

ssh -o StrictHostKeyChecking=no "$REMOTE_HOST" "REMOTE_DIR='$REMOTE_DIR' XRAY_IMAGE='$XRAY_IMAGE' AGENT_IMAGE_REPO='$AGENT_IMAGE_REPO' TAG='$TAG' bash -s" <<'REMOTE_EOF'
set -euo pipefail

cd "$REMOTE_DIR"

if [[ ! -f .env ]]; then
  echo "[deploy] missing .env in $REMOTE_DIR"
  echo "[deploy] copy .env.node.example to .env and fill required values"
  exit 1
fi

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
if [[ -f docker-compose.node-hostnet.yml ]] && grep -qE '^\s*HOSTNET_ENABLE\s*=\s*(1|true|yes)\s*$' .env; then
  echo "[deploy] hostnet enabled via HOSTNET_ENABLE; applying docker-compose.node-hostnet.yml"
  extra_args=(-f docker-compose.node-hostnet.yml)

  # In host network mode, service DNS won't resolve; agent must reach xray via loopback.
  if ! grep -qE '^\s*XRAY_API_ADDR\s*=\s*127\\.0\\.0\\.1:10085\s*$' .env; then
    echo "[deploy] ERROR: hostnet enabled but XRAY_API_ADDR is not 127.0.0.1:10085."
    echo "[deploy]        set XRAY_API_ADDR=127.0.0.1:10085 in .env and re-deploy."
    exit 1
  fi
fi

docker compose -f docker-compose.node-only.yml -f docker-compose.release.yml "${extra_args[@]}" pull xray agent
docker compose -f docker-compose.node-only.yml -f docker-compose.release.yml "${extra_args[@]}" up -d --no-build xray agent
docker compose -f docker-compose.node-only.yml -f docker-compose.release.yml "${extra_args[@]}" ps
REMOTE_EOF

echo
echo "[deploy] done: node-only $TAG"
