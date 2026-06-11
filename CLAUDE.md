# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

For authoritative constraints and runtime decisions, see `AGENTS.md`.

## Common Commands

```bash
# rtk is a local wrapper around go commands (summarized output); plain `go` works too
rtk go run ./cmd/server          # panel — must run from the repo root (see Caveats)
rtk go run ./cmd/node-agent      # node agent

rtk go test ./...                # full suite (~335 tests across 21 packages)
rtk go test ./internal/app       # one package
rtk go test ./internal/app -run TestName

npm run test:e2e                 # Playwright e2e; boots the panel itself on 127.0.0.1:18080 with a temp DB

cd frontend/ops-demo && npm ci && npm test   # vitest for the /ops-v2 preview SPA
cd frontend/ops-demo && npm run build        # builds dist/ (required before ENABLE_OPS_V2=true serves /ops-v2)
```

CI only compiles binaries and builds images — it runs **no tests**. Always run `rtk go test ./...` locally before pushing.

## Architecture Overview

Two Go entrypoints plus one optional preview SPA:

- `cmd/server`: control plane / admin panel. Startup order: `app.ApplyPendingRestore` (staged DB restore) → open + migrate SQLite → seed admin credential (fatal if `ALLOW_BASIC_AUTH=true` with default admin/admin123) → `StartWorkers` → optional agent mTLS listener (`PANEL_AGENT_MTLS_ADDR`, TLS ≥ 1.2, RequireAndVerifyClientCert) → main HTTP listener (`ADDR`, default :8080).
- `cmd/node-agent`: per-node agent — enrolls with the panel, maintains mTLS credentials, manages local Xray, reports usage/runtime, executes node jobs.
- `frontend/ops-demo`: React + Vite preview ops dashboard served at `/ops-v2` when `ENABLE_OPS_V2=true` and `frontend/ops-demo/dist` exists (dist is gitignored; built inside the panel Docker image).

Panel layering: `internal/app` (HTTP/SSR/workers) → `internal/service` (orchestration: `UserService`, `NodeService`, `UsageService`, `OpsService`; `App` implements `service.SyncRequester` — `RequestUsersSync` is throttled to one enqueue/15s, `...Now` variants bypass) → `internal/repo` (SQLite domain core) → `internal/db` (schema + idempotent migrations; no version table). The service extraction is **partial**: node control-plane, cert, backup, ops-alert, and subscription handlers still call `repo.Store` directly.

`internal/app`:
- `routes.go` — SSR pages: `/login`, `/users` (+ `/users/table` HTMX partial), `/users/{id}`, `/nodes`, `/nodes/{id}/deploy`, `/traffic`, `/enforcements`, `/ops`, `/ops-v2` (flag-gated). Public: `/sub/{token}`, `/healthz`. API: `/api/v1/*` including `ops/config|nodes|alerts`, backups, and the admin WebSocket `/api/v1/stream` (session-only, same-origin checked).
- `AgentRoutes` (separate mTLS listener): `/api/v1/usage`, `/api/v1/nodes/{id}/report`, `…/agent/users`, `…/jobs/claim?wait=N`, `…/jobs/{job}/finish`, `…/cert/renew`. Every agent handler re-checks client-cert CN `node-{id}` against the path id. `/api/v1/usage` is also registered on the main mux but can never authenticate there (mTLS-only middleware on a plain-HTTP listener) — the agent listener is the only live usage-ingest path.
- `StartWorkers` (app.go) runs: host metrics sampler, host-net natural-month rollup (keyed by `PANEL_TZ`), Telegram polling, node reconciler (30s: users_sync + managed-xray convergence), job timeout sweeper (5s), ops WebSocket snapshot publisher (no DB work with 0 subscribers), metric-history queue (memory → disk spillover → drop + ops alert), and a 5s tick loop: lifecycle/quota/IP-limit enforcement + stale-node cleanup, pruning, ops-alert sync, pending-alert dispatch (Telegram/SMTP).
- Auth ladder (`auth.go`): admin session cookie → optional Basic (only with non-default creds) → mTLS node identity (CN + per-node cert-pin allowlist; fixed scopes `nodes:report`, `usage:write`) → API key (`X-API-Key`, SHA-256 hash lookup, scope map in `requiredScope`). CSRF token = HMAC-SHA256(`CSRF_SECRET`, sessionID), enforced on SSR POSTs and session-authenticated API writes.

`internal/repo` + `internal/db`: hand-written parameterized SQL; all timestamps UTC RFC3339 TEXT. DSN forces `_foreign_keys=on`, `_busy_timeout=15000`, `_txlock=immediate` (every `BeginTx` is a write transaction); WAL set during `Migrate`.

Node job model (`internal/repo/node_jobs.go`): kinds `users_sync | xray_apply | xray_rollback | probe_dns | probe_tcp | probe_http` (`probe_ping` legacy alias). pending → running → succeeded/failed; at most one pending job per node+kind (probe kinds dedupe by correlation); claim is per-node serial; finish must echo the claimed `attempt` (fencing); retryable failures requeue up to `NODE_JOB_MAX_ATTEMPTS`. Desired/applied state versions are SHA-256 content hashes (sorted users list / xray payload), re-reconciled every 30s — the panel never shells into nodes.

`internal/agent` (`Agent.Run`): localhost health server, cert-renewer state machine (12h check, renew within 7d of expiry, atomic install + PanelClient hot-swap, re-enroll fallback when expired), one-shot synced-user restore into Xray, job runner (25s long-poll claim), usage loop (1s tick), heartbeat (2s report incl. online-IP snapshot, monthly RX/TX keyed by `AGENT_MONTH_TZ` → `time.Local` → UTC).

Usage pipeline invariants (do **not** break — see `docs/USAGE_PIPELINE_DESIGN.md` and the 2026-03-28 postmortem):
- flush-before-sample: no new stats/access sampling while any disk-queued batch is pending;
- ack state (`AckedStats`/epoch/access offset) advances only after `PushUsage` succeeds; crash replay relies on panel idempotency;
- dedupe key `(source, source_event_id)` is enforced twice: `usage_event_keys` PK + unique index on `traffic_events`;
- stats counters are epoch-rebased on regression; ingest guards: +5min future skew, 26h backdate cap for active users.

Managed Xray: panel stores/ships only `{template, vars, rollback_on_fail}` (templates in `internal/templates/xray`); the agent renders with precedence job vars > non-placeholder env > agent-local fallbacks (REALITY keypair auto-generated and persisted in `reality.json`), skips no-op applies via canonical-JSON compare, then backup → test → rename → reload using agent-local argv only (`XRAY_TEST_ARGS_JSON` / `XRAY_RELOAD_ARGS_JSON`). Never `sh -c`, never panel-supplied commands or file paths.

Subscription rendering is centralized in `internal/subscription`: `/sub/{token}` renders per-node URIs (vless_reality / hysteria2 / tuic) for `clash|singbox|v2rayn|shadowrocket` (UA auto-detect, `?target=` override), emits `Subscription-Userinfo` / `Profile-Update-Interval` headers; falls back to env-config proxy only for unrestricted users when no nodes are enabled.

Node deletion is staged: enabled → disable + drain `users_sync` → actual DB delete only once desired/applied users version equals the empty-list hash.

## Operational Model

- Admin auth is session-based; Basic Auth is an optional fallback via `ALLOW_BASIC_AUTH=true`, only when the admin credential is non-default (enforced fatally at startup).
- `/api/v1/*` supports admin auth and pre-provisioned API keys. Scope mapping lives in `internal/app/auth.go`. `POST /api/v1/usage` accepts node mTLS only.
- Node-agent control-plane auth is mTLS only. Node certs are checked by CA trust **and** per-node allowlist/pin, so single-node revoke never requires CA rotation. Enroll uses one-time codes (default 10min TTL); disabled nodes may still drain `users_sync` jobs but cannot report or write usage.
- Usage ingestion is idempotent; quota (`day|week|month` windows in per-user `quota_tz`), expiry, and IP-limit enforcement can deactivate users and trigger node resync. See `AGENTS.md` Data Rules for the authoritative semantics.
- Raw `traffic_events` are pruned aggressively (access 7d, stats 2d by default); `traffic_rollups_hourly` is the only durable per-user history — charts read rollups only.

## Testing Notes

- Unit tests are colocated with packages.
- `tests/functional` boots the real app (HTTP httptest + a second mTLS httptest server with a generated CA and per-node client certs). Coverage: user lifecycle, traffic endpoints, usage idempotency + mTLS node-identity override, disabled-node drain semantics, managed-node job ordering, subscription failure modes/headers/UA detection, Telegram bind, backups + restore staging + `ApplyPendingRestore`, agent cert renew hot-swap, `/api/v1/stream` snapshot + WS/poll payload parity, sidebar HTMX invariants.
- `tests/e2e/admin-click.spec.ts` (Playwright, chromium) click-drives login/sidebar, users workflow, nodes + deploy + managed-xray, traffic/ops/enforcements.
- Neither Go tests nor Playwright/vitest run in CI (`build` + `docker` checks compile/build only).

## Important Files

- `AGENTS.md`: hard constraints, data rules, deployment policy, branch protection.
- `README.md`: most current doc — feature surface, auth model + scope map, full API list, compose topology, release policy.
- `docs/DEPLOYMENT_OPERATION_MANUAL.md`: release / deploy flow, mTLS cert generation, panel-only / node-only rollout.
- `docs/OPERATION_MANUAL.md`: admin/operator runbook.
- `docs/USAGE_PIPELINE_DESIGN.md`: node-agent usage pipeline invariants.
- `docs/NODE_MONTHLY_USAGE_DESIGN.md`: node natural-month RX/TX telemetry semantics.
- `docs/OPS_MONITORING_DESIGN.md`: `/ops` + `/ops-v2` realtime monitoring, metric history, probes, alerts.
- `docs/XBOARD_LEARNINGS_UPGRADE_MODULES.md`: active upgrade-module roadmap.
- `docs/USER_SYNC_MODULE_PLAN.md`: delta user-sync plan (**not yet implemented** — no `internal/usersync` package exists).
- `docs/POSTMORTEM_2026-03-28_NODE14_USAGE_DUPLICATION.md`: usage-duplication incident; its invariants are encoded in agent tests.

## Current Caveats

- API key validation exists, but there is no UI or HTTP lifecycle endpoint for creating/listing/revoking API keys (repo CRUD helpers are test-only).
- SSR templates are parsed from the CWD-relative path `internal/app/templates` via `template.Must` — the panel panics at startup unless run from the repo root (tests chdir to root; Dockerfile relies on `WORKDIR /app`). `/ops-v2` assets are likewise read from `frontend/ops-demo/dist` at request time.
- `CSRF_SECRET` is read directly via `os.Getenv` in `internal/app/app.go`, not through `internal/config`; malformed values silently degrade to an ephemeral per-process secret.
- Usage-rejection contract: `/api/v1/usage` per-event error results carry structured `code` + `permanent` fields which new agents prefer; the legacy `error` strings remain the fallback contract for old agents (`isPermanentUsageRejection`/`isTransientUsageRejection` in `internal/agent/panel_client.go`). Do not reword those panel error strings until the legacy fallback is retired.

## Backup / Restore

Routes (admin session or API key with `backups:read` / `backups:write`):

- `POST /api/v1/backups` — VACUUM INTO + gzip the live DB to `BACKUP_DIR` (default `backups/`), insert a `backups` row, return record.
- `GET  /api/v1/backups` — list recent backups.
- `GET  /api/v1/backups/{id}/download` — stream the on-disk `.sqlite.gz`.
- `DELETE /api/v1/backups/{id}` — remove the file (best-effort) and the row.
- `POST /api/v1/backups/restore` — multipart upload (`backup` field) of a `.sqlite` or `.sqlite.gz`. Validates via `PRAGMA integrity_check` plus required Neutrino tables, then stages the file as `<DBPath>.pending-restore`. **A panel restart is required to apply.**

On startup, `app.ApplyPendingRestore` snapshots the current DB to `<DBPath>.pre-restore-<ts>.bak` (also moves `-wal`/`-shm` aside) and renames the pending file into place. Invalid pending files are left on disk for inspection and the panel refuses to start.
