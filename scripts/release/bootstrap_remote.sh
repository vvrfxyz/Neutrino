#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR"

REMOTE_HOST="${REMOTE_HOST:-}"
REMOTE_DIR="${REMOTE_DIR:-/root/neutrino}"
TMP_TAG="${TMP_TAG:-$(date -u +%Y%m%d-%H%M%S)}"
TMP_DIR="${REMOTE_DIR}/.bootstrap_tmp_${TMP_TAG}"

if [[ -z "$REMOTE_HOST" ]]; then
  echo "[bootstrap] ERROR: REMOTE_HOST is required (e.g. root@<panel-host>)"
  exit 1
fi

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
  docker-compose.yml \
  docker-compose.panel-only.yml \
  docker-compose.node-only.yml \
  docker-compose.panel-hostnet.yml \
  docker-compose.node-hostnet.yml \
  docker-compose.hostnet.yml \
  .env.example \
  .env.node.example \
  "$REMOTE_HOST:$TMP_DIR/"

ssh -o StrictHostKeyChecking=no "$REMOTE_HOST" "REMOTE_DIR='$REMOTE_DIR' TMP_DIR='$TMP_DIR' TMP_TAG='$TMP_TAG' bash -s" <<'REMOTE_EOF'
set -euo pipefail

cd "$REMOTE_DIR"

for f in docker-compose.yml docker-compose.panel-only.yml docker-compose.node-only.yml docker-compose.panel-hostnet.yml docker-compose.node-hostnet.yml docker-compose.hostnet.yml .env.example .env.node.example; do
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
  cp .env.example .env
  echo "[bootstrap] created .env from .env.example (PLEASE EDIT before deploy)"
else
  appended=0
  # Merge new keys from .env.example into existing .env (non-destructive).
  while IFS= read -r line; do
    if [[ "$line" =~ ^[A-Za-z_][A-Za-z0-9_]*= ]]; then
      key="${line%%=*}"
      if ! grep -qE "^${key}=" .env; then
        echo "$line" >> .env
        appended=1
      fi
    fi
  done < .env.example
  if [[ "$appended" -eq 1 ]]; then
    echo "[bootstrap] merged missing keys into existing .env"
  else
    echo "[bootstrap] .env already up to date"
  fi
fi

# If REALITY_SHORT_ID is still placeholder but XRAY_REALITY_SHORT_ID exists, copy it.
if grep -qE '^REALITY_SHORT_ID=REPLACE_WITH_' .env && grep -qE '^XRAY_REALITY_SHORT_ID=' .env; then
  sid="$(grep -E '^XRAY_REALITY_SHORT_ID=' .env | tail -n 1 | cut -d= -f2-)"
  if [[ -n "$sid" ]] && [[ "$sid" != REPLACE_WITH_* ]]; then
    sed -i "s|^REALITY_SHORT_ID=.*$|REALITY_SHORT_ID=${sid}|" .env || true
    echo "[bootstrap] copied REALITY_SHORT_ID from XRAY_REALITY_SHORT_ID"
  fi
fi

echo "[bootstrap] done in $REMOTE_DIR"
echo "[bootstrap] next:"
echo "  1) edit $REMOTE_DIR/.env (ADMIN_PASS / REALITY keys / domains etc.)"
echo "  2) deploy a GitHub Actions published tag: scripts/release/release_panel.sh <TAG>"
REMOTE_EOF
