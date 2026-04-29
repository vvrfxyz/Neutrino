#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT_DIR"

TAG="${1:-$(date -u +%Y%m%d-%H%M%S)}"

scripts/release/push_panel.sh "$TAG"
scripts/release/push_agent.sh "$TAG"
scripts/release/deploy_stack_remote.sh "$TAG"
