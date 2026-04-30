# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

For authoritative constraints and runtime decisions, see `AGENTS.md`.

## Common Commands

```bash
# Run the panel locally
go run ./cmd/server

# Run the node-agent locally
go run ./cmd/node-agent

# Run the full test suite
go test ./...

# Run a single package
go test ./internal/app

# Run a single test
go test ./internal/app -run TestName
```

## Architecture Overview

- Two Go entrypoints:
  - `cmd/server`: the control plane / admin panel. It opens SQLite, runs migrations, seeds the admin credential, serves the SSR admin UI and `/api/v1/*`, and optionally starts a dedicated agent mTLS listener.
  - `cmd/node-agent`: the per-node agent. It enrolls with the panel, maintains mTLS credentials, talks to local Xray, reports usage/runtime, and executes node jobs.
- `internal/app` is the panel orchestration layer:
  - `routes.go` defines the public/admin surface and the separate agent-only mTLS surface.
  - SSR UI pages are currently `/users`, `/users/{id}`, `/nodes`, `/nodes/{id}/deploy`, `/traffic`, `/enforcements`, `/ops`, plus `/login`.
  - `api_v1.go` handles user CRUD, user traffic/events, subscription and Telegram-bind endpoints, node CRUD, usage ingest, traffic summary, online users, and host metrics.
  - `handlers_nodes.go` handles node report, enroll, job claim/finish, cert revoke/renew, and managed Xray deploy/rollback endpoints.
  - `handlers_node_deploy_page.go` builds the node deployment page and the one-click bootstrap script that auto-checks / installs `docker compose` when needed.
  - `StartWorkers` runs host metrics, host monthly network rollups, Telegram polling, quota/expiry/IP-limit enforcement, pruning, and node reconciliation / timeout sweeping.
- `internal/repo` is the domain core:
  - SQLite persistence, auth/session state, usage ingestion, quota windows, IP-limit enforcement, node desired/applied state, durable node jobs, audit logs, enforcement logs, subscription tokens, Telegram bindings, API key validation, and operational aggregates.
- Panel-to-node sync is versioned and pull-based:
  - The panel stores desired state in DB.
  - It enqueues `node_jobs` (`users_sync`, `xray_apply`, `xray_rollback`).
  - Node-agents long-poll claim/finish those jobs over the mTLS listener.
  - The panel never shells into nodes directly.
- `internal/agent` contains node runtime behavior:
  - enrollment + certificate renewal,
  - disk-backed queue/state for resilient usage delivery,
  - Xray stats/access-log collection,
  - runtime report submission (including node-local natural-month RX/TX probe totals),
  - managed Xray bootstrap/apply/rollback using agent-local argv only,
  - REALITY key auto-generation / persistence when env values are placeholders.
- Subscription rendering is centralized in `internal/subscription`; `/sub/{token}` renders client-specific formats from the active link plus enabled nodes (or fallback proxy env config when no nodes are enabled).
- Managed Xray templates live in `internal/templates`; panel-side code renders templates + vars only and never ships arbitrary shell commands or file paths.

## Operational Model

- Admin auth is session-based by default; Basic Auth is an optional fallback via `ALLOW_BASIC_AUTH=true`, but only when the admin credential is no longer the default one.
- `/api/v1/*` supports admin auth and pre-provisioned API keys. Scope mapping lives in `internal/app/auth.go`.
- Node-agent control-plane auth is mTLS only. The dedicated mTLS listener serves usage ingest, node report, job claim/finish, and cert renew.
- Usage ingestion is idempotent by `source + source_event_id`; quota, expiry, and IP-limit enforcement can deactivate users and trigger downstream user resync.
- Node certs are checked both by CA trust and by per-node allowlist / pin validation, so single-node revoke does not require rotating the whole CA set.

## Testing Notes

- Unit tests are colocated with packages.
- Functional coverage lives in `tests/functional` and covers both the normal HTTP server and the agent-side mTLS control plane.
- Relevant integration-style areas already covered include user lifecycle, user detail traffic endpoints, subscription / Telegram bind flow, node deploy page behavior, and agent cert renew / job handling.

## Important Files

- `AGENTS.md`: hard constraints, deployment policy, runtime decisions.
- `README.md`: current feature surface, auth model, API list, compose topology, and docs index.
- `docs/DEPLOYMENT_OPERATION_MANUAL.md`: release / deploy flow, mTLS cert generation, panel-only / node-only rollout.
- `docs/OPERATION_MANUAL.md`: admin/operator runbook for `/users`, `/nodes`, `/traffic`, `/enforcements`, `/ops`, and Telegram.
- `docs/USAGE_PIPELINE_DESIGN.md`: invariants for the node-agent usage pipeline.
- `docs/NODE_MONTHLY_USAGE_DESIGN.md`: semantics and failure model for node natural-month RX/TX telemetry.

## Current Caveats

- API key validation exists, but there is currently no UI or HTTP lifecycle management endpoint for creating / listing / revoking API keys.

## Backup / Restore

Routes (admin session or API key with `backups:read` / `backups:write`):

- `POST /api/v1/backups` — VACUUM INTO + gzip the live DB to `BACKUP_DIR` (default `backups/`), insert a `backups` row, return record.
- `GET  /api/v1/backups` — list recent backups.
- `GET  /api/v1/backups/{id}/download` — stream the on-disk `.sqlite.gz`.
- `DELETE /api/v1/backups/{id}` — remove the file (best-effort) and the row.
- `POST /api/v1/backups/restore` — multipart upload (`backup` field) of a `.sqlite` or `.sqlite.gz`. Validates via `PRAGMA integrity_check` plus required Neutrino tables, then stages the file as `<DBPath>.pending-restore`. **A panel restart is required to apply.**

On startup, `app.ApplyPendingRestore` snapshots the current DB to `<DBPath>.pre-restore-<ts>.bak` (also moves `-wal`/`-shm` aside) and renames the pending file into place. Invalid pending files are left on disk for inspection and the panel refuses to start.
