FROM golang:1.25-bookworm AS builder

ARG HTTP_PROXY
ARG HTTPS_PROXY
ARG ALL_PROXY

ENV http_proxy=${HTTP_PROXY}
ENV https_proxy=${HTTPS_PROXY}
ENV all_proxy=${ALL_PROXY}

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=1 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/neutrino ./cmd/server

FROM node:24-bookworm AS ops-demo-builder

WORKDIR /src/frontend/ops-demo

COPY frontend/ops-demo/package.json frontend/ops-demo/package-lock.json ./
RUN npm ci

COPY frontend/ops-demo ./
RUN npm run build

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /out/neutrino /app/neutrino
COPY internal/app/templates /app/internal/app/templates
COPY --from=ops-demo-builder /src/frontend/ops-demo/dist /app/frontend/ops-demo/dist

ENV ADDR=:8080
ENV DB_PATH=/data/neutrino.db

VOLUME ["/data"]
EXPOSE 8080

CMD ["/app/neutrino"]
