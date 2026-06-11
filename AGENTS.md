# AGENTS.md

## Project Scope
- This repository is the source of truth for the Neutrino panel and backend.
- Production Xray uses the pinned upstream container image; this repo never vendors or builds Xray itself.
- This is a greenfield project: prioritize correctness and simplicity over backward compatibility. Breaking changes are allowed unless explicitly required.

## Engineering Principles
- Greenfield rule: prefer the correct target design over compatibility shims. When an existing local design is wrong or too limiting, change it directly and update the dependent code/tests in the same development slice.
- Reject over-design: do not introduce broad abstractions, plugin systems, alternate kernels, or generalized protocols before the current feature needs them. Build the smallest end-to-end path that satisfies the intended production behavior, then extend only when a real second use case appears.
- For cross-cutting upgrades, start from the authoritative source of truth whenever feasible, then work backward through storage, APIs, UI, and tests. Avoid building temporary observation layers if the authoritative data can be obtained simply and safely first.
- For node-runtime features, prefer node-first implementation: make node-agent obtain the real runtime state first, then adapt panel storage, APIs, UI, and enforcement around that state in the same focused slice.
- Reject fallback that hides logic errors: do not add fallback paths that make broken authoritative behavior appear healthy. If fallback is explicitly required for availability, it must expose degraded state clearly in logs, metrics, UI, or tests.

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
- `AGENT_MONTH_TZ` (node-agent only) sets the timezone for the node natural-month RX/TX rollup key; fallback order is `AGENT_MONTH_TZ` → `time.Local` → UTC (with a one-time warning).

## Deployment Policy
- For container verification and release acceptance, skip local Docker startup.
- Canonical panel/API image publication is GitHub Actions workflow `Docker Image`.
- Pull requests build the Docker image only; they must not push registry images.
- Non-PR workflow runs publish multi-arch `linux/amd64,linux/arm64` images to `ghcr.io/<owner>/neutrino-panel` and `ghcr.io/<owner>/neutrino-node`.
- If `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` are configured, the workflow also publishes to Docker Hub. `DOCKERHUB_PANEL_IMAGE` and `DOCKERHUB_NODE_IMAGE` repo variables override the Docker Hub image names. `DOCKERHUB_IMAGE` remains a backward-compatible panel override. Defaults are `<DOCKERHUB_USERNAME>/neutrino-panel` and `<DOCKERHUB_USERNAME>/neutrino-node`.
- Deploy servers by pulling pinned tags produced by the workflow; remote servers must not build release images directly.
- Release scripts deploy published tags only; do not build release images outside GitHub Actions unless explicitly requested.
- Current production panel server: `<panel-host>` (`root@<panel-ip>`), using the embedded `/data/docker-compose.yml` stack with app files in `/data/neutrino`.
- `main` is protected: required status checks are `build` and `docker` with strict branch freshness, linear history, required conversation resolution, no force pushes, and no branch deletion.
- `main` has PR review rules enabled with `required_approving_review_count=0`; admins are not subject to branch protection (`enforce_admins=false`).

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
- Over-limit auto-disable is implemented by the usage ingest path `UsageService.RecordBatch → RecordUsageBatchIdempotent → applyUsageEventTx → enforceLimit`:
  - when `effective_bytes > monthly_limit_bytes + credit_bytes`, user status becomes `over_limit`,
  - active proxy links are deactivated,
  - `quota_windows.over_limit_at` and enforcement log are written.
- Quota threshold alerting is queued in the same ingest path via `queueQuotaAlertsTx` (80%/90%); sending is handled by the app worker.
- IP over-limit auto-disable is implemented by node report `online_snapshot` + `ApplyOnlineSnapshot` + `EnforceIPLimit`:
  - authoritative online sessions are tracked in `online_sessions` (distinct `client_ip`),
  - streak-based enforcement flips `active` users to `over_ip_limit`, deactivates links, and writes enforcement log + alert.
- Expiry/quota sweeps are called on request paths (before `ListUsers`, `CreateProxyLink`, `SetUserStatus`, `RecordUsage`) and periodically by the app worker loop.
- Xray stats delta + access log parsing are done by node-agent (panel never polls node stats/logs).
- Access log parsing should store destination metadata with zero-byte outbound events.
- Panel triggers managed operations via durable jobs:
  - `node_jobs.kind=users_sync|xray_apply|xray_rollback|probe_dns|probe_tcp|probe_http` (`probe_ping` is a legacy alias for `probe_tcp`).
  - Node-agent claims jobs (pull model), executes locally, and reports result/error for audit + retries.
  - Probe kinds dedupe per node+kind+correlation; all running jobs are bounded by the timeout sweeper.

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
- `/login` and `/logout`: session-based admin auth flow.
- `/users`: create user, list users (+ `/users/table` HTMX partial), link copy, generate link, enable/disable/delete.
- `/users/{id}`: user detail page with link, quota summary, recent events table.
- `/nodes`: node list/management; `/nodes/{id}/deploy`: node deploy + managed-xray page.
- `/traffic`: traffic charts (rollup-backed).
- `/enforcements`: enforcement log.
- `/ops`: operations page (host metrics, node cards, online users, probes, alerts).
- `/ops-v2`: flag-gated (`ENABLE_OPS_V2=true`) React preview dashboard served from `frontend/ops-demo/dist`.
