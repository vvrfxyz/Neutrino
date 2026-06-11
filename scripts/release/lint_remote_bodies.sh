#!/usr/bin/env bash
# Lint the remote-execution bodies embedded in deploy scripts.
#
# The deploy scripts ship their remote logic as quoted heredocs
# (<<'REMOTE_EOF'), which shellcheck does not parse when checking the
# outer script. An unterminated inner heredoc therefore passes both
# `bash -n` and a plain shellcheck run while silently swallowing every
# command after it on the remote host. This script extracts each
# REMOTE_EOF body and shellchecks it as a standalone bash script.
set -euo pipefail

cd "$(dirname "$0")"

fail=0
for script in *.sh; do
  [[ "$script" == "$(basename "$0")" ]] && continue
  grep -q "<<'REMOTE_EOF'" "$script" || continue
  body="$(mktemp -t remote-body.XXXXXX.sh)"
  awk "/<<'REMOTE_EOF'/{flag=1;next}/^REMOTE_EOF\$/{flag=0}flag" "$script" > "$body"
  if ! shellcheck --shell=bash --severity=warning "$body"; then
    echo "[lint] FAIL: remote body of $script (extracted to $body)" >&2
    fail=1
  else
    rm -f "$body"
  fi
done
exit "$fail"
