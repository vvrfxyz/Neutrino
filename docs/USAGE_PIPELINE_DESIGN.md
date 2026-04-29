# Usage Pipeline Design

## Purpose

The node-agent usage pipeline exists to transform local Xray observations into
durable, idempotent panel-side accounting without double counting or silently
dropping usage.

This document defines the invariants the implementation must preserve.

## Core invariants

### 1. Flush-before-sample

The agent **must flush older queued usage before sampling any fresh usage**.

Rationale:

- queued batches represent an unfinished accounting boundary;
- generating a newer stats snapshot while an older stats batch is still pending
  can cause overlapping deltas;
- once the panel becomes reachable again, overlapping deltas can be accepted as
  distinct `source_event_id`s and inflate user traffic.

Implementation rule:

- if the queue still contains any pending batch after the flush phase, the agent
  must skip fresh batch generation for that tick.

### 2. Empty-queue generation may produce both batch kinds

When the queue is empty and polling intervals are due, the agent may enqueue:

- one `access` batch, and
- one `stats` batch

in the same tick.

Rationale:

- preserving the empty-queue precondition keeps accounting safe;
- generating only one kind per tick lets frequent access batches starve stats on
  busy nodes.

### 3. Stats acknowledgements must match what was actually emitted

`AckedStats` is local state used to compute future deltas. It must advance only
for counters already represented by a successfully accepted batch, plus
zero-delta counter resets that require no panel-side event.

This is especially important when `PUSH_BATCH_MAX_EVENTS` truncates a batch:

- emitted directions/users may advance;
- non-emitted directions/users must **not** advance.

Otherwise the agent can silently lose unreported usage.

### 4. Counter resets require epoch separation

When a current Xray counter is lower than the last acknowledged counter, the
agent must treat that as a counter reset and bump the stats epoch used inside
`source_event_id`.

This prevents collisions between pre-reset and post-reset stats events.

### 5. Zero-delta resets are bookkeeping-only

If a counter reset produces zero delta (for example, old counter `N`, current
counter `0`), no panel event is required, but local acknowledgement state still
must move to the reset value so the agent does not keep re-bumping epochs on
future ticks.

## Failure model

The pipeline assumes:

- queue persistence is durable enough to survive process restarts;
- panel-side usage ingest is idempotent on `(source, source_event_id)`;
- the agent only advances local acknowledgements after a queued batch has been
  accepted.

## Operational signals

When debugging node-side usage accounting, check:

1. queue depth / queue bytes;
2. current `state.json` `acked_stats`;
3. whether recent `xray-stats` deltas match the difference implied by
   `source_event_id` counters;
4. whether the queue stayed non-empty during panel-side failures.

## Non-goals

This document does not define:

- panel-side quota policy;
- access-log parsing details unrelated to durability/idempotency;
- node job execution semantics.
