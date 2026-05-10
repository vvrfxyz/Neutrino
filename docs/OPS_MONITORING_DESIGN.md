# Ops Monitoring Design

## Purpose

This document records the current Neutrino operations monitoring design. It
replaces older implementation plans for the realtime `/ops` work, the optional
`/ops-v2` preview, and NodeGet-inspired monitoring improvements.

## Current Surfaces

- `/ops`: stable SSR/Alpine operations dashboard.
- `/ops-v2`: optional preview frontend served from `frontend/ops-demo/dist`
  when `ENABLE_OPS_V2=true`.
- `/api/v1/stream`: admin-session WebSocket that publishes `ops_snapshot`.
- Polling APIs:
  - `GET /api/v1/ops/config`
  - `GET /api/v1/ops/nodes`
  - `GET /api/v1/ops/alerts`
  - `GET /api/v1/metrics/host`
  - `GET /api/v1/online-users`
  - `GET /api/v1/nodes/{id}/metrics`
  - `GET /api/v1/nodes/{id}/probe-results`

## Data Model

Latest state:

- `node_runtime_metrics`: one latest runtime row per node.
- `node_monthly_usage`: latest natural-month node host RX/TX view.
- `node_static_facts`: deduplicated static node facts with canonical hash.
- `online_sessions`: online user/client IP read model.
- `ops_alerts`: active and resolved operational alerts.

Historical state:

- `node_metric_samples`: summary runtime samples for charts.
- `node_metric_details`: short-lived detailed JSON payloads.
- `node_probe_results`: active diagnostic probe results.

## Runtime Report Path

Node-agent reports runtime state through the existing node report endpoint:

```text
node-agent runtime sampler
  -> POST /api/v1/nodes/{id}/report over mTLS
  -> latest state updated synchronously
  -> historical sample/detail enqueued asynchronously
  -> /ops and /api/v1/stream consume latest state
```

The report path separates correctness-critical latest state from optional
history:

- Latest writes are synchronous and may fail the report.
- Historical `node_metric_samples` / `node_metric_details` writes go through
  the panel-side metric history queue.
- If the memory queue is full, the panel writes backlog files to the configured
  disk queue.
- If both memory and disk queue are unavailable, history is dropped and a
  `metric_history_dropped` ops alert is emitted.

## Realtime Snapshot Path

The operations WebSocket publisher runs on `OPS_SNAPSHOT_INTERVAL_SEC`
(default `2` seconds).

Behavior:

- No subscribers means no snapshot work.
- Host latest values come from the in-memory host monitor.
- Online users come from `online_sessions`.
- Node cards come from the ops service/latest cache.
- Alerts come from `ops_alerts`.
- Polling endpoints expose equivalent data for reconnect/fallback behavior.

## Latest Cache

`OpsLatestCache` reduces repeated `/ops` and WebSocket read amplification.

Refresh sources:

- node report
- node job claim/finish
- node metadata changes
- node changes/removal
- app warmup from DB

The cache is an optimization only; the durable source of truth remains SQLite.

## Static Facts

Static node facts are low-frequency reports. Canonical JSON hashing prevents
duplicate rows when facts are semantically unchanged but key order or whitespace
differs.

Examples:

- OS name/version
- kernel/version
- architecture
- hostname
- virtualization
- CPU model/core counts
- agent and Xray versions
- optional facts JSON

## Metric History

Metric history is intentionally split:

- `node_metric_samples` keeps chart-friendly summary fields.
- `node_metric_details` stores short-lived variable-shape details.

Current default retention:

- samples: `NODE_METRIC_SAMPLE_RETENTION_DAYS=14`
- details: `NODE_METRIC_DETAIL_RETENTION_HOURS=72`

Querying uses the existing sample table with bucket aggregation. No rollup table
is currently required.

## Active Probes

Probe jobs are structured `node_jobs`; they do not execute shell commands.

Supported kinds:

- `probe_dns`
- `probe_tcp`
- `probe_http`
- `probe_ping` as a legacy alias for DNS behavior

Safety rules:

- payloads are parsed and validated by `internal/probe`;
- localhost, link-local, cloud metadata, and private targets are denied unless
  the request explicitly allows private addresses;
- HTTP probes restrict method, redirect behavior, body size, and timeout;
- probe jobs use target/correlation dedupe;
- critical control jobs keep priority over probe jobs.

Results are written to `node_probe_results`. Failed probes can create
`probe_failed` ops alerts; recovered probes resolve them.

## Configuration

Panel:

```env
OPS_SNAPSHOT_INTERVAL_SEC=2
HOST_METRICS_INTERVAL_SEC=2
NODE_METRIC_HISTORY_QUEUE_CAPACITY=4096
NODE_METRIC_HISTORY_QUEUE_DIR=
NODE_METRIC_HISTORY_QUEUE_MAX_BYTES=67108864
NODE_METRIC_SAMPLE_RETENTION_DAYS=14
NODE_METRIC_DETAIL_RETENTION_HOURS=72
NODE_PROBE_RESULT_RETENTION_DAYS=30
OPS_ALERT_RESOLVED_RETENTION_DAYS=90
ENABLE_OPS_V2=false
```

Node-agent:

```env
AGENT_RUNTIME_REPORT_SEC=2
AGENT_STATIC_FACTS_REPORT_SEC=1800
```

Usage polling and access-log polling remain independently configured and should
not be tied to UI refresh frequency.

## Security Boundaries

- Panel to node-agent control stays on mTLS.
- Node operations use pull-based durable jobs.
- Job payloads are structured and allowlisted.
- Managed Xray operations execute only node-local configured argv.
- No WebShell, arbitrary shell, arbitrary file read/write, or remote JS worker
  execution is part of the ops design.

## Operational Checks

- `/ops` should show host metrics, online users, node runtime, queue state,
  alerts, and node natural-month RX/TX.
- `/api/v1/stream` should emit `ops_snapshot` while the dashboard is connected.
- `/api/v1/ops/nodes` should reflect node report/job/metadata changes without
  waiting for a full cache rebuild.
- `node_metric_history_dropped` alerts indicate historical monitoring loss, not
  node liveness failure.
- Probe failures should be visible in `node_probe_results` and `ops_alerts`.
