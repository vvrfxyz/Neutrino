# Ops Realtime + HeroUI Plan

Date: 2026-05-07

## Goal

Upgrade Neutrino's operations module from a mostly current-state monitor into a node operations center:

- Faster realtime feedback: default 2-second operations snapshots, with a supported 1-second high-frequency mode.
- Better observability: current latest metrics, historical metric samples, short-term full details, and static node facts.
- Safer active diagnostics: built-in probe jobs only, without arbitrary shell execution.
- Better node context: region, provider, cost, renewal, tags, and static asset facts.
- A new frontend direction: build a standalone HeroUI demo first, review the design, then integrate it as a separate `/ops-v2` surface.

This plan intentionally absorbs the useful parts of NodeGet's monitoring design while preserving Neutrino's existing safety model:

- Panel <-> node-agent auth remains mTLS.
- Node operations remain pull-based durable jobs.
- Job payloads remain structured and allowlisted.
- No arbitrary shell, WebShell, remote file read/write, or embedded JS worker runtime.

## External Reference Notes

NodeGet ideas to absorb:

- Split monitoring data into static facts, dynamic full data, and dynamic summary data.
- Store frequently queried summaries in a wide table.
- Store detailed variable-shape data as short-lived JSON.
- Hash static facts to deduplicate unchanged hardware/system reports.
- Use tasks/cron for active checks such as ping/TCP/HTTP probes.
- Track flexible node metadata such as region, location, cost, and renewal data.

HeroUI current direction:

- HeroUI v3 is a React component library built around Tailwind CSS v4 and React Aria Components.
- Primary package for web React usage is `@heroui/react`.
- Official docs:
  - https://heroui.com/docs/getting-started
  - https://heroui.com/docs/react/getting-started/quick-start
  - https://heroui.com/docs/react/releases/v3-0-0

## Non-Goals

- Do not replace the current `/ops` page immediately.
- Do not refactor the existing SSR templates in the first frontend pass.
- Do not introduce NodeGet-style Exec, WebShell, arbitrary config edit, arbitrary file access, or JS Worker execution.
- Do not make usage ingestion or Xray stats polling automatically follow the UI refresh rate.
- Do not turn Neutrino into a general-purpose server management panel.

## Phase 0: Realtime Baseline

Default target:

- Operations snapshot interval: 2 seconds.
- Fallback polling interval: 2 seconds.
- Panel host metric sampling interval: 2 seconds.
- Agent runtime report interval: 2 seconds.
- Optional high-frequency mode: 1 second.

Configuration:

```env
OPS_SNAPSHOT_INTERVAL_SEC=2
HOST_METRICS_INTERVAL_SEC=2
AGENT_RUNTIME_REPORT_SEC=2
```

Current 5-second or slow-refresh points to change:

- `internal/app/ws_ops_publisher.go`
  - Current `opsSnapshotInterval = 5 * time.Second`.
  - Replace hardcoded constant with app config.
- `internal/app/templates/ops.tmpl`
  - Current fallback polling interval is `5000`.
  - Replace with server-rendered config or API-provided interval.
- `internal/monitor/host.go`
  - Current invalid interval fallback is 5 seconds.
  - Allow 1-second minimum, default to configured interval.
- `internal/app/app.go`
  - Current `hostMonitor.Start(ctx, 5*time.Second, ...)`.
  - Use `cfg.HostMetricsIntervalSec`.
- `internal/agent/agent.go`
  - Current heartbeat ticker is 30 seconds.
  - Split runtime report cadence from static facts cadence.
- `.env.node.example` and node deploy template
  - Keep `ACCESS_POLL_SEC=2`.
  - Keep `STATS_POLL_SEC=5` initially.
  - Add `AGENT_RUNTIME_REPORT_SEC=2`.

Important distinction:

- Runtime observability can be 1-2 seconds.
- Usage stats and access-log polling should remain independently configured and should not be forced to 1 second by the UI.
- Static facts should stay low-frequency, for example every 5-30 minutes.

Backend optimization before high-frequency rollout:

- `OpsService.BuildNodeItems` currently calls `GetNodeJobSummary` per node.
- Add a batch query such as `ListNodeJobSummaries(ctx)` to avoid N+1 queries every 1-2 seconds.
- Keep WebSocket publishing guarded by subscriber count.
- Snapshot payload should include latest/current state only, not historical series.

Acceptance:

- `/api/v1/stream` emits `ops_snapshot` every configured interval while subscribers exist.
- Polling fallback uses the same interval.
- 2-second mode shows no visible UI staleness.
- 1-second mode is available but not the default.
- `go test ./...` passes.

## Phase 1: Monitoring Data Layering

Keep the current latest table:

- `node_runtime_metrics`
  - Single latest row per node.
  - Used by `/ops` and WebSocket snapshots.

Add historical summary samples:

```sql
CREATE TABLE IF NOT EXISTS node_metric_samples (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  sampled_at TEXT NOT NULL,
  cpu_percent REAL NOT NULL DEFAULT 0,
  load1 REAL NOT NULL DEFAULT 0,
  load5 REAL NOT NULL DEFAULT 0,
  load15 REAL NOT NULL DEFAULT 0,
  memory_used_bytes INTEGER NOT NULL DEFAULT 0,
  memory_total_bytes INTEGER NOT NULL DEFAULT 0,
  memory_available_bytes INTEGER NOT NULL DEFAULT 0,
  swap_used_bytes INTEGER NOT NULL DEFAULT 0,
  swap_total_bytes INTEGER NOT NULL DEFAULT 0,
  disk_total_bytes INTEGER NOT NULL DEFAULT 0,
  disk_used_bytes INTEGER NOT NULL DEFAULT 0,
  disk_free_bytes INTEGER NOT NULL DEFAULT 0,
  disk_used_percent REAL NOT NULL DEFAULT 0,
  disk_read_bps REAL NOT NULL DEFAULT 0,
  disk_write_bps REAL NOT NULL DEFAULT 0,
  inbound_bps REAL NOT NULL DEFAULT 0,
  outbound_bps REAL NOT NULL DEFAULT 0,
  tcp_connections INTEGER NOT NULL DEFAULT 0,
  udp_connections INTEGER NOT NULL DEFAULT 0,
  process_count INTEGER NOT NULL DEFAULT 0,
  system_uptime_sec INTEGER NOT NULL DEFAULT 0,
  agent_uptime_sec INTEGER NOT NULL DEFAULT 0,
  boot_time TEXT,
  queue_bytes INTEGER NOT NULL DEFAULT 0,
  queue_batches INTEGER NOT NULL DEFAULT 0,
  goroutines INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_node_metric_samples_node_time
ON node_metric_samples(node_id, sampled_at DESC);
```

Add short-lived details:

```sql
CREATE TABLE IF NOT EXISTS node_metric_details (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  sampled_at TEXT NOT NULL,
  data_json TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_node_metric_details_node_time
ON node_metric_details(node_id, sampled_at DESC);
```

Detail JSON may include:

- Per-disk usage and read/write speed.
- Per-interface RX/TX totals and speed.
- Per-core CPU usage and frequency.
- Optional GPU facts in the future.

Write path:

- `ApplyNodeReport` continues to upsert `node_runtime_metrics`.
- It also inserts a `node_metric_samples` row.
- If details are present, it inserts into `node_metric_details`.

Failure handling:

- Latest metrics write failure should fail the report.
- Historical sample/detail failure may log and continue if latest succeeded. This prevents historical storage from breaking the control plane.

Acceptance:

- A node runtime report updates latest metrics and inserts a sample.
- Existing `/ops` continues to work from latest metrics.
- Sample queries are indexed by node and time.

## Phase 2: Agent Runtime Metrics Expansion

Extend the agent runtime metrics payload with:

- Load averages: 1/5/15.
- Memory: total, available, used.
- Swap: total, used.
- System uptime and boot time.
- Agent uptime as a separate field.
- Process count.
- TCP/UDP connection counts.
- Disk read/write bytes per second.
- Per-disk detail summary.
- Per-interface detail summary.
- Agent version.
- Xray version or pinned image/version marker.
- Applied config version markers where useful.

Implementation targets:

- `internal/agent/agent.go`
  - Split `heartbeatMetrics` into smaller collectors where useful.
  - Avoid blocking runtime report on a single failed collector.
- `internal/agent/panel_client.go`
  - Extend `NodeReportMetrics`.
- `internal/repo/node_report.go`
  - Extend `NodeReportMetricsInput`.
- `internal/repo/node_runtime_metrics.go`
  - Store expanded latest fields.

Naming cleanup:

- Current `UptimeSec` represents agent uptime in practice.
- Add `AgentUptimeSec`.
- Add `SystemUptimeSec`.
- Keep old `UptimeSec` during transition if needed, but prefer explicit names in new APIs.

Cadence:

- Runtime metrics: default 2 seconds.
- Static facts: 5-30 minutes.
- Usage stats: keep `STATS_POLL_SEC=5` initially.
- Access logs: keep `ACCESS_POLL_SEC=2`.

Acceptance:

- Old nodes that report only the old fields still render.
- New nodes report expanded fields.
- Values are clamped and normalized.
- Collector failures do not crash the agent.

## Phase 3: Static Node Facts

Add static facts deduplicated by hash:

```sql
CREATE TABLE IF NOT EXISTS node_static_facts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  reported_at TEXT NOT NULL,
  data_hash TEXT NOT NULL,
  os_name TEXT NOT NULL DEFAULT '',
  os_version TEXT NOT NULL DEFAULT '',
  kernel TEXT NOT NULL DEFAULT '',
  kernel_version TEXT NOT NULL DEFAULT '',
  arch TEXT NOT NULL DEFAULT '',
  hostname TEXT NOT NULL DEFAULT '',
  virtualization TEXT NOT NULL DEFAULT '',
  cpu_model TEXT NOT NULL DEFAULT '',
  cpu_physical_cores INTEGER NOT NULL DEFAULT 0,
  cpu_logical_cores INTEGER NOT NULL DEFAULT 0,
  agent_version TEXT NOT NULL DEFAULT '',
  xray_version TEXT NOT NULL DEFAULT '',
  facts_json TEXT NOT NULL DEFAULT '{}',
  UNIQUE(node_id, data_hash)
);

CREATE INDEX IF NOT EXISTS idx_node_static_facts_node_reported
ON node_static_facts(node_id, reported_at DESC);
```

Hash input:

- OS and kernel facts.
- Architecture.
- Hostname.
- Virtualization marker.
- CPU model/core counts.
- Agent version.
- Xray version.
- Optional GPU/static device facts later.

UI usage:

- `/ops-v2` node detail drawer displays latest static facts.
- Existing `/nodes/{id}/deploy` can later show full static facts.
- Version mismatch or unknown version can be surfaced as an operations warning.

Acceptance:

- Same facts do not create duplicate rows.
- Changed facts create a new row.
- Node deletion cascades facts.

## Phase 4: Historical Metrics APIs

Add read APIs:

```http
GET /api/v1/nodes/{id}/metrics?range=1h|6h|24h|7d&step=raw|1m|5m|1h
GET /api/v1/nodes/{id}/metric-details/latest
GET /api/v1/nodes/{id}/static-facts/latest
GET /api/v1/nodes/{id}/static-facts/history
```

Aggregation:

- CPU avg/max.
- Memory used avg/max.
- Disk used percent avg/max.
- Inbound/outbound avg/max.
- TCP/UDP avg/max.
- Queue max.
- Sample count.

Permissions:

- Use `metrics:read` or fold under existing `nodes:read` only if the project wants fewer scopes.
- Prefer `metrics:read` for clarity as the surface grows.

Acceptance:

- Range and step are strict allowlists.
- Empty ranges return empty series.
- Queries are bounded and indexed.
- API output is stable for frontend demo and `/ops-v2`.

## Phase 5: Standalone HeroUI Demo

Do not modify the current SSR frontend yet.

Create:

```text
frontend/ops-demo/
```

Suggested stack:

- React.
- TypeScript.
- Vite.
- HeroUI.
- Tailwind CSS v4.
- Recharts or uPlot for charts.

Data adapters:

- `mockAdapter`
  - Fixed JSON fixtures for design review.
  - Must include healthy, stale, disabled, error, drift, empty, and loading states.
- `liveAdapter`
  - Optional local connection to existing APIs.
  - WebSocket: `/api/v1/stream`.
  - Polling fallback: `/api/v1/ops/nodes`, `/api/v1/metrics/host`, `/api/v1/online-users`.
  - Later: historical node metrics and static facts APIs.

Demo routes/views:

- Operations overview.
- Node grid/list.
- Node detail drawer.
- Historical metric charts.
- Online users.
- Node jobs and version drift.
- Probe results.
- Alerts.
- Node metadata and cost area.

Design rules:

- This is an operational tool, not a landing page.
- Prioritize dense but calm scanning.
- Avoid decorative hero sections.
- Use icons and familiar controls for actions.
- Keep cards for repeated node items and drawers/modals, not nested card stacks.
- Ensure long node names, long IPs, and long errors do not break layout.
- Text must not overlap in desktop or mobile.
- 2-second updates must not cause layout shift.

Acceptance:

- Demo builds independently.
- Desktop and mobile screenshots pass visual review.
- Loading/empty/error/stale states are designed, not improvised.
- Design review approves before Go app integration begins.

## Phase 6: HeroUI Design Review Gate

Review checklist:

- The first viewport immediately communicates node fleet health.
- Current status, historical trend, job state, and alerts have clear visual hierarchy.
- Online/stale/disabled/error/drift states are visually distinct.
- 2-second updates are smooth and do not move layout around.
- Mobile layout remains usable.
- Node detail drawer is more useful than cramming all details into cards.
- No feature-instruction text is baked into the app UI.
- No arbitrary marketing/hero page exists.

Tooling:

- Use Playwright to capture desktop and mobile screenshots.
- Check canvas/chart rendering for nonblank charts if chart canvas is used.
- Verify text overflow and no incoherent overlap.

## Phase 7: Integrate as `/ops-v2`

Only after the standalone demo passes review:

- Build the HeroUI demo as static assets.
- Add a Go route for `/ops-v2`.
- Keep `/ops` unchanged.
- Add feature flag:

```env
ENABLE_OPS_V2=true
```

Integration targets:

- Existing WebSocket stream.
- Existing polling APIs.
- New metrics history APIs.
- New static facts APIs.
- Later: probe, alert, and metadata APIs.

Migration strategy:

- `/ops` remains the stable fallback.
- `/ops-v2` is the new preview surface.
- Once stable in production, decide whether `/ops-v2` replaces `/ops`.

Acceptance:

- `/ops` still renders.
- `/ops-v2` renders with real data.
- WebSocket and polling fallback both work.
- Feature flag can disable `/ops-v2`.

## Phase 8: Safe Active Probes

Absorb NodeGet's active check idea, but only as safe built-in probes.

New job kinds:

```text
probe_ping
probe_tcp
probe_http
```

Explicitly forbidden:

- Shell.
- WebShell.
- Arbitrary executable paths.
- Arbitrary file reads/writes.
- Remote config editing.

Payload examples:

```json
{
  "target": "example.com",
  "port": 443,
  "timeout_ms": 3000,
  "count": 3
}
```

```json
{
  "url": "https://example.com/healthz",
  "method": "GET",
  "timeout_ms": 3000,
  "expect_status": [200, 204]
}
```

Result table:

```sql
CREATE TABLE IF NOT EXISTS node_probe_results (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  target TEXT NOT NULL,
  success INTEGER NOT NULL DEFAULT 0,
  latency_ms REAL NOT NULL DEFAULT 0,
  status_code INTEGER,
  error TEXT NOT NULL DEFAULT '',
  checked_at TEXT NOT NULL,
  source_job_id INTEGER REFERENCES node_jobs(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_node_probe_results_node_time
ON node_probe_results(node_id, checked_at DESC);
```

Acceptance:

- Probe job payloads are strictly validated.
- Timeout is enforced.
- Agent executes only built-in Go functions.
- Results are persisted and auditable.

## Phase 9: Node Metadata And Cost

Use controlled schema rather than a generic KV system.

Add:

```sql
CREATE TABLE IF NOT EXISTS node_metadata (
  node_id INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
  provider TEXT NOT NULL DEFAULT '',
  region TEXT NOT NULL DEFAULT '',
  country TEXT NOT NULL DEFAULT '',
  city TEXT NOT NULL DEFAULT '',
  latitude REAL,
  longitude REAL,
  tags_json TEXT NOT NULL DEFAULT '[]',
  monthly_cost_cents INTEGER NOT NULL DEFAULT 0,
  currency TEXT NOT NULL DEFAULT 'USD',
  renew_cycle TEXT NOT NULL DEFAULT '',
  renew_at TEXT,
  note TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);
```

UI:

- `/ops-v2` node detail drawer shows provider, region, tags, renewal, and cost.
- `/nodes` can later expose editing controls.
- Add renewal-due alerting after alerts exist.

Acceptance:

- Tags and notes are length-limited.
- Costs are integer cents.
- Metadata does not affect subscription rendering fields.

## Phase 10: Alerts

Add durable operational alerts:

```sql
CREATE TABLE IF NOT EXISTS ops_alerts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  node_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  severity TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('active','resolved')),
  message TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT NOT NULL,
  first_seen_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  resolved_at TEXT,
  UNIQUE(dedupe_key, status)
);

CREATE INDEX IF NOT EXISTS idx_ops_alerts_status_seen
ON ops_alerts(status, last_seen_at DESC);
```

Alert kinds:

- `node_stale`
- `agent_metrics_missing`
- `disk_high`
- `cpu_high`
- `memory_high`
- `queue_backlog`
- `job_stuck`
- `version_drift`
- `probe_failed`
- `renew_due`
- `xray_apply_failed`

Rules:

- Alert creation and notification are decoupled.
- Dedupe by stable key.
- Recovery resolves active alert.
- Telegram notification can reuse the existing notifier/worker model.

Acceptance:

- Repeated failures update one active alert.
- Recovery resolves the alert.
- Notification failure does not block alert state updates.

## Phase 11: Retention And Cleanup

Add configurable retention:

```env
NODE_METRIC_SAMPLE_RETENTION_DAYS=14
NODE_METRIC_DETAIL_RETENTION_HOURS=72
NODE_PROBE_RESULT_RETENTION_DAYS=30
OPS_ALERT_RESOLVED_RETENTION_DAYS=90
```

Retention policy:

- Summary samples: 14 days by default.
- Full detail JSON: 72 hours by default.
- Probe results: 30 days by default.
- Resolved alerts: 90 days by default.
- Static facts: keep indefinitely until node deletion.

Acceptance:

- Cleanup uses indexed timestamp columns.
- Worker logs deleted counts.
- Cleanup never blocks request paths.

## Phase 12: Testing And Delivery Gates

Backend tests:

- Config parsing for new intervals.
- 1-second minimum handling.
- Batch node job summary query.
- Runtime metrics normalization and clamping.
- Sample insertion.
- Static facts hash dedupe.
- Metrics API range/step validation.
- Probe payload validation.
- Alert dedupe and resolve.
- Retention cleanup.

Frontend demo tests:

- Build test.
- Rendering tests for major states.
- Playwright screenshots for desktop and mobile.
- WebSocket reconnect and polling fallback behavior.

Required commands:

```bash
rtk go test ./...
```

For the standalone frontend once created:

```bash
npm run build
npm run test
```

Manual smoke:

- Login.
- Open `/ops`.
- Open `/ops-v2` when enabled.
- Confirm 2-second WebSocket refresh.
- Confirm 2-second polling fallback.
- Confirm latest metrics, historical charts, static facts, and online users render.
- Create a safe probe job and view result.
- Trigger and resolve a stale-node alert.

## Recommended Implementation Order

1. Add interval config and switch `/ops` snapshot, polling fallback, host monitor, and agent runtime report to 2 seconds by default.
2. Remove high-frequency N+1 query risk by batching node job summaries.
3. Create standalone `frontend/ops-demo` with HeroUI, mock data, and the target operations layout.
4. Review and iterate on the demo until the design passes.
5. Add metric samples, metric details, and static facts storage.
6. Extend agent runtime metrics and static fact reports.
7. Add historical metrics and static facts APIs.
8. Integrate approved demo as `/ops-v2` behind `ENABLE_OPS_V2`.
9. Add safe probe jobs and results UI.
10. Add node metadata/cost fields and UI.
11. Add alerts and notifications.
12. Add retention cleanup and documentation updates.

## First Milestone

The first milestone should be deliberately small:

- `OPS_SNAPSHOT_INTERVAL_SEC=2`
- `HOST_METRICS_INTERVAL_SEC=2`
- `AGENT_RUNTIME_REPORT_SEC=2`
- Batch job summaries for `/ops`.
- Keep old `/ops` working.
- Create `frontend/ops-demo` with mock HeroUI screens.

This gives fast visible progress without committing the existing frontend to the new design before it is ready.
