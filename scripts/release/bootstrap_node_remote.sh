#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR"

REMOTE_HOST="${REMOTE_HOST:-}"
REMOTE_DIR="${REMOTE_DIR:-/root/neutrino-node}"
TMP_TAG="${TMP_TAG:-$(date -u +%Y%m%d-%H%M%S)}"
TMP_DIR="${REMOTE_DIR}/.bootstrap_tmp_${TMP_TAG}"

if [[ -z "$REMOTE_HOST" ]]; then
  echo "[bootstrap] ERROR: REMOTE_HOST is required (e.g. root@<node-host>)"
  exit 1
fi

echo "[bootstrap] mode: node-only"
echo "[bootstrap] remote: $REMOTE_HOST"
echo "[bootstrap] remote dir: $REMOTE_DIR"
echo "[bootstrap] tmp dir: $TMP_DIR"

ssh -o StrictHostKeyChecking=no "$REMOTE_HOST" "REMOTE_DIR='$REMOTE_DIR' TMP_DIR='$TMP_DIR' bash -s" <<'REMOTE_EOF'
set -euo pipefail
mkdir -p "$REMOTE_DIR"
mkdir -p "$REMOTE_DIR/data"
mkdir -p "$REMOTE_DIR/data/mtls"
mkdir -p "$REMOTE_DIR/data/agent"
mkdir -p "$TMP_DIR"
REMOTE_EOF

# Upload compose and env templates without overwriting secrets.
scp -o StrictHostKeyChecking=no \
  docker-compose.node-only.yml \
  docker-compose.node-hostnet.yml \
  .env.node.example \
  "$REMOTE_HOST:$TMP_DIR/"

ssh -o StrictHostKeyChecking=no "$REMOTE_HOST" "REMOTE_DIR='$REMOTE_DIR' TMP_DIR='$TMP_DIR' TMP_TAG='$TMP_TAG' bash -s" <<'REMOTE_EOF'
set -euo pipefail

cd "$REMOTE_DIR"

for f in docker-compose.node-only.yml docker-compose.node-hostnet.yml .env.node.example; do
  src="${TMP_DIR}/${f}"
  if [[ -f "$src" ]]; then
    if [[ -f "$f" ]]; then
      cp "$f" "$f.bak.${TMP_TAG}"
    fi
    mv "$src" "$f"
  fi
done
rm -rf "$TMP_DIR" || true

if [[ ! -f .env ]]; then
  cp .env.node.example .env
  echo "[bootstrap] created .env from .env.node.example (PLEASE EDIT before deploy)"
else
  appended=0
  while IFS= read -r line; do
    if [[ "$line" =~ ^[A-Za-z_][A-Za-z0-9_]*= ]]; then
      key="${line%%=*}"
      if ! grep -qE "^${key}=" .env; then
        echo "$line" >> .env
        appended=1
      fi
    fi
  done < .env.node.example
  if [[ "$appended" -eq 1 ]]; then
    echo "[bootstrap] merged missing keys into existing .env"
  else
    echo "[bootstrap] .env already up to date"
  fi
fi

echo "[bootstrap] done in $REMOTE_DIR"
echo "[bootstrap] next:"
echo "  1) edit $REMOTE_DIR/.env (PANEL_URL / PANEL_MTLS_URL / ENROLL_CODE / NODE_ID / REALITY key)"
echo "  2) run local deploy script: scripts/release/release_node.sh <TAG>"
REMOTE_EOF
