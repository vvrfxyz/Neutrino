#!/usr/bin/env sh
set -eu

: "${XRAY_INBOUND_TAG:=vless-reality}"
: "${XRAY_VLESS_PORT:=24443}"
: "${XRAY_API_PORT:=10085}"
: "${XRAY_REALITY_DEST:=www.microsoft.com:443}"
: "${XRAY_REALITY_SERVER_NAME:=www.microsoft.com}"
: "${XRAY_REALITY_PRIVATE_KEY:=REPLACE_WITH_REALITY_PRIVATE_KEY}"
: "${XRAY_REALITY_SHORT_ID:=REPLACE_WITH_SHORT_ID}"

mkdir -p /etc/xray
mkdir -p /var/log/xray

# If a config already exists (e.g. managed by node-agent via shared volume),
# do not overwrite it on restart.
if [ ! -f /etc/xray/config.json ]; then
  envsubst < /etc/xray/config.tmpl.json > /etc/xray/config.json
fi

exec /usr/local/bin/xray run -c /etc/xray/config.json
