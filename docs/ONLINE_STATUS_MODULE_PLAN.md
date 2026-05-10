# Online Status Module Plan

## Goal

Make `node-agent` read the current authoritative online IP set from Xray
runtime and report it through the existing node report API. The panel applies
that snapshot atomically to `online_sessions`; `/ops`, user detail pages, and
IP-limit enforcement continue to read `online_sessions`.

## Data Flow

```text
Xray online stats API
  -> node-agent online snapshot
  -> POST /api/v1/nodes/{id}/report
  -> repo.ApplyOnlineSnapshot
  -> online_sessions
  -> /ops, user detail, EnforceIPLimit
```

## Implementation Steps

### 1. Add Xray online IP query support

File:

- `internal/xrayapi/client.go`

Add:

```go
type OnlineIP struct {
	IP         string
	LastSeenAt time.Time
}

func (c *Client) PullOnlineIPs(ctx context.Context, email string) ([]OnlineIP, error)
```

Behavior:

- Call Xray gRPC `GetStatsOnlineIpList`.
- Use stat name `user>>>EMAIL>>>online`.
- Convert response `ips` map values into `time.Time`.
- Validate each IP with `net.ParseIP`.
- Return an empty slice for not-found stats.
- Return an error for API, dial, malformed timestamp, or other unexpected
  failures.

### 2. Extend node report payload

File:

- `internal/agent/panel_client.go`

Add:

```go
type OnlineSnapshot struct {
	ObservedAt string               `json:"observed_at"`
	Items      []OnlineSnapshotItem `json:"items"`
}

type OnlineSnapshotItem struct {
	UserID     int64  `json:"user_id"`
	Email      string `json:"email,omitempty"`
	ClientIP   string `json:"client_ip"`
	LastSeenAt string `json:"last_seen_at"`
}
```

Add to `NodeReportPayload`:

```go
OnlineSnapshot *OnlineSnapshot `json:"online_snapshot,omitempty"`
```

### 3. Build online snapshot in node-agent

File:

- `internal/agent/agent.go`

Behavior:

- During heartbeat/report payload construction, use the current synced user
  set as the scan list.
- For each synced user email, call `xrayapi.PullOnlineIPs`.
- Map every online IP back to the panel `user_id`.
- Build one `OnlineSnapshot` with `observed_at` and all online items.
- Attach `OnlineSnapshot` only when the full collection succeeds.
- If collection fails, log the error clearly and omit `online_snapshot` from
  that report.

### 4. Add repo snapshot application

File:

- `internal/repo/online_sessions.go`

Add:

```go
type OnlineSnapshotInput struct {
	ObservedAt time.Time
	Items      []OnlineSnapshotItemInput
}

type OnlineSnapshotItemInput struct {
	UserID     int64
	ClientIP   string
	LastSeenAt time.Time
}

func (s *Store) ApplyOnlineSnapshot(ctx context.Context, nodeID int64, in OnlineSnapshotInput) error
```

Transaction behavior:

- Require `nodeID > 0`.
- Normalize `observed_at` to UTC; use `time.Now().UTC()` if it is zero.
- For every item:
  - require `user_id > 0`;
  - validate `client_ip` with `net.ParseIP`;
  - use `observed_at` if `last_seen_at` is zero or outside a reasonable report
    window;
  - upsert `(user_id, client_ip, node_id)` into `online_sessions`.
- Delete prior `online_sessions` rows for the same `node_id` whose
  `(user_id, client_ip)` pair is not present in this successful snapshot.
- Commit all snapshot changes atomically.

### 5. Extend panel report input and apply path

File:

- `internal/repo/node_report.go`

Add to `NodeReportInput`:

```go
OnlineSnapshot *OnlineSnapshotInput `json:"online_snapshot,omitempty"`
```

In `ApplyNodeReportLatest`:

- Continue applying metrics and static facts as today.
- If `OnlineSnapshot != nil`, call:

```go
s.ApplyOnlineSnapshot(ctx, nodeID, *in.OnlineSnapshot)
```

### 6. Ensure report handler accepts the snapshot

File:

- `internal/app/handlers_nodes.go`

Behavior:

- Keep using the existing `POST /api/v1/nodes/{id}/report` route.
- Ensure JSON decode into `repo.NodeReportInput` accepts
  `online_snapshot`.
- Return an error if applying the snapshot fails.

### 7. Make usage ingestion stop writing online state

File:

- `internal/repo/usage.go`

Behavior:

- Stop updating `online_sessions` from `RecordUsage`.
- Keep usage accounting and idempotency behavior unchanged.
- Keep access-log-derived usage events as usage events only.

### 8. Tests

Add or update tests for:

- `xrayapi.PullOnlineIPs`:
  - returns parsed IPs and timestamps;
  - returns empty result for not-found stats;
  - rejects invalid IP or malformed timestamp.
- `repo.ApplyOnlineSnapshot`:
  - upserts current online IPs;
  - deletes stale IPs from the same node after a successful snapshot;
  - does not touch other nodes;
  - validates invalid node, user, and IP inputs consistently.
- `repo.EnforceIPLimit`:
  - triggers from snapshot-written `online_sessions`.
- `repo.RecordUsage`:
  - does not update `online_sessions`.
- `app` node report:
  - accepts `online_snapshot`;
  - writes rows that `/api/v1/online-users` can read.

## Acceptance Criteria

- `go test ./...` passes.
- A successful node report with `online_snapshot` updates `online_sessions`.
- `/ops` shows online users from snapshot-written rows.
- User detail pages show that user's online IPs from snapshot-written rows.
- Existing IP-limit enforcement works from snapshot-written rows.
- Xray online collection failure is logged by node-agent and does not silently
  update `online_sessions`.
