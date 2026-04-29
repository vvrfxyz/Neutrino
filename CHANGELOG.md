# Changelog

## Unreleased

- Fixed `/ops` heartbeat presentation by keeping node `last_seen_at` tied to panel receive time, while separately rendering the probe's own runtime report time on the node card.
- Added node natural-month RX/TX telemetry: node-agent now persists a local month accumulator, reports cumulative `month_rx_bytes` / `month_tx_bytes`, and panel stores the latest per-node month view for `/ops`.
- Documented the new node monthly usage design and clarified the distinction between panel host traffic, node host traffic, and user accounting.
- Redesigned `/ops` node monitoring cards into clearer per-node dashboards with a stronger header, control-plane/runtime split, and footer-style error surfacing, while keeping the underlying APIs and refresh flow unchanged.
- Tightened API authorization so `GET /api/v1/ops/nodes` now requires `nodes:read` instead of falling through without a scope check.
- Tightened Telegram self-service auth so `/me`, `/usage`, and `/sub` require a verified `/bind <code>` chat binding instead of username matching fallback.
- Marked admin session cookies as `Secure` when requests arrive via HTTPS / forwarded HTTPS, avoiding accidental downgrade over plain HTTP in proxied deployments.
- Fixed Clash subscription rendering so proxy groups reference the actual rendered proxy names, avoiding broken `AUTO` groups when node labels differ from `node-N`.
- Generalized over-limit auto-reactivation to the user's actual `quota_cycle` (`day|week|month`) instead of only monthly rollover.
- When extending an already expired user, the system now reactivates that user's latest proxy link so the restored `active` state is immediately usable.
- Refreshed the active documentation set (`README`, deployment runbook, operations manual, and Claude guidance) so it matches the current code surface, admin pages, auth model, node deploy flow, and release scripts.
- Hardened the node-agent usage pipeline so fresh stats/access sampling stops while any queued batch remains pending, eliminating overlapping delta generation during panel outages.
- Fixed stats batch acknowledgement semantics so `AckedStats` advances only for directions actually emitted in a truncated batch, preventing silent traffic loss under `PUSH_BATCH_MAX_EVENTS`.
- Allowed access and stats batches to both enqueue in the same empty-queue tick so busy access traffic cannot starve stats reporting.
- Added design documentation for usage-pipeline invariants and a postmortem for the March 28 node-14 traffic duplication incident.
- Updated the node one-click agent deployment script to verify `docker compose` first and install it automatically when missing.
- Strengthened node-side user synchronization so partial apply failures are treated as incomplete work and retried instead of being marked as fully applied.
- Refreshed user lists from current expiry/quota state before key admin and node-sync paths so the UI and downstream sync see up-to-date status more consistently.
- Added unit coverage for stale-user cleanup and partial-failure handling during user synchronization.
- Documented that manually disabling or deleting a user on managed nodes now triggers an extra node refresh so the change takes effect sooner.
- Ignored local `.buildx-cache/` artifacts to keep release/build leftovers out of git status.
