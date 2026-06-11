#!/usr/bin/env bash
# Generate the Neutrino agent-mTLS CA (EC P-256, self-signed).
#
# Usage: scripts/mtls/gen_ca.sh <out-dir>
#
# Produces <out-dir>/ca.key and <out-dir>/ca.crt. The same CA both verifies
# node client certs on the panel's agent listener and signs enroll CSRs
# (issuing CA). Keep ca.key offline where possible; see the deployment manual.
set -euo pipefail

out_dir="${1:?usage: gen_ca.sh <out-dir>}"
days="${CA_DAYS:-3650}"
subject="${CA_SUBJECT:-/CN=Neutrino Agent CA}"

mkdir -p "$out_dir"
if [[ -e "$out_dir/ca.key" || -e "$out_dir/ca.crt" ]]; then
  echo "refusing to overwrite existing $out_dir/ca.key or ca.crt" >&2
  exit 1
fi

openssl ecparam -name prime256v1 -genkey -noout -out "$out_dir/ca.key"
chmod 600 "$out_dir/ca.key"
openssl req -x509 -new -key "$out_dir/ca.key" -sha256 -days "$days" \
  -subj "$subject" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -out "$out_dir/ca.crt"

echo "wrote $out_dir/ca.key and $out_dir/ca.crt (valid $days days)"
