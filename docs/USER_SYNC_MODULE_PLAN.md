# User Sync Module Plan

## Goal

Make `users_sync` efficient without weakening correctness. The panel remains
the source of truth, full sync remains the authoritative repair path, and delta
sync is used only when the node-agent has the exact baseline version required by
the job.

Implement the full baseline, safe delta path, repair behavior, and tests in one
development slice.

## Data Flow

```text
Panel users for node
  -> canonical user snapshot + target version
  -> users_sync job payload: full or delta
  -> node-agent applies to local Xray
  -> node-agent persists synced users + synced version
  -> node-agent finishes job with applied_version
  -> panel accepts applied version only after version validation
```

## Core Rules

- A delta job is valid only when `agent.SyncedUsersVersion == base_version`.
- A full job never depends on prior agent state.
- Full repair must always be available and must override pending delta work.
- Delta is an optimization, not a second source of truth. If a delta is
  ambiguous, too large, generated from a missing baseline, or not clearly safer
  than full sync, enqueue full.
- Delta eligibility must be tracked by a durable per-node marker. Do not infer
  it from matching desired/applied hashes alone, because legacy panel state and
  newly upgraded agent state cannot prove the agent has a persisted versioned
  baseline.
- Mismatched base, stale target, or local hash mismatch are not retryable delta
  errors. They should request a forced full sync instead of re-running the same
  broken delta.
- Xray API operation failures are retryable execution errors and must not update
  the persisted synced user baseline.
- Do not update `nodes.applied_users_version` unless the returned
  `applied_version` matches the finished job's desired version or the node's
  current desired user version.
- An empty job `desired_version` is never an acceptance wildcard. Legacy jobs
  with empty desired version may be accepted only when `applied_version` matches
  the node's current desired version.

## Reference Takeaways

Xboard and Xboard-Node are useful references for operational shape, not an
implementation target. Keep these lessons and discard the mismatched parts:

- Keep the useful full/delta split: full user snapshots repair state, while
  small user changes can be applied without rebuilding the whole runtime.
- Keep the event-collapse idea from Xboard-Node's mailbox: full snapshots win
  over pending deltas, latest pending state wins over older pending state, and a
  delta before a known baseline triggers reconcile instead of being guessed.
- Keep the hot user-operation idea where Xray supports it, but make failures
  explicit through durable job status and repair jobs rather than WebSocket-only
  best effort.
- Do not adopt token-auth WebSocket control, Redis pub/sub delivery, weak typed
  payloads, machine-mode multiplexing, multi-kernel abstractions, speed-limit
  fields, or device-limit state in this module. Neutrino's control plane remains
  mTLS + pull-based durable `node_jobs`, and IP/device enforcement remains the
  existing online-snapshot path.
- Do not add ETag/304 polling semantics to `/agent/users`. Neutrino uses job
  desired versions and materialized snapshots for consistency; the full snapshot
  endpoint is a repair/read path, not an independent polling cache.

## Implementation Steps

### 1. Add a shared usersync package

Files:

- `internal/usersync/usersync.go`
- `internal/usersync/usersync_test.go`

Add canonical types:

```go
type Item struct {
	UserID int64  `json:"user_id"`
	Email  string `json:"email"`
	Status string `json:"status"`
	UUID   string `json:"uuid,omitempty"`
}

type Change struct {
	Action string `json:"action"` // "upsert" or "remove"
	User   Item   `json:"user"`
}

type JobPayload struct {
	Schema        int      `json:"schema"`
	Mode          string   `json:"mode"` // "full" or "delta"
	BaseVersion   string   `json:"base_version,omitempty"`
	TargetVersion string   `json:"target_version"`
	Reason        string   `json:"reason,omitempty"`
	CreatedAt     string   `json:"created_at,omitempty"`
	Changes       []Change `json:"changes,omitempty"`
}
```

Add helpers:

- `ValidateItems(items []Item) error`
- `HashItems(items []Item) string`
- `DiffItems(base, target []Item) (changes []Change, safe bool)`
- `ApplyChanges(base []Item, changes []Change) ([]Item, error)`

Keep this package independent from `internal/repo` to avoid an import cycle.
The canonical DB query and repo-to-sync-item adapter should live in
`internal/repo`, because sync preparation needs to read users inside the same
repository transaction that writes the snapshot and enqueues the job.
Agent-side user structs and repository rows must be converted to
`usersync.Item` before hashing, diffing, or applying changes. Do not keep
secondary hash structs or local hash implementations after this package exists.
Do not expand the canonical item with quota, traffic, speed-limit, device-limit,
or node metadata in this slice. If a future module needs extra runtime user
attributes, bump the payload schema, define the hash compatibility contract
explicitly, and force full sync across the schema boundary.

Canonical validation:

- Add `ValidateItems` and call it before hashing, diffing, storing snapshots,
  persisting agent state, or accepting a materialized full response.
- Reject duplicate `user_id`.
- Reject duplicate or empty normalized `email`. Normalization for validation is
  trim-only (`strings.TrimSpace`); do not lowercase, Unicode-normalize, or
  otherwise rewrite email values in this module. Xray user operations are keyed
  by email, so duplicate emails make removal/upsert ambiguous even when user IDs
  differ.
- Hashing and stored snapshots must preserve the original canonical `email`
  string returned by the DB or panel response. Validation may inspect the
  trim-only form, but it must not mutate the item before hashing; otherwise the
  legacy hash contract can drift.
- Reject `user_id <= 0`.
- Reject unknown status. The initial allowed statuses are `active`, `disabled`,
  `expired`, `over_limit`, and `over_ip_limit`; keep this list aligned with the
  repo user-status state machine.
- Reject active users with empty `uuid` only at the Xray-application layer, not
  in canonical validation. Existing full sync semantics remove inactive or
  uuid-less users, and that behavior should remain explicit in the agent path.
- `HashItems`, `DiffItems`, and `ApplyChanges` may assume their inputs have
  already passed validation, but tests must cover that callers validate before
  these helpers are used on DB snapshots, stored snapshots, and agent state.

Hash compatibility:

- Use the same canonical fields as the existing `UsersDesiredVersion` and
  `hashUsers`: `user_id`, `email`, `status`, `uuid`.
- Preserve the legacy JSON field order used by both existing implementations:
  `user_id`, `email`, `status`, then `uuid`. Hashes are computed over JSON
  bytes, so struct field order is part of the compatibility contract.
- Sort by `user_id`.
- Preserve `omitempty` behavior for empty `uuid`.
- Treat nil and empty input as the same canonical empty user list. `HashItems`
  must marshal an empty non-nil slice as `[]`, not `null`, so disabled-node
  cleanup and delete convergence keep the existing `UsersDesiredVersion([]repo.User{})`
  hash.
- Replace the existing local hash implementations by delegating to
  `usersync.HashItems` so panel and agent cannot drift later.
- Add at least one fixture that compares the exact hash for a mixed active and
  inactive user list against the pre-refactor implementation before deleting
  the old local hash code.
- Add an exact empty-list hash fixture that matches the current
  `UsersDesiredVersion([]repo.User{})` output.

Diff behavior:

- Produce only `upsert` and `remove`.
- Return changes in deterministic `user_id` order. Stable payload order keeps
  tests, logs, and job JSON diffs readable without changing correctness.
- Any item missing from target becomes `remove`.
- Any new item or changed `status`/`uuid` becomes `upsert`.
- If the same `user_id` has a changed `email`, return `safe=false`; the caller
  must enqueue a full job.
- If an email moves across user IDs between base and target, return
  `safe=false`. Xray operations are keyed by email, so cross-ID email ownership
  changes must go through full sync instead of relying on delta operation order.
- If `len(changes)` is above a conservative threshold or the encoded delta is
  close to the full snapshot size, treat the delta as not worth it and enqueue a
  full job. Pick the threshold in code as a named constant with tests; do not
  make it configurable until real production data requires it.

Apply behavior:

- `ApplyChanges` must not mutate its `base` or `changes` inputs.
- Return an error for malformed deltas, including duplicate changes for the
  same `user_id`, `remove` for a missing base user, `upsert` without a valid
  `user_id`/`email`/`status`, or unknown actions.
- For `remove`, require the change item to match the base item for that
  `user_id` at least on `email`, `status`, and `uuid`. A mismatched remove is
  invalid because the agent would otherwise be able to compute the target hash
  while deleting the wrong Xray email.

### 2. Persist per-node user snapshots

Files:

- `internal/db/db.go`
- `internal/repo/user_sync_snapshots.go`
- `internal/repo/user_sync_snapshots_test.go`

Add node columns:

```sql
ALTER TABLE nodes ADD COLUMN users_sync_baseline_schema INTEGER NOT NULL DEFAULT 0;
ALTER TABLE nodes ADD COLUMN users_sync_delta_ready_at TEXT;
ALTER TABLE nodes ADD COLUMN users_sync_baseline_backfill_at TEXT;
```

Column behavior:

- `users_sync_baseline_schema=0` means the panel must not generate delta jobs
  for that node, even if `applied_users_version == desired_users_version`.
- Set `users_sync_baseline_schema=1` and `users_sync_delta_ready_at` only after
  the finish path accepts a schema-1 full users-sync success reported by a new
  agent result (`result_json.mode == "full"`). Legacy full jobs or old agents
  that return no structured result must not enable delta generation.
- Keep `users_sync_baseline_schema=1` after accepted schema-1 delta successes.
- Reset `users_sync_baseline_schema` to `0` if a future schema bump changes the
  user payload/hash contract or if accepted agent state is explicitly cleared.
- `users_sync_baseline_backfill_at` records that rollout attempted to send a
  versioned full baseline job. It prevents old agents from receiving a new
  `baseline_backfill` job on every panel startup when they cannot return the
  schema-1 result needed to set `users_sync_baseline_schema=1`.

Add table:

```sql
CREATE TABLE IF NOT EXISTS node_user_sync_snapshots (
	node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	version TEXT NOT NULL,
	users_json TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY(node_id, version)
);

CREATE INDEX IF NOT EXISTS idx_node_user_sync_snapshots_node_created
ON node_user_sync_snapshots(node_id, created_at);
```

Repository behavior:

- Upsert a snapshot by `(node_id, version)`.
- Read a snapshot by `(node_id, version)`.
- Decode `users_json` into `[]usersync.Item`.
- Query canonical node users directly as `[]usersync.Item` inside sync
  preparation transactions. This keeps the user read, target hash, snapshot,
  desired version, and job payload consistent.
- The canonical query must preserve current access semantics: users with no
  `user_node_access` rows are allowed on all nodes, and restricted users are
  included only for listed nodes. Disabled nodes still materialize an empty
  target list.
- Sort snapshots by `user_id` before hashing and before storing `users_json`, so
  the stored JSON is canonical and stable for later diffing/debugging.
- Prune old snapshots best-effort while retaining:
  - `nodes.desired_users_version`;
  - `nodes.applied_users_version`;
  - pending/running `users_sync.desired_version`;
  - every pending/running delta job `base_version`.
- If pruning cannot parse a pending/running job payload, keep snapshots rather
  than risking deletion of a needed delta baseline.
- Trigger pruning after each successful snapshot upsert.
- In addition to referenced versions, retain at least the newest 10 snapshots
  per node for debugging and rollback analysis. Pruning must never delete a
  retained referenced version just because it is older than the newest 10.
- Snapshot writes and pruning must be best-effort in the right direction:
  failure to write the target snapshot is fatal to sync preparation; failure to
  prune old snapshots is logged and non-fatal.

### 3. Add lifecycle refresh without sync side effects

Files:

- `internal/service/users.go`
- callers in `internal/service/nodes.go`

Add a helper used by sync generation:

```go
type LifecycleRefreshResult struct {
	Expired     int
	Reactivated int
}

func (r LifecycleRefreshResult) Changed() bool {
	return r.Expired > 0 || r.Reactivated > 0
}

func (s *UserService) RefreshLifecycleStateNoSync(ctx context.Context) (LifecycleRefreshResult, error)
```

Behavior:

- Call `SweepExpiredUsers`.
- Call `SweepQuotaWindows`.
- Return the affected counts and errors.
- Do not call `RequestUsersSync`, `RequestUsersSyncNow`, or
  `RequestUsersSyncForNodeNow`.
- Call this helper before starting the users-sync preparation transaction. Do
  not call sweep methods from inside `PrepareUsersSync`, because those methods
  open their own transactions and can conflict with the sync transaction.
- Callers must not ignore `result.Changed()`. Lifecycle sweeps are global user
  mutations, even when discovered while preparing a single-node sync or serving
  `/agent/users`.
- If `result.Changed()` is true, arrange a global users reconcile outside the
  current repository transaction after the current node's materialize/prepare
  work has completed. Use a coalesced signal that is observable and cannot be
  silently dropped by throttle. The current fire-and-forget throttled
  `RequestUsersSync` shape is not sufficient for this path unless it is changed
  to record a pending reconcile when throttled.
- If scheduling the global reconcile fails or is unavailable, log visible
  degraded state and persist or expose a pending/degraded reconcile indicator
  before relying on the periodic reconciler. Add tests for this degraded path so
  the window is intentional.

Keep the existing `RefreshLifecycleState` behavior for UI/API paths that should
trigger sync after lifecycle changes.

### 4. Generate users_sync jobs transactionally

Files:

- `internal/service/nodes.go`
- `internal/repo/user_sync_jobs.go`
- tests near existing node job and node service tests

Replace the current `EnqueueUsersSyncForNode` implementation with a service
path backed by repo-only store methods similar to `DeployManagedXray`. Split the
implementation into an outer transaction owner and an inner helper:

```go
func (s *Store) PrepareUsersSync(ctx context.Context, nodeID int64, forceFull bool, reason string) (jobID int64, enqueued bool, targetVersion string, err error)
func (s *Store) prepareUsersSyncTx(ctx context.Context, tx *sql.Tx, nodeID int64, forceFull bool, reason string) (jobID int64, enqueued bool, targetVersion string, err error)
```

`PrepareUsersSync` owns the transaction for ordinary service calls.
`prepareUsersSyncTx` must not open or commit a transaction; it materializes the
target snapshot, writes `desired_users_version`, decides full/delta, and enqueues
the job using the supplied transaction. Finish paths for `users_sync`,
`xray_apply`, `xray_rollback`, and rollout backfill must call the Tx helper so a
forced full payload cannot be created with a stale or empty `target_version`.

Behavior:

1. The service layer runs the no-side-effect lifecycle refresh before the first
   store prepare call in a reconcile cycle. For global enabled-node reconcile,
   run this refresh once for the whole cycle, then call the transaction helper
   for each node; do not run `SweepExpiredUsers`/`SweepQuotaWindows` once per
   node.
   The store method must not call lifecycle sweep helpers, request users sync,
   or depend on service-layer interfaces.
   If the refresh reports global lifecycle changes, the service layer must
   schedule a global reconcile after this node's prepare call returns.
2. The store method runs one repository transaction:
   - read the node;
   - read users allowed for that node directly into canonical
     `[]usersync.Item`;
   - validate the canonical target with `usersync.ValidateItems`;
   - use an empty target list for disabled nodes;
   - compute `targetVersion := usersync.HashItems(target)`;
   - write the target snapshot;
   - update `nodes.desired_users_version`;
   - read the applied snapshot when needed for delta;
   - decide job mode;
   - enqueue the `users_sync` job with structured payload.

Mode selection:

- Use full when the node has no `applied_users_version`.
- Use full when the applied snapshot cannot be found.
- Use full when the applied snapshot exists but does not hash to
  `applied_users_version`. A stored snapshot/hash mismatch is corruption or a
  bug, so log it and avoid generating a delta from that snapshot.
- When a job is going to be enqueued, use full instead of delta if
  `nodes.users_sync_baseline_schema < 1`. Delta jobs are allowed only after a
  schema-1 full success has been accepted from a new agent and the durable
  marker has been set.
- Use full for explicit repair requests.
- Use full when `DiffItems` returns `safe=false`.
- Use full when the delta is empty but `applied_users_version != targetVersion`,
  because that indicates hash/snapshot inconsistency that should not be hidden.
- Use delta only when the applied snapshot exists, the diff is safe, and
  `nodes.users_sync_baseline_schema >= 1`.
- If `forceFull=false` and `applied_users_version == targetVersion`, update
  desired/snapshot as needed and do not enqueue a job for ordinary reconcile.
  This applies to disabled nodes too; once the empty target has been applied, no
  repeated cleanup job is needed. The rollout/backfill path that must establish
  a delta-ready baseline should call this method with `forceFull=true` for nodes
  whose `users_sync_baseline_schema < 1`.
- If `forceFull=true`, enqueue a full job even when
  `applied_users_version == targetVersion`, because the agent has explicitly
  reported that its local baseline needs repair.
- `xray_apply` success must call the forced-full path, not ordinary reconcile.
  Xray reload/apply can rebuild runtime state and wipe hot-added API users even
  when `applied_users_version == targetVersion`, so the follow-up must be
  `forceFull=true` with reason `xray_apply_followup`. Keep this conservative
  default for every accepted `xray_apply` success unless the agent result grows
  an explicit, trusted `runtime_reloaded=false` contract and tests prove the
  runtime was not restarted/reloaded.
- Any managed Xray operation that actually reloads or restarts the local runtime
  must force a full users sync afterward. This includes accepted `xray_apply`
  success, successful `xray_rollback`, and failed `xray_apply` attempts that
  rolled back and reloaded the previous config. Use distinct reasons such as
  `xray_apply_followup`, `xray_rollback_followup`, and
  `xray_apply_rollback_followup`.
- During rollout, do not generate delta jobs for a node until the panel has
  accepted at least one versioned full users-sync success from that node. A node
  with only legacy applied state may have `nodes.applied_users_version`, but an
  upgraded agent can still lack `SyncedUsersVersion`; sending a delta first only
  guarantees a base-mismatch repair cycle. Enforce this with
  `nodes.users_sync_baseline_schema`, not with hashes alone. The rollout task
  should enqueue one forced full users sync for each existing node with
  `users_sync_baseline_schema < 1`; ordinary no-drift reconcile should not
  repeatedly enqueue full jobs for old agents that cannot set the marker.

Rollout/backfill entrypoint:

- Add an app startup hook or worker task, for example
  `NodeService.EnqueueUsersSyncBaselineBackfill(ctx)`, that scans every enabled
  sync-capable node with `users_sync_baseline_schema < 1` and enqueues exactly
  one forced full users sync per node with reason `baseline_backfill`.
- Run this hook after migrations and service construction, before relying on
  periodic ordinary reconcile. It must not depend on a user mutation, node
  mutation, or operator action to run.
- Make the hook idempotent: if a pending/running full backfill or repair full
  already exists for the node, leave it; if only stale pending deltas exist,
  replace them with the full backfill job using the full-over-delta rules.
- After successfully enqueueing or finding an equivalent pending/running full
  backfill, set `users_sync_baseline_backfill_at=now`. On later startups, skip
  nodes with this timestamp unless an operator explicitly requests repair or the
  backfill job was canceled before claim. This avoids repeated startup enqueue
  loops for old agents that cannot set `users_sync_baseline_schema=1`.
- Do not let this timestamp permanently strand a node after the agent is later
  upgraded. Record enough observed agent capability, such as last structured
  users-sync result schema or node-agent version if available, and run a bounded
  recheck/backfill when that capability changes from legacy/unknown to schema-1
  capable.
- If a `baseline_backfill` job is terminal failed or canceled before setting
  `users_sync_baseline_schema=1`, clear `users_sync_baseline_backfill_at` or
  record visible degraded rollout state and schedule a bounded retry. Do not
  leave a node permanently schema-0 with desired/applied versions equal and no
  future full baseline path.
- Apply the same cleanup to any full users-sync job that was accepted as an
  equivalent backfill candidate. If that equivalent repair/full job terminally
  fails or is canceled before the schema marker is set, clear
  `users_sync_baseline_backfill_at` or record degraded rollout state with a
  bounded retry.
- Do not mark `users_sync_baseline_schema=1` from the backfill enqueue itself;
  only the accepted schema-1 full finish may set the marker.

Structured payloads:

- Full:

```json
{
  "schema": 1,
  "mode": "full",
  "target_version": "<targetVersion>",
  "reason": "<reason>",
  "created_at": "<RFC3339 UTC>"
}
```

- Delta:

```json
{
  "schema": 1,
  "mode": "delta",
  "base_version": "<appliedVersion>",
  "target_version": "<targetVersion>",
  "created_at": "<RFC3339 UTC>",
  "changes": [
    {
      "action": "upsert",
      "user": {
        "user_id": 123,
        "email": "user@example.com",
        "status": "active",
        "uuid": "<uuid>"
      }
    }
  ]
}
```

Keep `{}` as the legacy full payload for compatibility in the agent parser.
Use a small fixed reason vocabulary in tests and logs, for example
`reconcile`, `repair`, `base_version_mismatch`, `local_hash_mismatch`,
`remove_base_mismatch`, `invalid_local_baseline`,
`full_response_hash_mismatch`, `finish_version_mismatch`,
`baseline_backfill`, `xray_apply_followup`, `xray_rollback_followup`,
`xray_apply_rollback_followup`, and `xray_runtime_unknown_followup`.

### 5. Enforce full-over-delta job priority

Files:

- `internal/repo/user_sync_jobs.go`
- `internal/repo/node_jobs.go` only if a small shared helper is needed

Add a dedicated users-sync enqueue function instead of calling generic
`EnqueueNodeJob` directly from the service:

```go
func (s *Store) EnqueueUsersSyncJobTx(...)
```

Rules:

- Pending full must not be overwritten by a later delta.
- Full may overwrite a pending delta.
- A forced full must cancel or overwrite every older pending delta for the same
  node before returning. Do not allow an older pending delta to remain claimable
  ahead of the forced full after the current running job finishes.
- Pending full with the same target version may be kept as-is for ordinary
  reconcile. Forced full repair/backfill may reuse a pending full only if that
  pending full already carries a repair-compatible protected reason; otherwise
  rewrite or replace it so its payload reason is protected and later redundant
  cleanup will not cancel the runtime repair.
- Newer delta may overwrite older pending delta for the same node.
- A pending delta may be kept only when its `base_version` still equals the
  node's current `applied_users_version`; otherwise replace it with a freshly
  prepared job so it will not fail immediately after claim.
- Treat empty or `{}` pending users_sync payloads as legacy full for overwrite
  decisions. Unparsable payloads or structured payloads with unknown schema/mode
  are invalid, not protected full jobs: ordinary delta should not overwrite them
  blindly, but forced full repair/backfill must cancel or overwrite them.
- If ordinary reconcile finds a claimable invalid pending `users_sync` payload
  that would block convergence, cancel it and enqueue a full repair, or persist
  visible degraded queue state that triggers operator/actionable repair. Do not
  let an invalid pending payload remain as a permanent queue poison pill.
- Ordinary reconcile leaves a running users_sync job alone; if its result becomes
  stale, finish validation or the next reconcile will enqueue repair.
- Forced full repair is stricter: `forceFull=true` must not be considered
  satisfied by a running delta, even when the running delta has the same
  `desired_version`. It must leave or create a pending full repair job behind
  the running job, and that pending full must be the next claimable
  `users_sync` job for the node after the running job finishes.
- `forceFull=true` may reuse a running job only when the captured running
  payload is already a full payload and its reason is a repair-compatible full
  reason. Otherwise enqueue or overwrite a pending full.
- Keep claim ordering simple only if the enqueue transaction guarantees there is
  no older pending delta left ahead of the forced full. If multiple pending
  `users_sync` jobs are allowed to coexist, `ClaimNextNodeJob` must prefer full
  payloads over delta payloads for the same node.

### 6. Return versioned full snapshots to the agent

Files:

- `internal/app/handlers_nodes.go`
- `internal/service/nodes.go`
- `internal/agent/panel_client.go`

Change `GET /api/v1/nodes/{id}/agent/users` response to:

```json
{
  "schema": 1,
  "version": "<current desired user version>",
  "users": []
}
```

Behavior:

- Preserve mTLS authorization.
- Replace the current `RefreshLifecycleState` call with the no-side-effect
  lifecycle helper; this endpoint must not recursively request users sync while
  serving a full snapshot.
- If the lifecycle helper reports changes, schedule a global users reconcile
  outside the materialize transaction via the app's coalesced sync signal. This
  prevents a single node's `/agent/users` request from consuming global expiry
  or quota reactivation changes without notifying the other nodes.
- Always materialize the current canonical DB users for the node, compute the
  version, write the snapshot, and update `nodes.desired_users_version` without
  enqueueing a job.
- Validate the materialized canonical users with `usersync.ValidateItems` before
  hashing or storing the snapshot.
- Do the materialize/hash/snapshot/desired-version update in one repository
  transaction, for example with a dedicated
  `MaterializeUsersSnapshotForAgent(ctx, nodeID)` store method. The endpoint
  must not read users, compute a version, and update desired state in separate
  autocommit operations.
- Disabled nodes materialize an empty user list, matching ordinary sync
  preparation behavior.
- Return that freshly materialized snapshot. Do not prefer an existing desired
  snapshot here; full sync and full repair must read current panel truth.
- Existing agents that decode only `users` continue to work because extra fields
  are ignored.
- New agents must treat a missing `version` as old-panel compatibility and use
  the full path only.
- Change `PanelClient.FetchUsers` to return both `users` and `version` rather
  than only `[]UserSyncItem`; keep old-panel JSON compatibility by allowing
  `version == ""`.

Do not use this endpoint as a second hidden source of truth for delta jobs.
Delta jobs carry their own target version and changes.

### 7. Persist agent baseline version

Files:

- `internal/agent/state.go`
- `internal/agent/agent.go`
- `internal/agent/*test.go`

Add to `State`:

```go
SyncedUsersVersion string `json:"synced_users_version,omitempty"`

PendingUsersSync *PendingUsersSyncState `json:"pending_users_sync,omitempty"`

type PendingUsersSyncState struct {
	Mode                 string         `json:"mode"`
	BaseVersion          string         `json:"base_version,omitempty"`
	TargetVersion        string         `json:"target_version"`
	TargetUsers          []UserSyncItem `json:"target_users"`
	PossibleRuntimeUsers []UserSyncItem `json:"possible_runtime_users,omitempty"`
	CreatedAt            string         `json:"created_at"`
}
```

Behavior:

- Change `StateStore.Update` to be memory-atomic. The current mutate-then-save
  shape must be replaced with copy-on-write or a transactional helper: build a
  deep copy, run the mutation on the copy, write it to disk, and swap `st.s` only
  after the save succeeds. A failed save must leave both persisted state and
  in-memory `State` unchanged.
- Full sync success persists both `SyncedUsers` and `SyncedUsersVersion`.
- Delta sync success applies changes to a deep-copied persisted baseline in
  memory, checks `usersync.HashItems(next) == target_version`, then persists
  both users and version.
- Any failed sync attempt leaves both `SyncedUsers` and `SyncedUsersVersion`
  unchanged.
- On successful full sync from an old panel with no response version, persist the
  locally computed `usersync.HashItems(users)` as `SyncedUsersVersion` so later
  versioned panel jobs have a concrete baseline.
- Treat state persistence failure as a retryable job failure and do not return
  `AppliedVersion`. A users_sync job is not successful unless Xray operations
  and local baseline persistence both succeed.
- Before mutating Xray for either full or delta, capture any existing
  `PendingUsersSync`, merge its `TargetUsers` and `PossibleRuntimeUsers` into a
  deduplicated `PossibleRuntimeUsers` set, then persist a new
  `PendingUsersSync` with the materialized target users, target version, and
  merged possible runtime users. If this pre-operation journal cannot be
  persisted, fail retryably before touching Xray.
- After Xray operations and committed baseline persistence succeed, clear
  `PendingUsersSync` in the same state update that writes `SyncedUsers` and
  `SyncedUsersVersion`.
- If Xray operations or final baseline persistence fail after some Xray
  mutations may have occurred, leave `PendingUsersSync` intact. Later full sync
  execution must treat `SyncedUsers`, `PendingUsersSync.TargetUsers`, and
  `PendingUsersSync.PossibleRuntimeUsers` as the possible previous runtime set
  for stale-user removal, so users added during one or more failed attempts
  cannot become untracked.
- Delta execution may reuse an existing `PendingUsersSync` only when it matches
  the same `mode`, `base_version`, and `target_version`. An unrelated pending
  journal makes delta unsafe; request forced full instead of touching Xray.
- Do not ignore `StateStore.Update` errors. Full and delta paths must surface
  those errors in the finish result as retryable failures.
- `State.Snapshot()` currently returns a shallow copy. Either make the
  users-sync path deep-copy `SyncedUsers` before deriving a new baseline, or
  introduce a state helper that returns a deep-copied synced-users baseline.
- State loading should accept old state files with no version; those agents must
  do a full sync before any delta can be accepted.
- Prefer a rollout/backfill gate on the panel side: existing nodes should get a
  forced full users sync after this module is deployed, and delta generation
  should remain disabled for each node until that versioned full success is
  accepted. Agent-side base mismatch repair remains a safety net, not the normal
  upgrade path.

### 8. Execute full and delta jobs in node-agent

Files:

- `internal/agent/agent.go`
- `internal/agent/panel_client.go`
- `internal/agent/users_sync_test.go`

Parse `job.PayloadJSON`:

- Empty or `{}` means legacy full.
- `schema=1, mode=full` means full.
- `schema=1, mode=delta` means delta.
- Unknown schema or mode is a permanent job failure.

Payload validation:

- Validate and normalize the parsed payload before touching Xray.
- Legacy full payloads require no local baseline and no target version.
- Schema-1 full payloads require non-empty `target_version` but still require no
  local baseline.
- Delta payloads require non-empty `base_version`, non-empty `target_version`,
  at least one change, and only `upsert` or `remove` changes.
- Every delta change must include a valid `user_id`, `email`, and `status`.
  `upsert` uses the target item; `remove` uses the base item so the agent knows
  which Xray email to remove.
- For `remove`, verify the change item matches the agent's local base item for
  the same `user_id` on `email`, `status`, and `uuid` before computing the next
  baseline or touching Xray. If it does not match, fail non-retryably with
  `need_full_sync=true` and reason `remove_base_mismatch`.
- Invalid delta payloads are non-retryable failures with `need_full_sync=true`
  and must not modify Xray or persisted agent state.
- Validate the persisted base with `usersync.ValidateItems` before applying a
  delta. If the local baseline is invalid, fail non-retryably with
  `need_full_sync=true` and reason `invalid_local_baseline`.

Full behavior:

- Fetch the current versioned users from the panel.
- Validate fetched users with `usersync.ValidateItems` before hashing,
  journaling, or touching Xray.
- Compute `computedVersion := usersync.HashItems(users)` after validation.
- If `response.version` is non-empty, require
  `response.version == computedVersion` before journaling, touching Xray,
  persisting state, or returning `AppliedVersion`. A schema-1 full response whose
  version does not hash-match its users is internally inconsistent; fail
  retryably without `AppliedVersion` and include reason
  `full_response_hash_mismatch`.
- Apply with the existing full sync semantics.
- Normalize `appliedVersion`: use `response.version` when present; otherwise
  use `computedVersion` for old-panel compatibility.
- Before Xray operations, persist `PendingUsersSync` with the full target.
- Apply removals against the union of committed `SyncedUsers` and the previously
  captured pending target plus its `PossibleRuntimeUsers`, then apply the fetched
  target. Deduplicate by Xray email when building this removal set.
- Persist users and `appliedVersion`, and clear `PendingUsersSync`, in one state
  update. If persistence fails, return retryable failure without
  `AppliedVersion`.
- Return `AppliedVersion=appliedVersion`.
- It is valid for the fetched full response version to differ from the job's
  `target_version` if panel state changed after the job was created. The finish
  handler decides whether to accept it by comparing against the node's current
  desired version.
- Result JSON `target_version` must always be the actual fetched/applied
  `appliedVersion`, not necessarily the stale job payload target. This keeps
  finish validation and delta-ready marker checks tied to what the agent really
  applied.
- If the fetched full response has an empty user list, apply it as an explicit
  cleanup snapshot, not as "no data". This is how disabled-node cleanup and node
  delete safety converge.

Delta behavior:

1. Require `State.SyncedUsersVersion == payload.BaseVersion`.
2. If the base mismatches, return a non-retryable failed job with:

```json
{
  "mode": "delta",
  "need_full_sync": true,
  "reason": "base_version_mismatch"
}
```

3. Verify `usersync.HashItems(State.SyncedUsers) == State.SyncedUsersVersion`
   before applying changes. If the committed local baseline hash does not match
   its stored version, fail non-retryably with `need_full_sync=true` and reason
   `invalid_local_baseline`; do not touch Xray or state.
4. Build the next local user list from a deep copy with `ApplyChanges`.
   `ApplyChanges` validation errors are non-retryable and request full sync.
5. Validate the materialized `next` list with `usersync.ValidateItems`.
   Validation errors are non-retryable and request full sync.
6. Verify the local hash equals `payload.TargetVersion`.
7. If the hash mismatches, return non-retryable failure with
   `need_full_sync=true` and reason `local_hash_mismatch`.
8. Before Xray operations, persist `PendingUsersSync` with the materialized
   `next` list.
9. Only after payload validation, base-hash validation, next-baseline
   validation, local hash verification, and pre-operation journaling pass, apply
   each change through the Xray API:
   - `upsert` with active `uuid` calls `UpsertUser`.
   - `upsert` for inactive or empty `uuid` calls `RemoveUser`.
   - `remove` calls `RemoveUser`.
10. If any Xray operation fails, return retryable failure and do not update
   state. Some operations may already have reached Xray; retries must rely on
   idempotent `UpsertUser`/`RemoveUser` behavior, the unchanged committed
   baseline, and the retained pending journal.
11. Persist users and `SyncedUsersVersion=payload.TargetVersion`, and clear
    `PendingUsersSync`, in one state update. If persistence fails, return
    retryable failure without `AppliedVersion`.
12. Return `AppliedVersion=payload.TargetVersion`.

Execution guard:

- Hold the existing agent-side user sync mutex around full and delta Xray
  changes plus baseline persistence so two user sync paths cannot interleave
  local Xray operations and state writes. Any path that maps runtime Xray users
  back to panel users must use the effective synced-users helper described in
  the restore/online snapshot section, not raw persisted `SyncedUsers`.

Result JSON for both modes should include:

- `mode`
- `base_version` when present
- `target_version`
- `synced`
- `removed`
- `failed`
- `need_full_sync` when true
- `reason` when present
- `failures` when present

### 9. Validate finish results and enqueue forced full repair

Files:

- `internal/app/handlers_nodes.go`
- `internal/repo/user_sync_jobs.go`
- tests near handler/node job tests

Move node-job finish side effects out of the HTTP handler into repo/service
entrypoints. The handler should decode and authorize the request, then call a
method that owns the terminal transition and all follow-up writes.

For `users_sync`, use an entrypoint such as:

```go
func (s *Store) FinishUsersSyncJobForNode(ctx context.Context, nodeID, jobID int64, in FinishNodeJobInput, appliedVersion string) (FinishUsersSyncResult, error)
```

That method must:

- Load the running job before terminal/retry transition and compare against the
  captured job's `desired_version`.
- Preserve `FinishNodeJobForNode` retry semantics, but run the users-sync
  follow-up decisions in the same repository transaction whenever the final
  status is terminal.
- Use the final status for all follow-up decisions. If `finalStatus == "pending"`,
  stop there; the retry is already scheduled.
- Reload the node in the same transaction before comparing against current
  `desired_users_version`.
- If it is a succeeded users_sync result:
  - trim and require a non-empty `applied_version`;
  - accept `applied_version` only when it equals the captured job's non-empty
    `desired_version` or the node's current `desired_users_version`;
  - if it is not accepted, do not update `nodes.applied_users_version`; enqueue
    a forced full users sync with reason `finish_version_mismatch` and stop;
  - if it is accepted, update `nodes.applied_users_version`;
  - if the captured payload and result show an accepted schema-1 full success
    from the new agent (`payload.schema == 1`, `payload.mode == "full"`,
    `result_json.mode == "full"`, and
    `result_json.target_version == applied_version`), set
    `nodes.users_sync_baseline_schema=1` and `users_sync_delta_ready_at=now`;
    do not set this marker for legacy empty payloads or missing/old result JSON;
  - if a schema-1 users_sync job succeeds with an accepted `applied_version`,
    returns missing, legacy, or malformed result JSON, and the node was already
    delta-ready before this job (`users_sync_baseline_schema >= 1`), treat it as
    agent capability drift: clear `nodes.users_sync_baseline_schema` to `0`,
    clear `nodes.users_sync_delta_ready_at`, record visible degraded node state,
    and enqueue a forced full backfill/repair instead of continuing to send
    deltas;
  - if a schema-1 full backfill succeeds on a node that was not delta-ready yet
    and returns old-format or missing result JSON, accept the applied version but
    leave `users_sync_baseline_schema=0`. Record legacy agent capability/degraded
    rollout state and do not enqueue an immediate forced full loop for that same
    legacy success;
  - after accepting success, cancel or overwrite any pending delta users_sync
    jobs whose `base_version` no longer matches the accepted
    `applied_version`; otherwise a stale pending delta can immediately fail and
    trigger an unnecessary repair full sync;
  - after accepting success, cancel redundant pending users_sync jobs whose
    `desired_version` equals the accepted `applied_version`, including same
    version full jobs and same-version deltas that no longer need to run. Do not
    cancel protected repair/backfill full jobs whose reason requires runtime
    repair even at the same version, including `xray_apply_followup`,
    `xray_apply_rollback_followup`, `xray_rollback_followup`,
    `xray_runtime_unknown_followup`, and `baseline_backfill`;
  - if the accepted version is not the node's current desired version, enqueue a
    follow-up users sync so stale successful work converges immediately;
- If it is a terminal failed users_sync result whose result JSON contains
  `need_full_sync=true`, enqueue a forced full users sync immediately.
- If the failure is retryable and `FinishNodeJobForNode` requeued it as pending,
  do not enqueue forced full yet.
- Return whether applied version was accepted, whether a full repair was
  enqueued, whether a follow-up reconcile was enqueued, and any stale pending
  delta cleanup count. The HTTP handler must not ignore these errors or perform
  best-effort follow-up writes after the transaction.

Add an explicit service entrypoint:

```go
func (s *NodeService) EnqueueFullUsersSyncForNode(ctx context.Context, nodeID int64, reason string) error
```

This must always produce a full payload and must obey full-over-delta priority.

For managed Xray jobs, add finish entrypoints that update Xray applied state and
enqueue the users follow-up in one transaction whenever runtime reload may have
removed hot-added API users. For `xray_apply`, use an entrypoint such as:

```go
func (s *Store) FinishXrayApplyJobForNode(ctx context.Context, nodeID, jobID int64, in FinishNodeJobInput, appliedVersion string) (FinishXrayApplyResult, error)
```

That method must:

- Preserve existing retry semantics. If `finalStatus == "pending"` and the
  result does not indicate runtime reload or runtime state uncertainty, stop
  there. If the result says the runtime was reloaded or may have been reloaded,
  enqueue the forced full users-sync repair before allowing the requeued Xray job
  to run again.
- On terminal success, trim and require non-empty `appliedVersion`.
- Accept the Xray applied version only when it equals the captured
  `xray_apply.desired_version` or the node's current `desired_xray_version`.
- In the same repository transaction as the accepted
  `nodes.applied_xray_version` update, enqueue a forced full `users_sync` with
  reason `xray_apply_followup`.
- If a terminal failed `xray_apply` result reports that the agent rolled back
  and reloaded the previous config, enqueue a forced full `users_sync` with
  reason `xray_apply_rollback_followup` in the same finish transaction. The
  failure status should remain failed; this follow-up only repairs runtime users
  after the reload.
- If `FinishNodeJobForNode` would requeue the failed `xray_apply` as pending but
  the result reports that rollback already reloaded runtime, or reports that
  runtime state is unknown after a reload attempt, do not let that retry be the
  next same-node control job. Either mark this `xray_apply` terminal failed and
  enqueue the forced full users-sync repair, or keep the retry pending but make
  claim ordering prefer the forced full `users_sync` repair before the requeued
  `xray_apply`. Retry scheduling must not delay runtime-user repair after a
  reload has already happened or may have happened.
- Use the same internal full-over-delta enqueue helper as ordinary users-sync
  repair, so a running delta cannot swallow the forced full follow-up.
- Roll back the finish transaction if the forced full users-sync follow-up
  cannot be enqueued. The handler must not update `applied_xray_version` and
  then best-effort the user sync after the fact.

Add the same transactional follow-up for accepted `xray_rollback` success:

- Use the existing rollback applied-version rules, but move them into a
  repo/service finish entrypoint instead of handler best-effort writes.
- In the same transaction as `MarkNodeXrayRolledBack`, enqueue a forced full
  `users_sync` with reason `xray_rollback_followup`.
- If the rollback finish path cannot enqueue the forced full users sync, roll
  back the applied-Xray update and return an error to the handler.
- If a failed `xray_rollback` result reports that the backup restore happened
  and runtime reload was attempted, treat runtime-user state as unknown. Enqueue
  a forced full `users_sync` with reason `xray_runtime_unknown_followup` before
  allowing any requeued rollback job to claim again. A failed rollback validation
  that never restored the config and never attempted reload does not need this
  users repair.

Agent result JSON for managed Xray jobs must expose whether the runtime was
reloaded/restarted during a failed attempt, for example
`{"runtime_reloaded": true, "rollback_applied": true}`. When the command result
cannot prove whether Xray reloaded, expose that explicitly, for example
`{"runtime_state_unknown": true, "rollback_restore_applied": true}`. The panel
should enqueue `xray_apply_rollback_followup` only for a confirmed apply
rollback reload, and `xray_runtime_unknown_followup` for unknown runtime state. A
validation failure that never touched/reloaded Xray does not need users repair.

Timeout sweeper behavior:

- `SweepTimedOutRunningJobs` must not bypass runtime-user repair for managed
  Xray jobs. When a running `xray_apply` or `xray_rollback` times out, the panel
  no longer knows whether the agent reloaded/restarted runtime before losing the
  finish report.
- For any timed-out managed Xray job that is requeued or terminal failed,
  conservatively enqueue a forced full `users_sync` in the same sweep
  transaction with reason `xray_runtime_unknown_followup`, or persist an
  equivalent visible `runtime_user_state_unknown` flag that immediately triggers
  that forced full before ordinary reconcile can skip on matching versions.
- If the timed-out Xray job still has retry attempts left, the sweep must not
  let the requeued `xray_apply`/`xray_rollback` claim before the repair. Either
  mark the timed-out Xray job terminal failed when runtime-user state is unknown,
  or update claim priority/status so the forced full `users_sync` repair is the
  next same-node control job before any Xray retry.
- This timeout follow-up must use `prepareUsersSyncTx` and full-over-delta
  priority so it cannot be swallowed by pending/running delta state.

Rollout compatibility:

- Old agents that ignore structured `PayloadJSON` and always perform full sync
  remain safe. They will fetch `/agent/users`, apply the full snapshot, and
  return a hash/version that the finish validator accepts only under the same
  version rules. If a node previously had `users_sync_baseline_schema=1` but now
  returns old-format or malformed users_sync result JSON for a schema-1 job,
  treat it as agent capability drift: clear the delta-ready marker and ready
  timestamp, record degraded state, and force a versioned full backfill before
  sending deltas again.
- New agents talking to old panels also remain safe because missing response
  `version` forces the full path and stores the locally computed hash baseline.
  Delta jobs require schema-1 payloads and therefore cannot be produced by the
  old panel.

### 10. Keep restore and online snapshot behavior coherent

Files:

- `internal/agent/agent.go`
- `internal/agent/online_snapshot_test.go`

Behavior:

- Startup restore may continue using `SyncedUsers` to repopulate Xray.
- Add a shared agent helper, for example `EffectiveSyncedUsers()`, that returns
  committed `SyncedUsers` plus deduplicated `PendingUsersSync.TargetUsers` and
  `PendingUsersSync.PossibleRuntimeUsers` while a pending journal exists.
- Online snapshot, stats, access-log attribution, and IP-limit mapping must use
  that helper instead of reading raw `SyncedUsers`. A failed Xray mutation may
  already have hot-added users before state commit, so committed baseline alone
  is not enough during that repair window.
- Delta success must update `SyncedUsers` exactly as full sync would, so online
  snapshot user mapping remains correct.

### 11. Tests

Add or update tests for:

- `usersync.HashItems` matches existing `UsersDesiredVersion`/`hashUsers`
  canonical output.
- `usersync.ValidateItems` rejects duplicate `user_id`, duplicate/empty email,
  invalid ID, and unknown status.
- `usersync.ValidateItems` uses trim-only email normalization for validation and
  does not mutate the canonical item used for hashing.
- Agent delta validates both the persisted base and materialized next baseline
  before hashing or touching Xray; invalid local baseline requests full repair.
- `usersync.HashItems` preserves the legacy JSON field order
  `user_id,email,status,uuid` with an exact hash fixture.
- `usersync.HashItems(nil)` and `usersync.HashItems([]usersync.Item{})` both
  match the existing empty-list hash produced from JSON `[]`, not `null`.
- `usersync.DiffItems` and `ApplyChanges` for add, remove, status change, UUID
  change, and email change forcing full.
- `usersync.DiffItems` returns changes in deterministic `user_id` order.
- `usersync.DiffItems` treats cross-ID email ownership movement as unsafe and
  forces full.
- Delta size threshold chooses full when the delta is too large or no smaller
  than full.
- Stored base snapshot hash mismatch chooses full and logs the mismatch.
- `usersync.ApplyChanges` rejects duplicate changes, remove-missing-base,
  remove-base-mismatch, invalid upsert, and unknown actions without mutating
  inputs.
- Canonical node-user query preserves unrestricted-user and restricted-user
  access semantics inside the sync transaction.
- No-side-effect lifecycle refresh runs sweeps but does not request sync.
- No-side-effect lifecycle refresh returns affected counts, and single-node
  sync plus `/agent/users` callers schedule global reconcile when counts change.
- Global enabled-node reconcile runs lifecycle refresh once per reconcile cycle,
  not once per node.
- If the global reconcile signal is unavailable after lifecycle changes, the
  degraded path is logged/tested instead of silently relying on periodic repair.
- Lifecycle-triggered global reconcile is not silently dropped by throttle; a
  throttled signal records pending work or visible degraded state.
- Transactional generation writes snapshot, desired version, and job together.
- `prepareUsersSyncTx` is used from finish/backfill transactions without nested
  transactions and always produces a full payload with the freshly materialized
  `target_version`.
- `xray_apply` success enqueues a forced full users sync with reason
  `xray_apply_followup`, even when applied and desired user versions already
  match.
- `xray_apply` accepted applied-version update and forced full users-sync
  follow-up happen in one repo/service finish transaction.
- Failed `xray_apply` that rolled back/reloaded runtime enqueues forced full
  users sync with reason `xray_apply_rollback_followup`.
- Retryable `xray_apply` failure that is requeued but already rolled
  back/reloaded runtime does not run before the forced full users-sync repair;
  either the apply is terminal failed or claim ordering runs users repair first.
- Successful `xray_rollback` marks rollback and enqueues forced full users sync
  with reason `xray_rollback_followup` in one transaction.
- Failed `xray_rollback` after restore/reload was attempted enqueues forced full
  users sync with reason `xray_runtime_unknown_followup`, and the requeued
  rollback does not claim before that repair.
- Timed-out running `xray_apply`/`xray_rollback` jobs enqueue forced full
  users-sync repair with reason `xray_runtime_unknown_followup` or persist a
  visible runtime-user-unknown flag that immediately triggers that repair.
- Timed-out managed Xray jobs with attempts remaining do not claim before the
  forced full users-sync repair; either they are terminal failed or claim
  ordering runs repair first.
- Rollout/backfill sends existing nodes one versioned full sync before allowing
  delta generation for that node.
- Startup or worker rollout hook scans every enabled sync-capable node with
  `users_sync_baseline_schema < 1` and idempotently enqueues one forced full
  `baseline_backfill` job without waiting for unrelated mutations.
- Rollout hook sets `users_sync_baseline_backfill_at` after enqueueing/finding
  an equivalent full backfill and does not enqueue a new startup backfill forever
  for old agents that cannot set the schema marker.
- Old-agent baseline success followed by later observed schema-1 capability or
  agent-version upgrade triggers a bounded backfill recheck even when
  desired/applied versions have not changed.
- Terminal failed or canceled `baseline_backfill` or equivalent full repair
  clears the backfill-at marker or records visible degraded rollout state with
  bounded retry.
- Delta mode selection requires `nodes.users_sync_baseline_schema=1`; matching
  desired/applied hashes alone are not enough.
- Schema-1 full finish from a new agent sets
  `nodes.users_sync_baseline_schema=1` only after accepted applied version and
  matching `result_json.target_version`.
- Old-format success on a schema-0 baseline backfill records legacy/degraded
  capability without immediately enqueueing another full loop; old-format or
  malformed success from a previously delta-ready node clears the marker and
  forces full backfill.
- Full-over-delta enqueue priority.
- Forced full enqueue removes or overwrites older pending deltas so the next
  claimed pending `users_sync` after a running job is the forced full.
- Forced full repair/backfill rewrites same-version ordinary pending full jobs
  with a protected repair reason, unless the pending full is already
  repair-compatible.
- Forced full repair/backfill overwrites or cancels unparsable/unknown pending
  users_sync payloads, while ordinary delta does not treat those invalid payloads
  as a protected full.
- Ordinary reconcile cancels/repairs claimable invalid pending users_sync
  payloads or records visible degraded queue state, so invalid payloads cannot
  become permanent queue poison pills.
- Forced full repair is not swallowed by a running delta; it leaves a pending
  full repair unless the running job is already full repair.
- Pending stale delta is replaced after an accepted running sync advances the
  applied version.
- Accepted users_sync success cancels redundant pending users_sync jobs whose
  `desired_version` equals the accepted applied version, but preserves protected
  repair/backfill full jobs with same version.
- Versioned `/agent/users` response and old-agent compatibility.
- Versioned `/agent/users` materializes users, writes snapshot, and updates
  `desired_users_version` atomically.
- Agent full sync saves `SyncedUsersVersion`.
- Agent full sync handles old panel response with no version.
- Agent schema-1 full response with non-empty `version` must hash-match the
  returned users before journaling, Xray mutation, state persistence, or
  returning `AppliedVersion`; mismatch is retryable and leaves state unchanged.
- Agent full response hash mismatch includes reason
  `full_response_hash_mismatch` in result JSON and does not enable the panel
  delta-ready marker.
- Agent schema-1 full result JSON reports `target_version` as the actual
  fetched/applied version, including stale full jobs where it differs from the
  job payload target.
- `StateStore.Update` is copy-on-write or transactional: failed disk writes do
  not mutate in-memory `State`.
- Agent full sync persists `PendingUsersSync` before Xray operations, clears it
  with the committed baseline on success, and uses retained
  `PossibleRuntimeUsers` for later stale-removal if previous attempts mutated
  Xray but failed to persist.
- Agent pending journal preserves `PossibleRuntimeUsers` across repeated failed
  attempts so stale removal covers users hot-added by older failed attempts.
- Agent full sync treats state persistence failure as retryable and does not
  return an applied version.
- Agent full sync applies an empty versioned response as cleanup and persists
  the empty-list version.
- Agent delta success updates Xray and persisted baseline/version.
- Agent delta persists a pending journal before Xray operations and clears it
  only with the committed baseline/version.
- Agent delta refuses unrelated pending journals and requests full repair.
- Agent delta base mismatch is non-retryable and requests full sync.
- Agent delta verifies `HashItems(SyncedUsers) == SyncedUsersVersion` before
  applying changes; mismatch requests full sync with `invalid_local_baseline`.
- Agent delta Xray operation failure is retryable and leaves state unchanged.
- Agent delta local hash mismatch is non-retryable and requests full sync.
- Agent delta malformed payload and remove-base-mismatch are non-retryable and
  request full sync.
- Agent schema-1 full payload with missing `target_version` is a permanent
  payload failure; legacy `{}` full remains accepted.
- Agent delta state persistence failure is retryable, leaves state unchanged,
  and does not return an applied version.
- Online snapshot, stats, access-log attribution, and IP-limit mapping use the
  shared effective synced-users helper and include committed users plus pending
  possible runtime users while a pending journal exists.
- Finish success mismatch does not update applied version and enqueues full.
- Finish success with empty `applied_version` does not update applied version
  and enqueues full.
- Finish legacy empty job desired accepts only when `applied_version` equals the
  node's current desired version.
- Finish terminal `need_full_sync=true` enqueues full.
- Finish accepted success cancels or overwrites stale pending deltas.
- Finish accepted-applied update, stale delta cleanup, forced full repair, and
  follow-up enqueue decisions happen through the repo/service finish entrypoint
  rather than best-effort handler writes.
- Snapshot pruning retains desired, applied, pending/running target, and pending
  delta base versions, while keeping at least the newest 10 snapshots per node.

## Acceptance Criteria

- `go test ./...` passes.
- A node with no baseline receives and applies a full users sync.
- A node with a matching baseline receives and applies a delta users sync.
- A mismatched or stale delta does not loop retries; it results in a forced full
  sync.
- `nodes.applied_users_version` is updated only after validated success.
- Restarted node-agent keeps its synced user baseline and can accept later delta
  jobs only when the baseline version matches.
