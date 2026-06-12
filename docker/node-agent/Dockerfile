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

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/node-agent ./cmd/node-agent
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/neutrinoctl ./cmd/neutrinoctl

FROM docker:27-cli

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /out/node-agent /app/node-agent
COPY --from=builder /out/neutrinoctl /usr/local/bin/neutrinoctl

EXPOSE 9090
CMD ["/app/node-agent"]

