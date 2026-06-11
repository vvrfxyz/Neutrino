#!/usr/bin/env bash
# Generate the panel agent-listener server certificate, signed by the CA from
# gen_ca.sh.
#
# Usage: scripts/mtls/gen_panel_server.sh <out-dir> "<san1,san2,...>"
#   SAN entries are autodetected as IP vs DNS, e.g. "panel.example.com,127.0.0.1".
#
# Produces <out-dir>/panel-agent-server.key and panel-agent-server.crt.
set -euo pipefail

out_dir="${1:?usage: gen_panel_server.sh <out-dir> <sans>}"
sans_csv="${2:?usage: gen_panel_server.sh <out-dir> <sans>}"
days="${SERVER_CERT_DAYS:-825}"

if [[ ! -f "$out_dir/ca.key" || ! -f "$out_dir/ca.crt" ]]; then
  echo "missing $out_dir/ca.key / ca.crt — run gen_ca.sh first" >&2
  exit 1
fi

san_list=""
first_san=""
IFS=',' read -ra entries <<<"$sans_csv"
for raw in "${entries[@]}"; do
  entry="$(echo "$raw" | tr -d '[:space:]')"
  [[ -z "$entry" ]] && continue
  [[ -z "$first_san" ]] && first_san="$entry"
  if [[ "$entry" =~ ^[0-9.]+$ || "$entry" == *:*:* ]]; then
    san_list+="${san_list:+,}IP:$entry"
  else
    san_list+="${san_list:+,}DNS:$entry"
  fi
done
if [[ -z "$san_list" ]]; then
  echo "no usable SAN entries in: $sans_csv" >&2
  exit 1
fi

key="$out_dir/panel-agent-server.key"
crt="$out_dir/panel-agent-server.crt"
csr="$(mktemp)"
trap 'rm -f "$csr"' EXIT

openssl ecparam -name prime256v1 -genkey -noout -out "$key"
chmod 600 "$key"
openssl req -new -key "$key" -subj "/CN=$first_san" -out "$csr"
openssl x509 -req -in "$csr" -CA "$out_dir/ca.crt" -CAkey "$out_dir/ca.key" \
  -CAcreateserial -sha256 -days "$days" \
  -extfile <(printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\nkeyUsage=critical,digitalSignature\nbasicConstraints=CA:FALSE\n' "$san_list") \
  -out "$crt"

echo "wrote $key and $crt (SANs: $san_list, valid $days days)"
