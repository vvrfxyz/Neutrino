#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR"

TAG="${1:-$(date -u +%Y%m%d-%H%M%S)}"

cat >&2 <<EOF
[release] deploying panel/API tag: $TAG
[release] image must already be published by GitHub Actions workflow "Docker Image".
EOF

scripts/release/deploy_panel_remote.sh "$TAG"
