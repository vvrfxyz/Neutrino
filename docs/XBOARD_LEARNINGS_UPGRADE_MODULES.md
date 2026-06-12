# Xboard Learning Upgrade Modules

This document tracks the upgrade modules inspired by Xboard and Xboard-Node.
The goal is to learn useful product and operations patterns without changing
Neutrino's core safety model.

## Guardrails

- Keep panel to node-agent communication on mTLS.
- Keep the pull-based durable `node_jobs` model.
- Keep usage accounting idempotent by `source + source_event_id`.
- Keep the agent usage pipeline flush-before-sample.
- Do not adopt token-auth WebSocket control as the core control plane.
- Do not add a full plugin marketplace, payment system, or order system.
- Do not copy Xboard-Node code directly; learn designs and reimplement only
  what fits Neutrino.

## Upgrade Modules

### 1. Subscription Enhancement

Make `/sub/{token}` more client-aware and easier to extend.

Scope:

- Renderer registry instead of a monolithic target switch.
- Target aliases such as `mihomo` for Clash.Meta/Mihomo output.
- `flag`, `types`, and simple keyword `filter` query parameters.
- Minimal client capability filtering.
- Later: safe built-in profile presets.

### 2. Online Status

Make node-agent obtain the real current online IP set first, then make the
panel reflect that authoritative runtime snapshot.

Detailed implementation plan: [Online Status Module Plan](ONLINE_STATUS_MODULE_PLAN.md).

Scope:

- Keep `online_sessions` as the single read model.
- Use Xray's online stats API from node-agent when available
  (`user>>>EMAIL>>>online` / online IP list).
- Report a per-node authoritative online snapshot to the panel.
- Apply successful snapshots transactionally: upsert present IPs and expire
  stale IPs for the reporting node.
- Do not clear existing online rows when snapshot collection or reporting
  fails.
- Do not use access logs as an online-status fallback; snapshot failures should
  be visible so logic/configuration errors can be found and fixed.

### 3. User Sync

Optimize `users_sync` while keeping full sync as the authoritative repair path.

Detailed implementation plan: [User Sync Module Plan](USER_SYNC_MODULE_PLAN.md).

Scope:

- Preserve current full sync behavior.
- Add safe delta sync only when node versions match.
- Fall back to full sync on any version mismatch or delta apply failure.
- Keep periodic reconciler full-sync capable.

### 4. Managed Xray Config Enhancement

Make managed Xray more configurable without opening unsafe execution surfaces.

Scope:

- Structured `custom_routes` and `custom_outbounds` under node `extra_json`.
- Schema validation and preview before runtime use.
- Controlled rendering into managed Xray config.
- No arbitrary shell, arbitrary file paths, or secret material from panel.

### 5. Node Operations CLI

Add a small `neutrinoctl` for node-side diagnostics and recovery.

Scope:

- Commands such as `status`, `queue`, `cert`, `test-xray`, `apply-preview`,
  `rollback`, and `enroll-info`.
- Read local state and call existing fixed logic only.
- Do not implement an arbitrary shell proxy.

### 6. Kernel Boundary

Prepare the codebase for future kernel support without adding a second kernel
yet.

Scope:

- Extract small interfaces such as `UserApplier`, `TrafficSampler`, and
  `ConfigApplier`.
- Keep current Xray behavior unchanged.
- Treat sing-box support as a later decision after the Xray path is stable.

## Recommended Order

1. Subscription Enhancement. — **done** (renderer registry + capability sets, `mihomo`/`clashmeta`/`stash` aliases, `flag`/`types`/`filter` query params; profile presets remain future work)
2. Online Status. — **done**
3. User Sync. — **done** (`internal/usersync`, PR #10)
4. Managed Xray Config Enhancement. — **done** (`internal/xraycfg` + deploy-page editors/preview, PR #11)
5. Node Operations CLI. — **done** (`cmd/neutrinoctl`, PR #11)
6. Kernel Boundary. — **done** (`internal/agent/kernel.go`: UserApplier / TrafficSampler / UptimeProber / RuntimeClient / ConfigApplier)

Each module should be implemented as independently testable steps, with focused
tests and explicit failure behavior. Do not add fallback paths that hide broken
authoritative behavior.
