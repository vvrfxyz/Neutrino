# Node Monthly Usage Design

## Purpose

The `/ops` page needs a node-level answer to:

- how much upload (`TX`) the node has used in its current natural month;
- how much download (`RX`) the node has used in its current natural month.

This signal is **operational node telemetry**, not user accounting.

It exists alongside, not instead of:

- panel host monthly traffic, and
- user quota / usage accounting from `POST /api/v1/usage`.

Node-side month keys are computed in the agent's month timezone:
`AGENT_MONTH_TZ` when set, otherwise `time.Local`, otherwise UTC (with a
one-time warning). `AGENT_ACCESS_LOG_TZ` is **not** reused here. Panel-side
month keys (host-net rollup in `host_net_monthly_usage` and `/ops` display)
are computed using `PANEL_TZ` (default `UTC`). The two pipelines are
independent and may use different timezones without affecting each other.

## Semantics

The node monthly usage probe reports:

- OS-level network totals aggregated on the node host;
- natural-month accumulation in the node's local timezone;
- separate `RX` and `TX` counters.

It does **not** represent:

- per-user usage,
- quota-window usage,
- Xray-only traffic,
- billable traffic after any business logic adjustment.

## Why the panel computes month totals

The implementation intentionally keeps the month accumulator on the panel. The
node-agent reports raw host network counters, and the panel owns the per-node
month baseline / delta logic.

Reasons:

1. The agent is closest to the monotonic OS counters, but it should stay a
   stateless probe for month accounting.
2. Panel-side state survives agent restarts, agent image upgrades, and loss of
   the agent's local `state.json`.
3. Panel can still detect raw counter resets or counter-source changes and
   rebase without subtracting already accumulated month totals.

## Data flow

1. On every heartbeat tick, node-agent reads OS network totals.
2. The agent sends a heartbeat report containing:
   - `reported_at`,
   - runtime metrics,
   - `month_key`,
   - `month_timezone`,
   - `net_rx_total_bytes`,
   - `net_tx_total_bytes`,
   - `net_counter_source`.
3. Panel advances a persisted month accumulator:
   - new month: reset month totals and store the raw counter baseline;
   - same month with increasing totals: add positive deltas;
   - counter reset or counter-source change inside the month: move the baseline
     forward without subtracting from already accumulated month totals.
4. Panel stores:
   - `nodes.last_seen_at`: **panel receive time** for liveness / health;
   - `node_runtime_metrics.updated_at`: normalized agent `reported_at`;
   - `node_monthly_usage(node_id, month_key)`: cumulative month totals plus raw
     counter baseline / last totals.

## Time semantics

These timestamps must stay distinct:

- `last_seen_at`
  - meaning: when panel actually received node contact;
  - use: online / stale health judgment.
- `agent_metrics_at`
  - meaning: when the node probe sampled and reported runtime metrics.
- `node_monthly_usage.last_reported_at`
  - meaning: when the reported month totals were sampled on the node.

This separation is intentional. Liveness must not depend on node clock quality.

## Persistence model

### Agent side

`state.json` is not authoritative for node-month traffic. The agent only keeps
normal usage-pipeline cursors there.

### Panel side

`node_monthly_usage` stores the latest known month totals keyed by
`(node_id, month_key)`.

For the same node and month, panel keeps cumulative values monotonic:

- new raw totals above the previous raw totals add positive deltas;
- raw total decreases are treated as host reboot / counter reset and only move
  the baseline;
- counter source changes are treated as a rebase, not as additional traffic;
- out-of-order reports are ignored.

## Failure model and limits

This design is resilient to:

- panel restarts,
- panel network outages,
- node-agent restarts,
- node-agent local state loss,
- node reboot / raw counter reset.

This design cannot fully recover from:

- panel database loss in the middle of a month.

If the panel database loses the current month accumulator state mid-month, the
next raw counter sample can establish a new baseline, but the panel cannot
reconstruct the missing portion without an external source.

## UI guidance

On `/ops`:

- the top KPI "panel monthly traffic" is panel-host telemetry;
- each node card's "natural month total" is node-host telemetry;
- `month_key + timezone` should be shown next to the node total to avoid
  timezone ambiguity.

## Non-goals

This design does not attempt to:

- replace `traffic_events` / quota accounting;
- derive user traffic from node host totals;
- compute exact Xray-only NIC usage per protocol / inbound.
