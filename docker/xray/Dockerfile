FROM debian:bookworm-slim

ARG HTTP_PROXY
ARG HTTPS_PROXY
ARG ALL_PROXY

ENV http_proxy=${HTTP_PROXY}
ENV https_proxy=${HTTPS_PROXY}
ENV all_proxy=${ALL_PROXY}

ARG XRAY_VERSION=1.8.24

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl unzip gettext-base \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /usr/local/share/xray

RUN curl -fsSL -o /tmp/xray.zip "https://github.com/XTLS/Xray-core/releases/download/v${XRAY_VERSION}/Xray-linux-64.zip" \
    && unzip /tmp/xray.zip -d /tmp/xray \
    && install -m 0755 /tmp/xray/xray /usr/local/bin/xray \
    && install -m 0644 /tmp/xray/geoip.dat /usr/local/share/xray/geoip.dat \
    && install -m 0644 /tmp/xray/geosite.dat /usr/local/share/xray/geosite.dat \
    && rm -rf /tmp/xray /tmp/xray.zip

WORKDIR /etc/xray

COPY internal/templates/xray/config.tmpl.json /etc/xray/config.tmpl.json
COPY docker/xray/entrypoint.sh /entrypoint.sh

RUN chmod +x /entrypoint.sh

EXPOSE 24443 10085

ENTRYPOINT ["/entrypoint.sh"]
