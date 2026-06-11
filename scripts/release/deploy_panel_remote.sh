#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <panel-tag>"
  exit 1
fi

TAG="$1"
REMOTE_HOST="${REMOTE_HOST:-}"
REMOTE_DIR="${REMOTE_DIR:-/root/neutrino}"
IMAGE_REPO="${IMAGE_REPO:-ghcr.io/vvrfxyz/neutrino-panel}"
PANEL_STACK_COMPOSE="${PANEL_STACK_COMPOSE:-}"
PANEL_STACK_SERVICE="${PANEL_STACK_SERVICE:-neutrino-panel}"

if [[ -z "$REMOTE_HOST" ]]; then
  echo "[deploy] ERROR: REMOTE_HOST is required (e.g. root@<panel-host>)"
  exit 1
fi

echo "[deploy] mode: panel-only"
echo "[deploy] remote: $REMOTE_HOST"
echo "[deploy] remote dir: $REMOTE_DIR"
if [[ -n "$PANEL_STACK_COMPOSE" ]]; then
  echo "[deploy] embedded stack compose: $PANEL_STACK_COMPOSE"
  echo "[deploy] embedded stack service: $PANEL_STACK_SERVICE"
fi
echo "[deploy] set panel image: $IMAGE_REPO:$TAG"

tmp_upload="$(mktemp -d)"
cleanup() {
  rm -rf "$tmp_upload"
}
trap cleanup EXIT
cp docker-compose.panel-only.yml "$tmp_upload/docker-compose.panel-only.yml"
cp docker-compose.panel-hostnet.yml "$tmp_upload/docker-compose.panel-hostnet.yml"

ssh -o StrictHostKeyChecking=no "$REMOTE_HOST" "mkdir -p '$REMOTE_DIR'"
scp -o StrictHostKeyChecking=no \
  "$tmp_upload/docker-compose.panel-only.yml" \
  "$REMOTE_HOST:$REMOTE_DIR/docker-compose.panel-only.yml.tmp.$TAG"
scp -o StrictHostKeyChecking=no \
  "$tmp_upload/docker-compose.panel-hostnet.yml" \
  "$REMOTE_HOST:$REMOTE_DIR/docker-compose.panel-hostnet.yml.tmp.$TAG"

ssh -o StrictHostKeyChecking=no "$REMOTE_HOST" "REMOTE_DIR='$REMOTE_DIR' IMAGE_REPO='$IMAGE_REPO' TAG='$TAG' PANEL_STACK_COMPOSE='$PANEL_STACK_COMPOSE' PANEL_STACK_SERVICE='$PANEL_STACK_SERVICE' bash -s" <<'REMOTE_EOF'
set -euo pipefail

cd "$REMOTE_DIR"

if [[ ! -f .env ]]; then
  echo "[deploy] missing .env in $REMOTE_DIR"
  echo "[deploy] run: scripts/release/bootstrap_remote.sh (from your local machine) and then edit $REMOTE_DIR/.env"
  exit 1
fi

for compose_file in docker-compose.panel-only.yml docker-compose.panel-hostnet.yml; do
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

# agent -> panel mTLS server (panel-side listener): verify node certs + issue node certs
PANEL_AGENT_MTLS_ADDR_VAL="$(grep -E '^PANEL_AGENT_MTLS_ADDR=' .env | tail -n 1 | cut -d= -f2- | sed 's/[[:space:]]*$//')"
if [[ -z "$PANEL_AGENT_MTLS_ADDR_VAL" ]]; then
  fail_env "PANEL_AGENT_MTLS_ADDR is required (panel agent mTLS listener)."
fi
check_env_file_exists_csv PANEL_AGENT_MTLS_CA_CERT_PATHS
check_env_file_exists PANEL_AGENT_MTLS_SERVER_CERT_PATH
check_env_file_exists PANEL_AGENT_MTLS_SERVER_KEY_PATH
check_env_file_exists PANEL_AGENT_MTLS_SIGNING_CA_CERT_PATH
check_env_file_exists PANEL_AGENT_MTLS_SIGNING_CA_KEY_PATH

if [[ -n "$PANEL_STACK_COMPOSE" ]]; then
  if [[ ! -f "$PANEL_STACK_COMPOSE" ]]; then
    fail_env "PANEL_STACK_COMPOSE not found: $PANEL_STACK_COMPOSE"
  fi
  stack_dir="$(dirname "$PANEL_STACK_COMPOSE")"
  stack_file="$(basename "$PANEL_STACK_COMPOSE")"
  cp "$PANEL_STACK_COMPOSE" "${PANEL_STACK_COMPOSE}.bak.${TAG}"
  python3 - "$PANEL_STACK_COMPOSE" "$PANEL_STACK_SERVICE" "${IMAGE_REPO}:${TAG}" <<'PY'
import re
import sys

path, service, image = sys.argv[1], sys.argv[2], sys.argv[3]
with open(path, "r", encoding="utf-8") as f:
    lines = f.readlines()

service_re = re.compile(rf"^  {re.escape(service)}:\s*$")
next_service_re = re.compile(r"^  [A-Za-z0-9_.-]+:\s*$")
image_re = re.compile(r"^    image:\s*.*$")
in_service = False
changed = False
for i, line in enumerate(lines):
    if service_re.match(line):
        in_service = True
        continue
    if in_service and next_service_re.match(line):
        break
    if in_service and image_re.match(line):
        lines[i] = f"    image: {image}\n"
        changed = True
        break

if not changed:
    raise SystemExit(f"service {service!r} image line not found in {path}")

with open(path, "w", encoding="utf-8") as f:
    f.writelines(lines)
PY
  cd "$stack_dir"
  docker compose -f "$stack_file" pull "$PANEL_STACK_SERVICE"
  docker compose -f "$stack_file" up -d --no-build "$PANEL_STACK_SERVICE"
  docker compose -f "$stack_file" ps "$PANEL_STACK_SERVICE"
  curl -fsS http://127.0.0.1:8080/healthz
  exit 0
fi

if [[ -f docker-compose.release.yml ]]; then
  cp docker-compose.release.yml docker-compose.release.yml.bak.${TAG}
fi

cat > docker-compose.release.yml <<EOF
services:
  neutrino:
    image: ${IMAGE_REPO}:${TAG}
EOF

extra_args=()
if [[ -f docker-compose.panel-hostnet.yml ]] && grep -qE '^\s*HOSTNET_ENABLE\s*=\s*(1|true|yes)\s*$' .env; then
  echo "[deploy] hostnet enabled via HOSTNET_ENABLE; applying docker-compose.panel-hostnet.yml"
  extra_args=(-f docker-compose.panel-hostnet.yml)
fi

docker compose -f docker-compose.panel-only.yml -f docker-compose.release.yml "${extra_args[@]}" pull neutrino
docker compose -f docker-compose.panel-only.yml -f docker-compose.release.yml "${extra_args[@]}" up -d --no-build neutrino
docker compose -f docker-compose.panel-only.yml -f docker-compose.release.yml "${extra_args[@]}" ps
curl -fsS http://127.0.0.1:8080/healthz
REMOTE_EOF

echo
echo "[deploy] done: $IMAGE_REPO:$TAG"
