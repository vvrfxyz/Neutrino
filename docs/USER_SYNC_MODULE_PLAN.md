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
- Mismatched base, stale target, or local hash mismatch are not retryable delta
  errors. They should request a forced full sync instead of re-running the same
  broken delta.
- Xray API operation failures are retryable execution errors and must not update
  the persisted synced user baseline.
- Do not update `nodes.applied_users_version` unless the returned
  `applied_version` matches the finished job's desired version or the node's
  current desired user version.

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
	UUID   string `json:"uuid,omitempty"`
	Status string `json:"status"`
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
	Changes       []Change `json:"changes,omitempty"`
}
```

Add helpers:

- `HashItems(items []Item) string`
- `DiffItems(base, target []Item) (changes []Change, safe bool)`
- `ApplyChanges(base []Item, changes []Change) []Item`

Keep this package independent from `internal/repo` to avoid an import cycle.
The repo-to-sync-item adapter can live in `internal/repo` or
`internal/service`, but it must stay outside `internal/usersync`.

Hash compatibility:

- Use the same canonical fields as the existing `UsersDesiredVersion` and
  `hashUsers`: `user_id`, `email`, `status`, `uuid`.
- Sort by `user_id`.
- Preserve `omitempty` behavior for empty `uuid`.
- Replace the existing local hash implementations by delegating to
  `usersync.HashItems` so panel and agent cannot drift later.

Diff behavior:

- Produce only `upsert` and `remove`.
- Any item missing from target becomes `remove`.
- Any new item or changed `status`/`uuid` becomes `upsert`.
- If the same `user_id` has a changed `email`, return `safe=false`; the caller
  must enqueue a full job.

### 2. Persist per-node user snapshots

Files:

- `internal/db/db.go`
- `internal/repo/user_sync_snapshots.go`
- `internal/repo/user_sync_snapshots_test.go`

Add table:

```sql
CREATE TABLE IF NOT EXISTS node_user_sync_snapshots (
	node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
	version TEXT NOT NULL,
	users_json TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY(node_id, version)
);
```

Repository behavior:

- Upsert a snapshot by `(node_id, version)`.
- Read a snapshot by `(node_id, version)`.
- Decode `users_json` into `[]usersync.Item`.
- Query canonical node users directly as `[]usersync.Item` inside sync
  preparation transactions. This keeps the user read, target hash, snapshot,
  desired version, and job payload consistent.
- Prune old snapshots best-effort while retaining:
  - `nodes.desired_users_version`;
  - `nodes.applied_users_version`;
  - pending/running `users_sync.desired_version`;
  - every pending/running delta job `base_version`.
- If pruning cannot parse a pending/running job payload, keep snapshots rather
  than risking deletion of a needed delta baseline.

### 3. Add lifecycle refresh without sync side effects

Files:

- `internal/service/users.go` or a small lifecycle helper file
- callers in `internal/service/nodes.go`

Add a helper used by sync generation:

```go
func (s *UserService) RefreshLifecycleStateNoSync(ctx context.Context) error
```

If wiring `UserService` into `NodeService` would create an awkward dependency,
put the helper beside `NodeService` and call the store sweep methods there. The
important contract is no sync request side effect.

Behavior:

- Call `SweepExpiredUsers`.
- Call `SweepQuotaWindows`.
- Return errors.
- Do not call `RequestUsersSync`, `RequestUsersSyncNow`, or
  `RequestUsersSyncForNodeNow`.

Keep the existing `RefreshLifecycleState` behavior for UI/API paths that should
trigger sync after lifecycle changes.

### 4. Generate users_sync jobs transactionally

Files:

- `internal/service/nodes.go`
- `internal/repo/user_sync_jobs.go`
- tests near existing node job and node service tests

Replace the current `EnqueueUsersSyncForNode` implementation with a service
path backed by one store method similar to `DeployManagedXray`, for example:

```go
func (s *Store) PrepareUsersSync(ctx context.Context, nodeID int64, forceFull bool, reason string) (jobID int64, enqueued bool, targetVersion string, err error)
```

Behavior:

1. Runs the no-side-effect lifecycle refresh.
2. In one repository transaction:
   - read the node;
   - read users allowed for that node directly into canonical
     `[]usersync.Item`;
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
- Use full for explicit repair requests.
- Use full when `DiffItems` returns `safe=false`.
- Use delta only when the applied snapshot exists and the diff is safe.
- If `applied_users_version == targetVersion`, update desired/snapshot as
  needed and do not enqueue a job for ordinary reconcile. This applies to
  disabled nodes too; once the empty target has been applied, no repeated
  cleanup job is needed.
- If `forceFull=true`, enqueue a full job even when
  `applied_users_version == targetVersion`, because the agent has explicitly
  reported that its local baseline needs repair.

Structured payloads:

- Full:

```json
{
  "schema": 1,
  "mode": "full",
  "target_version": "<targetVersion>",
  "reason": "<reason>"
}
```

- Delta:

```json
{
  "schema": 1,
  "mode": "delta",
  "base_version": "<appliedVersion>",
  "target_version": "<targetVersion>",
  "changes": []
}
```

Keep `{}` as the legacy full payload for compatibility in the agent parser.

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
- Newer delta may overwrite older pending delta for the same node.
- Treat empty, `{}`, unparsable, or unknown pending users_sync payloads as full
  for overwrite decisions. This preserves legacy pending jobs during rollout.
- A running users_sync job is left alone; if its result becomes stale, finish
  validation or the next reconcile will enqueue repair.

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
- Use the current desired snapshot when available.
- If no snapshot exists, materialize the current canonical snapshot and desired
  version without enqueueing another job, then respond with it.
- Existing agents that decode only `users` continue to work because extra fields
  are ignored.
- New agents must treat a missing `version` as old-panel compatibility and use
  the full path only.

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
```

Behavior:

- Full sync success persists both `SyncedUsers` and `SyncedUsersVersion`.
- Delta sync success applies changes to the persisted baseline in memory, checks
  `usersync.HashItems(next) == target_version`, then persists both users and
  version.
- Any failed sync attempt leaves both `SyncedUsers` and `SyncedUsersVersion`
  unchanged.
- State loading should accept old state files with no version; those agents must
  do a full sync before any delta can be accepted.

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

Full behavior:

- Fetch versioned users from the panel.
- Apply with the existing full sync semantics.
- Persist users and the response version.
- Return `AppliedVersion=response.version`.
- If the panel response has no version, compute the local hash and return that
  hash for old-panel compatibility.

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

3. Apply each change through the Xray API:
   - `upsert` with active `uuid` calls `UpsertUser`.
   - `upsert` for inactive or empty `uuid` calls `RemoveUser`.
   - `remove` calls `RemoveUser`.
4. If any Xray operation fails, return retryable failure and do not update
   state.
5. Build the next local user list with `ApplyChanges`.
6. Verify the local hash equals `payload.TargetVersion`.
7. If the hash mismatches, return non-retryable failure with
   `need_full_sync=true` and reason `local_hash_mismatch`.
8. Persist users and `SyncedUsersVersion=payload.TargetVersion`.
9. Return `AppliedVersion=payload.TargetVersion`.

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

When a `users_sync` job finishes:

- Call `FinishNodeJobForNode` as today to preserve retry behavior.
- Use the returned `finalStatus` for all follow-up decisions. If
  `finalStatus == "pending"`, stop there; the retry is already scheduled.
- If it is a succeeded users_sync result:
  - accept `applied_version` only when it equals the job's `desired_version` or
    the node's current `desired_users_version`;
  - then update `nodes.applied_users_version`;
  - if the accepted version is not the node's current desired version, enqueue a
    follow-up users sync so stale successful work converges immediately;
  - otherwise do not update applied, and enqueue a forced full users sync.
- If it is a terminal failed users_sync result whose result JSON contains
  `need_full_sync=true`, enqueue a forced full users sync immediately.
- If the failure is retryable and `FinishNodeJobForNode` requeued it as pending,
  do not enqueue forced full yet.

Add an explicit service entrypoint:

```go
func (s *NodeService) EnqueueFullUsersSyncForNode(ctx context.Context, nodeID int64, reason string) error
```

This must always produce a full payload and must obey full-over-delta priority.

### 10. Keep restore and online snapshot behavior coherent

Files:

- `internal/agent/agent.go`
- `internal/agent/online_snapshot_test.go`

Behavior:

- Startup restore may continue using `SyncedUsers` to repopulate Xray.
- Online snapshot collection continues to read `SyncedUsers`.
- Delta success must update `SyncedUsers` exactly as full sync would, so online
  snapshot user mapping remains correct.

### 11. Tests

Add or update tests for:

- `usersync.HashItems` matches existing `UsersDesiredVersion`/`hashUsers`
  canonical output.
- `usersync.DiffItems` and `ApplyChanges` for add, remove, status change, UUID
  change, and email change forcing full.
- No-side-effect lifecycle refresh runs sweeps but does not request sync.
- Transactional generation writes snapshot, desired version, and job together.
- Full-over-delta enqueue priority.
- Versioned `/agent/users` response and old-agent compatibility.
- Agent full sync saves `SyncedUsersVersion`.
- Agent full sync handles old panel response with no version.
- Agent delta success updates Xray and persisted baseline/version.
- Agent delta base mismatch is non-retryable and requests full sync.
- Agent delta Xray operation failure is retryable and leaves state unchanged.
- Agent delta local hash mismatch is non-retryable and requests full sync.
- Finish success mismatch does not update applied version and enqueues full.
- Finish terminal `need_full_sync=true` enqueues full.
- Snapshot pruning retains desired, applied, pending/running target, and pending
  delta base versions.

## Acceptance Criteria

- `go test ./...` passes.
- A node with no baseline receives and applies a full users sync.
- A node with a matching baseline receives and applies a delta users sync.
- A mismatched or stale delta does not loop retries; it results in a forced full
  sync.
- `nodes.applied_users_version` is updated only after validated success.
- Restarted node-agent keeps its synced user baseline and can accept later delta
  jobs only when the baseline version matches.
