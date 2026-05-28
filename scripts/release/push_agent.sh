#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR"

AGENT_IMAGE_REPO="${AGENT_IMAGE_REPO:-ghcr.io/vvrfxyz/neutrino-node}"
TAG="${1:-$(date -u +%Y%m%d-%H%M%S)}"
PLATFORM="${PLATFORM:-linux/amd64}"
CACHE_BASE="${BUILD_CACHE_DIR:-$ROOT_DIR/.buildx-cache}"
CACHE_DIR="${CACHE_BASE}/agent-${PLATFORM//\//-}"
CACHE_DIR_NEW="${CACHE_DIR}.new"

echo "[release] agent image repo: $AGENT_IMAGE_REPO"
echo "[release] tag: $TAG"
echo "[release] platform: $PLATFORM"
echo "[release] cache dir: $CACHE_DIR"

# Build may need proxy to download deps; capture before unsetting for Docker Hub operations.
HTTP_PROXY_ARG="${http_proxy:-${HTTP_PROXY:-}}"
HTTPS_PROXY_ARG="${https_proxy:-${HTTPS_PROXY:-}}"
ALL_PROXY_ARG="${all_proxy:-${ALL_PROXY:-}}"

# Keep Docker Hub operations direct unless explicitly configured otherwise.
unset http_proxy https_proxy all_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY

mkdir -p "$CACHE_BASE"
rm -rf "$CACHE_DIR_NEW"

cache_from_args=()
if [[ -d "$CACHE_DIR" ]]; then
  cache_from_args=(--cache-from "type=local,src=$CACHE_DIR")
fi

docker buildx build \
  --platform "$PLATFORM" \
  --pull=false \
  --provenance=false \
  --sbom=false \
  --quiet \
  -f docker/node-agent/Dockerfile \
  --build-arg "HTTP_PROXY=$HTTP_PROXY_ARG" \
  --build-arg "HTTPS_PROXY=$HTTPS_PROXY_ARG" \
  --build-arg "ALL_PROXY=$ALL_PROXY_ARG" \
  "${cache_from_args[@]+"${cache_from_args[@]}"}" \
  --cache-to "type=local,dest=$CACHE_DIR_NEW,mode=max" \
  -t "$AGENT_IMAGE_REPO:$TAG" \
  --load \
  .

if [[ -d "$CACHE_DIR_NEW" ]]; then
  rm -rf "$CACHE_DIR"
  mv "$CACHE_DIR_NEW" "$CACHE_DIR"
fi

docker push "$AGENT_IMAGE_REPO:$TAG"

echo
echo "[release] pushed: $AGENT_IMAGE_REPO:$TAG"
