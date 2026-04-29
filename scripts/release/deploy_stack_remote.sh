#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <panel-tag>"
  exit 1
fi

TAG="$1"
REMOTE_HOST="${REMOTE_HOST:-}"
REMOTE_DIR="${REMOTE_DIR:-/root/neutrino}"
IMAGE_REPO="${IMAGE_REPO:-ghcr.io/neutrino-proxy/panel}"

if [[ -z "$REMOTE_HOST" ]]; then
  echo "[deploy] ERROR: REMOTE_HOST is required (e.g. root@<host>)"
  exit 1
fi

echo "[deploy] remote: $REMOTE_HOST"
echo "[deploy] remote dir: $REMOTE_DIR"
echo "[deploy] mode: panel-only (pull model; nodes deploy via /nodes/{id}/deploy)"
echo "[deploy] set panel image: $IMAGE_REPO:$TAG"

ssh -o StrictHostKeyChecking=no "$REMOTE_HOST" "REMOTE_DIR='$REMOTE_DIR' IMAGE_REPO='$IMAGE_REPO' TAG='$TAG' bash -s" <<'REMOTE_EOF'
set -euo pipefail

cd "$REMOTE_DIR"

if [[ ! -f .env ]]; then
  echo "[deploy] missing .env in $REMOTE_DIR"
  echo "[deploy] run: scripts/release/bootstrap_remote.sh (from your local machine) and then edit $REMOTE_DIR/.env"
  exit 1
fi

fail_env() {
  echo "[deploy] ERROR: $1"
  exit 1
}

mounted_path_to_host() {
  # Compose mounts ./data -> /data inside containers.
  # For env values like /data/mtls/ca.crt, validate against the host path ./data/mtls/ca.crt.
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

check_env_file_exists_csv() {
  local key="$1"
  local val
  val="$(grep -E "^${key}=" .env | tail -n 1 | cut -d= -f2-)"
  val="$(echo "$val" | sed 's/[[:space:]]*$//')"
  if [[ -z "$val" ]]; then
    return 0
  fi
  local IFS=','
  for item in $val; do
    item="$(echo "$item" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')"
    if [[ -z "$item" ]]; then
      continue
    fi
    local host_path
    host_path="$(mounted_path_to_host "$item")"
    if [[ ! -f "$host_path" ]]; then
      fail_env "${key} item points to missing file: ${item} (host: ${host_path})"
    fi
  done
}

# Minimal safety gate: refuse obvious placeholder secrets/config.
if grep -qE '^ADMIN_PASS=REPLACE_WITH_' .env; then
  fail_env "ADMIN_PASS is placeholder. Set a strong ADMIN_PASS in .env."
fi
ADMIN_PASS_VAL="$(grep -E '^ADMIN_PASS=' .env | tail -n 1 | cut -d= -f2-)"
if [[ "${#ADMIN_PASS_VAL}" -lt 12 ]]; then
  fail_env "ADMIN_PASS too short (<12). Set a strong ADMIN_PASS in .env."
fi
if [[ "$ADMIN_PASS_VAL" == "admin123" || "$ADMIN_PASS_VAL" == "admin" || "$ADMIN_PASS_VAL" == "password" ]]; then
  fail_env "ADMIN_PASS is weak. Set a strong ADMIN_PASS in .env."
fi
if grep -qE '^XRAY_REALITY_PRIVATE_KEY=REPLACE_WITH_' .env; then
  fail_env "XRAY_REALITY_PRIVATE_KEY is placeholder. Set REALITY private key in .env."
fi
if grep -qE '^REALITY_PUBLIC_KEY=REPLACE_WITH_' .env; then
  fail_env "REALITY_PUBLIC_KEY is placeholder. Set REALITY public key in .env."
fi
if grep -qE '^REALITY_SHORT_ID=REPLACE_WITH_' .env; then
  fail_env "REALITY_SHORT_ID is placeholder. Set REALITY short id in .env."
fi
PANEL_AGENT_MTLS_ADDR_VAL="$(grep -E '^PANEL_AGENT_MTLS_ADDR=' .env | tail -n 1 | cut -d= -f2- | sed 's/[[:space:]]*$//')"
if [[ -z "$PANEL_AGENT_MTLS_ADDR_VAL" ]]; then
  fail_env "PANEL_AGENT_MTLS_ADDR is required (panel agent mTLS listener)."
fi
check_env_file_exists_csv PANEL_AGENT_MTLS_CA_CERT_PATHS
check_env_file_exists PANEL_AGENT_MTLS_SERVER_CERT_PATH
check_env_file_exists PANEL_AGENT_MTLS_SERVER_KEY_PATH
check_env_file_exists PANEL_AGENT_MTLS_SIGNING_CA_CERT_PATH
check_env_file_exists PANEL_AGENT_MTLS_SIGNING_CA_KEY_PATH

if [[ -f docker-compose.release.yml ]]; then
  cp docker-compose.release.yml docker-compose.release.yml.bak.${TAG}
fi

cat > docker-compose.release.yml <<EOF
services:
  neutrino:
    image: ${IMAGE_REPO}:${TAG}
    build: "."
EOF

extra_args=()
if [[ -f docker-compose.panel-hostnet.yml ]] && grep -qE '^\s*HOSTNET_ENABLE\s*=\s*(1|true|yes)\s*$' .env; then
  echo "[deploy] hostnet enabled via HOSTNET_ENABLE; applying docker-compose.panel-hostnet.yml"
  extra_args=(-f docker-compose.panel-hostnet.yml)
elif [[ -f docker-compose.panel-hostnet.yml ]]; then
  echo "[deploy] docker-compose.panel-hostnet.yml present but HOSTNET_ENABLE not true; skipping hostnet overrides"
fi

docker compose -f docker-compose.panel-only.yml -f docker-compose.release.yml "${extra_args[@]}" pull neutrino
docker compose -f docker-compose.panel-only.yml -f docker-compose.release.yml "${extra_args[@]}" up -d --no-build neutrino
docker compose -f docker-compose.panel-only.yml -f docker-compose.release.yml "${extra_args[@]}" ps
curl -fsS http://127.0.0.1:8080/healthz
REMOTE_EOF

echo
echo "[deploy] done: $IMAGE_REPO:$TAG"
