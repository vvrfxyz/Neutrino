# Postmortem: node 14 usage duplication

## Summary

On 2026-03-27 and 2026-03-28, traffic usage for node `14` (`node-14`) was
over-counted on the panel. The most visible symptom was user `user-A`
appearing to consume multiple terabytes of traffic that did not actually occur.

The issue was caused by node-agent usage generation continuing while older
queued stats batches were still blocked from delivery. Once panel connectivity
recovered, overlapping deltas were accepted as distinct events and inflated the
quota window.

## Impact

- Panel-side monthly usage for `user-A` was inflated to roughly multi-terabyte
  scale.
- Another user on the same node (`user-B`) was also affected, though by a much
  smaller amount.
- Incorrect over-limit / quota-alert state was produced from the bad totals.

## Root cause

The node-agent usage loop was intended to be "flush-first", but the actual loop
still generated fresh stats batches when:

- queued batches existed, and
- no flush succeeded in the current tick.

Because `AckedStats` only advances after successful delivery, the newly
generated stats batches repeatedly covered overlapping history from the same old
acknowledged counters.

Once the panel became reachable again, those overlapping deltas were ingested as
different events because each batch carried a later cumulative counter in
`source_event_id`.

## Contributing factors

### 1. Access-first generation could starve stats

While reviewing the fix, we also found that generation logic could let frequent
access batches indefinitely starve stats generation on a busy node because only
one batch kind was ever generated per empty-queue tick.

### 2. Stats batch truncation could over-advance acknowledgements

`PUSH_BATCH_MAX_EVENTS` truncation originally cut the emitted event slice after
building full-user acknowledgement targets. That meant un-emitted directions or
users could still be marked as acknowledged locally, silently dropping future
usage.

This was not the primary cause of the incident, but it was a real structural
risk in the same pipeline and was fixed as part of the corrective review.

## Detection

The issue was detected through anomalous user traffic on the panel and verified
by comparing:

- panel `traffic_events`,
- quota window totals,
- node-local `state.json`,
- queued batches, and
- cumulative counters embedded in `source_event_id`.

## Remediation completed

### Code

1. Enforced the stronger invariant: **no fresh usage sampling while any queued
   batch remains pending**.
2. Allowed access and stats batches to both enqueue in the same empty-queue tick
   so stats cannot be starved by access traffic.
3. Changed stats-batch planning so acknowledgements advance only for counters
   fully represented by the emitted batch, plus safe zero-delta reset cases.
4. Added focused unit coverage for queue gating, scheduling, truncation, and
   reset acknowledgement semantics.

### Data

1. Backed up the production SQLite database before modification.
2. Reconciled affected node-14 `xray-stats` rows by replacing stored `bytes`
   with the true delta implied by successive cumulative counters.
3. Rebuilt dependent rollups and user/window aggregates.
4. Cleared incorrect over-limit / alert state produced by the inflated totals.

## Validation

After deployment and reconciliation:

- node 14 queue drained to empty;
- recent `xray-stats` rows returned to normal small deltas;
- `user-A` current window returned to normal scale;
- mismatch verification over the reconciled node-14 stats rows returned zero
  remaining bad rows.

## Follow-up expectations

Any future change to node-agent usage logic should be reviewed against the
invariants in `docs/USAGE_PIPELINE_DESIGN.md`, especially:

- flush-before-sample,
- acknowledgement scope under truncation,
- reset/epoch behavior, and
- fairness between access and stats generation.
