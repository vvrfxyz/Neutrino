# AGENTS.md

## Project Scope
- This repository is the source of truth for the Neutrino panel and backend.
- `Xray-core/` in this repo is **reference only**. Do not run or modify it for production behavior.
- Production Xray should use installed release binaries.
- This is a greenfield project: prioritize correctness and simplicity over backward compatibility. Breaking changes are allowed unless explicitly required.

## Runtime Decisions
- Primary protocol: `VLESS + REALITY + xtls-rprx-vision`.
- Admin backend: Go SSR templates + HTMX/Alpine.js.
- Database: SQLite.
- Admin auth: session login is default, Basic Auth is optional fallback (`ALLOW_BASIC_AUTH=true`).
- Panel <-> node-agent auth: mTLS (mutual TLS). No bearer tokens.
- Deployment topology: panel-only on one machine; node-only (agent + xray) on each proxy node.
- Xray container image (node): `ghcr.io/xtls/xray-core:26.2.6` (pinned).

## Environment
- Default local proxy for networked commands:
  - `export https_proxy=http://127.0.0.1:6152`
  - `export http_proxy=http://127.0.0.1:6152`
  - `export all_proxy=socks5://127.0.0.1:6153`
- `PANEL_TZ` (panel-only, default `UTC`) drives panel-side natural-month host-net rollup and display. Do not rely on process `TZ` / `time.Local` for month keys.
- `AGENT_ACCESS_LOG_TZ` (node-agent only, default empty = `time.Local`) is the timezone used to parse Xray access-log timestamps (which carry no offset).

## Deployment Policy
- For container verification and release acceptance, skip local Docker startup.
- Release workflow is mandatory: build and push `linux/amd64` images to Docker Hub from local machine first, then deploy on server by pulling pinned tags.
- Remote server must not build release images directly.
- Prefer disabling proxy vars during Docker Hub operations unless explicitly requested.
- Current production panel server: `<panel-host>` (`root@<panel-ip>`), using the embedded `/data/docker-compose.yml` stack with app files in `/data/neutrino`.

## Data Rules
- Usage ingestion must support idempotency (`source + source_event_id`).
- Quota windows are per-user and roll by `users.quota_cycle` (`day|week|month`) in `users.quota_tz`.
- Quota enforcement is based on window counters:
  - `effective_bytes` is computed using `counting_mode` (`single` outbound only, `double` inbound+outbound).
  - Over-limit triggers when `effective_bytes > monthly_limit_bytes + credit_bytes`.
- Expiry auto-disable is implemented by `SweepExpiredUsers`:
  - it changes expired `active` users to `expired`,
  - deactivates active proxy links,
  - records enforcement log with reason `expired`.
- Over-limit auto-disable is implemented by `RecordUsage -> enforceLimit`:
  - when `effective_bytes > monthly_limit_bytes + credit_bytes`, user status becomes `over_limit`,
  - active proxy links are deactivated,
  - `quota_windows.over_limit_at` and enforcement log are written.
- Quota threshold alerting is queued in `RecordUsage -> queueQuotaAlertsTx` (80%/90%); sending is handled by the app worker.
- IP over-limit auto-disable is implemented by `RecordUsage -> touchOnlineSessionTx` + `EnforceIPLimit`:
  - online sessions are tracked in `online_sessions` (distinct `client_ip`),
  - streak-based enforcement flips `active` users to `over_ip_limit`, deactivates links, and writes enforcement log + alert.
- Expiry/quota sweeps are called on request paths (before `ListUsers`, `CreateProxyLink`, `SetUserStatus`, `RecordUsage`) and periodically by the app worker loop.
- Xray stats delta + access log parsing are done by node-agent (panel never polls node stats/logs).
- Access log parsing should store destination metadata with zero-byte outbound events.
- Panel triggers managed operations via durable jobs:
  - `node_jobs.kind=users_sync|xray_apply|xray_rollback`
  - Node-agent claims jobs (pull model), executes locally, and reports result/error for audit + retries.

## Safety
- Never commit secrets (private keys, passwords, tokens).
- REALITY private keys must stay outside git and be injected via env or secret mount.
- Managed Xray operations must not ship executable shell commands or arbitrary file paths in job payloads; node-agent executes only agent-local configured argv (no `/bin/sh -c`).
- Keep `.db`, WAL, and runtime artifacts ignored.

## Delivery Gates
- Before moving to next development phase, ensure:
  1. `go test ./...` passes.
  2. Local smoke tests for login, create user, generate link, enable/disable/delete, detail page, usage ingest, expiry auto-disable behavior, and over-limit auto-disable behavior pass.
  3. Container build and startup checks pass (if Docker available).

## Current Admin UI Baseline
- `/users`: create user, list users, link copy, generate link, enable/disable/delete.
- `/users/{id}`: user detail page with link, quota summary, recent events table.
- `/ops`: operations page (host metrics + online users).
- `/login` and `/logout`: session-based admin auth flow.
