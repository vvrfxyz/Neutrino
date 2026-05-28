#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR"

IMAGE_REPO="${IMAGE_REPO:-ghcr.io/vvrfxyz/neutrino-panel}"
TAG="${1:-$(date -u +%Y%m%d-%H%M%S)}"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
CACHE_BASE="${BUILD_CACHE_DIR:-$ROOT_DIR/.buildx-cache}"
CACHE_KEY="${PLATFORMS//\//-}"
CACHE_KEY="${CACHE_KEY//,/-}"
CACHE_DIR="${CACHE_BASE}/panel-${CACHE_KEY}"
CACHE_DIR_NEW="${CACHE_DIR}.new"

echo "[release] image repo: $IMAGE_REPO"
echo "[release] tag: $TAG"
echo "[release] platforms: $PLATFORMS"
echo "[release] cache dir: $CACHE_DIR"

# Build may need proxy to download deps; capture before unsetting for registry operations.
HTTP_PROXY_ARG="${http_proxy:-${HTTP_PROXY:-}}"
HTTPS_PROXY_ARG="${https_proxy:-${HTTPS_PROXY:-}}"
ALL_PROXY_ARG="${all_proxy:-${ALL_PROXY:-}}"

# Keep registry operations direct unless explicitly configured otherwise.
unset http_proxy https_proxy all_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY

mkdir -p "$CACHE_BASE"
rm -rf "$CACHE_DIR_NEW"

cache_from_args=()
if [[ -d "$CACHE_DIR" ]]; then
  cache_from_args=(--cache-from "type=local,src=$CACHE_DIR")
fi

docker buildx build \
  --platform "$PLATFORMS" \
  --pull=false \
  --provenance=false \
  --sbom=false \
  --quiet \
  -f Dockerfile \
  --build-arg "HTTP_PROXY=$HTTP_PROXY_ARG" \
  --build-arg "HTTPS_PROXY=$HTTPS_PROXY_ARG" \
  --build-arg "ALL_PROXY=$ALL_PROXY_ARG" \
  "${cache_from_args[@]}" \
  --cache-to "type=local,dest=$CACHE_DIR_NEW,mode=max" \
  -t "$IMAGE_REPO:$TAG" \
  --push \
  .

if [[ -d "$CACHE_DIR_NEW" ]]; then
  rm -rf "$CACHE_DIR"
  mv "$CACHE_DIR_NEW" "$CACHE_DIR"
fi

echo
echo "[release] pushed: $IMAGE_REPO:$TAG"
echo "[release] next:"
echo "  scripts/release/deploy_panel_remote.sh $TAG"
