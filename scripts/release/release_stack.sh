#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR"

TAG="${1:-$(date -u +%Y%m%d-%H%M%S)}"

cat >&2 <<EOF
[release] deploying stack tag: $TAG
[release] note: canonical panel/API images are published by GitHub Actions workflow "Docker Image".
[release]       use scripts/release/push_panel.sh only as a local fallback publisher.
EOF

scripts/release/push_agent.sh "$TAG"
scripts/release/deploy_stack_remote.sh "$TAG"
