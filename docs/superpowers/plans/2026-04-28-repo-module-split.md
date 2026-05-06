# Repo Module Split Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

> **Status update — 2026-05-06:** Code implementation is complete. Phase 1 repo splitting is reflected in the current tree (`extensions.go` has been removed, `store.go` is down to the shared core, and domain files exist for users, usage, subscriptions, traffic, online sessions, nodes, API keys, backups, and ops queries). Phase 2 service wiring is also complete: `internal/service/{users,usage,nodes,ops}.go` now owns lifecycle, user mutations, usage batch recording, node sync/job/managed-Xray orchestration, and ops aggregation used by handlers. Validation: `go test ./...` passes. Historical per-step commit commands below were not executed as part of this status update unless a separate commit is requested.

**Goal:** Split the monolithic `internal/repo/store.go` (2407 LOC) and `internal/repo/extensions.go` (2222 LOC) into focused domain files, then introduce a thin `internal/service` layer that owns business orchestration.

**Architecture:** Two phases. Phase 1 keeps everything in `package repo` but splits the two megafiles into domain-focused files. Phase 2 introduces `internal/service/{users,usage,nodes,ops}.go` that extract orchestration logic currently living in `internal/app/app.go`. Dependencies flow: `app → service → repo`. No interface abstractions yet — concrete `*Store` and `*Service` types.

**Tech Stack:** Go 1.22+, SQLite, existing test harness (`go test ./...`)

**Guiding Principles:**
- Split by "who owns the rules" — not by UI page
- `usage` owns billing/quota/enforcement rules
- `node` owns orchestration/job/cert rules
- `user` owns lifecycle/subscription rules
- `ops` is a read-only observation window
- Every step must pass `go test ./...` and `go build ./...`

## Completion audit — 2026-05-06

| Area | Current status |
|---|---|
| Phase 1 repo split | Complete. `internal/repo/store.go` is a shared core file, and domain behavior has been moved into focused files including `users.go`, `usage.go`, `traffic.go`, `subscriptions.go`, `online_sessions.go`, `nodes.go`, `api_keys.go`, `backup.go`, and `ops_queries.go`. |
| `extensions.go` cleanup | Complete. The file no longer exists in `internal/repo/`. |
| Phase 2 service layer | Complete. `internal/service/users.go`, `usage.go`, `nodes.go`, and `ops.go` are wired through `App` service accessors. |
| App responsibility boundary | Updated. HTTP handlers retain parsing, response shaping, template rendering, and audit logging; multi-step user lifecycle/sync, usage batch side effects, node sync/job/managed-Xray orchestration, and ops aggregation are handled by services. |
| Verification | `go test ./...` passed on 2026-05-06. |

---

## Phase 1: Split `internal/repo` into domain files

The goal is to take `store.go` (2407 LOC) and `extensions.go` (2222 LOC) and distribute their functions into focused files while keeping `package repo` unchanged. No API changes, no renames — pure file reorganization.

### Target file layout after Phase 1

```
internal/repo/
  store.go            # Store struct, New(), models, shared helpers (scanUserRow, etc.)
  users.go            # User CRUD, status transitions, proxy links, GetUserByUsername
  subscriptions.go    # Subscription tokens, Telegram bindings
  usage.go            # RecordUsage*, idempotency, quota windows, enforcement, alerts
  online_sessions.go  # Online users, IP limit enforcement, session touch/sweep
  traffic.go          # TrafficSeries, TrafficSummary, rollup, prune
  nodes.go            # Node CRUD, ListNodes, desired states (moved from node_desired_states.go)
  node_jobs.go        # (already exists — keep as-is)
  node_certs.go       # (already exists — keep as-is)
  node_enroll.go      # (already exists — keep as-is)
  node_report.go      # (already exists — keep as-is)
  node_ops.go         # (already exists — keep as-is)
  node_monthly_usage.go  # (already exists — keep as-is)
  node_runtime_metrics.go # (already exists — keep as-is)
  host_net_usage.go   # (already exists — keep as-is)
  api_keys.go         # API key create/list/revoke/validate
  audit.go            # (already exists — keep as-is)
  auth.go             # (already exists — keep as-is)
  state.go            # (already exists — keep as-is)
  backup.go           # Backup record CRUD
  ops_queries.go      # ListAlerts, ListEnforcementLogs, GetTrafficTotals
  helpers.go          # Shared unexported helpers: nullableString, parseOptionalRFC3339, etc.
```

---

### Task 1: Extract shared models and helpers into `store.go` core

Reduce `store.go` to only: struct definitions, `New()`, the big `usersSelectQuery`, `scanUserRow`, and general shared helpers. Move all business methods out.

**Files:**
- Modify: `internal/repo/store.go`
- Create: `internal/repo/helpers.go`

- [ ] **Step 1: Create `helpers.go` with shared unexported utilities**

Move these functions from `store.go` to a new `helpers.go`:
- `nullableString`
- `nullableInt64`
- `trafficBucketKey`
- `nextLocalDayStart`
- `quotaWindowBounds`

```go
// internal/repo/helpers.go
package repo

import (
	"database/sql"
	"fmt"
	"time"
)

func nullableString(v string) sql.NullString {
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

func nullableInt64(v int64) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}

func trafficBucketKey(t time.Time, period string) string {
	switch period {
	case "hour":
		return t.Truncate(time.Hour).Format(time.RFC3339)
	case "day":
		y, m, d := t.Date()
		return fmt.Sprintf("%04d-%02d-%02dT00:00:00Z", y, m, d)
	default:
		return t.Truncate(time.Hour).Format(time.RFC3339)
	}
}

func nextLocalDayStart(loc *time.Location, day time.Time) time.Time {
	y, m, d := day.In(loc).Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, loc).UTC()
}

func quotaWindowBounds(cycleType, tz string, t time.Time) (time.Time, time.Time, string) {
	// ... exact copy from store.go lines 1659-1688
}
```

Note: Copy the full implementation of `quotaWindowBounds` verbatim from `store.go:1659-1688`.

- [ ] **Step 2: Remove moved functions from `store.go`**

Delete the function bodies of `nullableString`, `nullableInt64`, `trafficBucketKey`, `nextLocalDayStart`, `quotaWindowBounds` from `store.go` (lines 1834-1870 approx).

- [ ] **Step 3: Verify build and tests pass**

Run:
```bash
go build ./... && go test ./internal/repo/...
```
Expected: PASS (same package, no API change)

- [ ] **Step 4: Commit**

```bash
git add internal/repo/helpers.go internal/repo/store.go
git commit -m "refactor(repo): extract shared helpers to helpers.go"
```

---

### Task 2: Extract user lifecycle methods into `users.go`

**Files:**
- Modify: `internal/repo/store.go`
- Create: `internal/repo/users.go`

- [ ] **Step 1: Create `users.go` with user lifecycle functions**

Move these functions from `store.go` to `users.go`:
- `ListUsers` (line 476)
- `ListUsersForNode` (line 455)
- `GetUser` (line 496)
- `GetUserByUsername` (from `extensions.go:1654`)
- `CreateUser` (line 734)
- `normalizeCreateUserInput` (line 802)
- `CreateProxyLink` (line 840)
- `SetUserStatus` (line 896)
- `activateLatestProxyLinkTx` (line 991)
- `DeleteUser` (line 1047)
- `buildVLESSLink` (line 1638)
- `normalizeNodeIDs` (from `extensions.go:2041`)
- `validateNodeIDsTx` (from `extensions.go:2061`)
- `SetUserNodeAccess` (from `extensions.go:2113`)
- `setUserNodeAccessTx` (from `extensions.go:2145`)
- `ListUserNodeIDs` (from `extensions.go:2164`)
- `ListEnabledNodesForUser` (from `extensions.go:2183`)

Keep in `store.go`: the `usersSelectQuery` constant and `scanUserRow` (they're shared by users.go, usage.go, traffic.go).

```go
// internal/repo/users.go
package repo

import (
	"context"
	// ... required imports
)

// User lifecycle: CRUD, status, proxy links, node access
```

- [ ] **Step 2: Remove moved functions from `store.go` and `extensions.go`**

- [ ] **Step 3: Verify build and tests**

```bash
go build ./... && go test ./...
```

- [ ] **Step 4: Commit**

```bash
git add internal/repo/users.go internal/repo/store.go internal/repo/extensions.go
git commit -m "refactor(repo): extract user lifecycle to users.go"
```

---

### Task 3: Extract subscription and Telegram binding into `subscriptions.go`

**Files:**
- Modify: `internal/repo/extensions.go`
- Create: `internal/repo/subscriptions.go`

- [ ] **Step 1: Create `subscriptions.go`**

Move from `extensions.go`:
- `createSubscriptionTokenTx` (line 67)
- `getSubscriptionTokenByUserTx` (line 90)
- `GetOrCreateSubscriptionToken` (line 117)
- `GetSubscriptionTokenByUserID` (line 140)
- `RotateSubscriptionToken` (line 167)
- `SetSubscriptionTokenEnabled` (line 201)
- `GetUserBySubscriptionToken` (line 214)
- `scanTelegramBindingRow` (line 265)
- `ensureTelegramBindingTx` (line 304)
- `newTelegramBindCode` (line 361)
- `setTelegramBindCodeTx` (line 369)
- `recordTelegramBindAttemptTx` (line 402)
- `EnsureTelegramBinding` (line 458)
- `GetTelegramBindingByUserID` (line 474)
- `RegenerateTelegramBindCode` (line 485)
- `BindTelegramChatByCode` (line 514)
- `GetUserByTelegramChatID` (line 589)

Also move the related constants: `telegramBindCodeBytes`, `telegramBindCodeTTL`, `telegramBindAttemptWindow`, `telegramBindMaxAttempts`, `telegramBindAttemptGCGrace`.

- [ ] **Step 2: Remove moved code from `extensions.go`**

- [ ] **Step 3: Verify build and tests**

```bash
go build ./... && go test ./...
```

- [ ] **Step 4: Commit**

```bash
git add internal/repo/subscriptions.go internal/repo/extensions.go
git commit -m "refactor(repo): extract subscriptions and telegram bindings"
```

---

### Task 4: Extract usage, quota, and enforcement into `usage.go`

This is the most critical split — the billing/enforcement core.

**Files:**
- Modify: `internal/repo/store.go`
- Create: `internal/repo/usage.go`

- [ ] **Step 1: Create `usage.go`**

Move from `store.go`:
- `RecordUsage` (line 1076)
- `normalizeUsageInput` (line 1182)
- `loadUserUsageMetaTx` (line 1204)
- `validateUsageWriteTimestamp` (line 1237)
- `usageAllowedForUserState` (line 1249)
- `ensureUserAllowedOnNodeTx` (line 1265)
- `usageEventExistsTx` (line 1288)
- `reserveUsageEventKeyTx` (line 1305)
- `ensureQuotaWindowTx` (line 1325)
- `applyUsageEventTx` (line 1366)
- `recordTrafficRollupHourlyTx` (line 1440)
- `updateTrafficStats` (line 1475)
- `updateQuotaWindow` (line 1540)
- `enforceLimit` (line 1589)
- `RecordUsageIdempotent` (line 1865)
- `RecordUsageBatchIdempotent` (line 1923)

Move from `extensions.go`:
- `SweepQuotaWindows` (line 1075)
- `ResetUserQuota` (line 1121)
- `CreditUserQuota` (line 1170)
- `reactivateOverLimitUserIfWithinQuotaTx` (line 1217)
- `reactivateOverLimitUserForNewCycleTx` (from `store.go:1015`)
- `ExtendUserPlan` (line 1285)
- `queueQuotaAlertsTx` (line 1343)
- `ListPendingAlerts` (line 1395)
- `MarkAlertSent` (line 1432)
- `SweepExpiredUsers` (from `store.go:1126`)

Also move: `usageMaxFutureSkew`, `usageMaxActiveBackdate` constants.

- [ ] **Step 2: Remove moved code from `store.go` and `extensions.go`**

- [ ] **Step 3: Verify build and tests**

```bash
go build ./... && go test ./...
```

- [ ] **Step 4: Commit**

```bash
git add internal/repo/usage.go internal/repo/store.go internal/repo/extensions.go
git commit -m "refactor(repo): extract usage/quota/enforcement to usage.go"
```

---

### Task 5: Extract online sessions into `online_sessions.go`

**Files:**
- Modify: `internal/repo/extensions.go`
- Create: `internal/repo/online_sessions.go`

- [ ] **Step 1: Create `online_sessions.go`**

Move from `extensions.go`:
- `touchOnlineSessionTx` (line 1046)
- `ListOnlineUsers` (line 1441)
- `ListUserOnlineStats` (line 1476)
- `ListUserOnlineSessions` (line 1502)
- `EnforceIPLimit` (line 1540)
- `PruneOnlineSessions` (line 1733)

Move `UserOnlineStats` struct definition from `extensions.go` top.

- [ ] **Step 2: Remove moved code from `extensions.go`**

- [ ] **Step 3: Verify build and tests**

```bash
go build ./... && go test ./...
```

- [ ] **Step 4: Commit**

```bash
git add internal/repo/online_sessions.go internal/repo/extensions.go
git commit -m "refactor(repo): extract online sessions to online_sessions.go"
```

---

### Task 6: Extract traffic queries into `traffic.go`

**Files:**
- Modify: `internal/repo/store.go`
- Create: `internal/repo/traffic.go`

- [ ] **Step 1: Create `traffic.go`**

Move from `store.go`:
- `ListUserEvents` (line 518)
- `ListUserEventsFiltered` (line 578)
- `GetTrafficSeries` (line 653)
- `GetTrafficSummary` (line 2076)
- `getTrafficSummaryAt` (line 2080)
- `trafficSummaryBucketExpr` (line 2339)
- `trafficSummaryRange` (line 2353)

Move from `extensions.go`:
- `PruneTrafficEvents` (line 1680)

- [ ] **Step 2: Remove from source files**

- [ ] **Step 3: Verify build and tests**

```bash
go build ./... && go test ./...
```

- [ ] **Step 4: Commit**

```bash
git add internal/repo/traffic.go internal/repo/store.go internal/repo/extensions.go
git commit -m "refactor(repo): extract traffic queries to traffic.go"
```

---

### Task 7: Extract nodes CRUD into `nodes.go`

**Files:**
- Modify: `internal/repo/store.go`, `internal/repo/extensions.go`
- Create: `internal/repo/nodes.go`

- [ ] **Step 1: Create `nodes.go`**

Move from `store.go`:
- `UpdateNodeLastSeen` (line 286)
- `UpdateNodeAgentStatus` (line 296)
- `GetNode` (line 312)

Move from `extensions.go`:
- `normalizeNodeInput` (line 609)
- `scanNode` (line 682)
- `ListNodes` (line 747)
- `ListEnabledNodes` (line 774)
- `ListManagedNodes` (line 801)
- `CreateNode` (line 828)
- `UpdateNode` (line 863)
- `DeleteNode` (line 897)
- `NodeDeleteWouldWidenAccess` (line 945)
- `listNodeDeleteBlockedUsernamesTx` (line 968)
- `DeleteStaleNodes` (line 996)

- [ ] **Step 2: Remove from source files**

- [ ] **Step 3: Verify build and tests**

```bash
go build ./... && go test ./...
```

- [ ] **Step 4: Commit**

```bash
git add internal/repo/nodes.go internal/repo/store.go internal/repo/extensions.go
git commit -m "refactor(repo): extract node CRUD to nodes.go"
```

---

### Task 8: Extract API keys and backup into dedicated files

**Files:**
- Modify: `internal/repo/extensions.go`
- Create: `internal/repo/api_keys.go`, `internal/repo/backup.go`, `internal/repo/ops_queries.go`

- [ ] **Step 1: Create `api_keys.go`**

Move from `extensions.go`:
- `hashAPIKey` (line 51)
- `CreateAPIKey` (line 1768)
- `ListAPIKeys` (line 1802)
- `RevokeAPIKey` (line 1833)
- `ValidateAPIKey` (line 1842)

Move `APIKeyAuth` struct.

- [ ] **Step 2: Create `backup.go`**

Move from `extensions.go`:
- `InsertBackupRecord` (line 1889)
- `ListBackups` (line 1908)
- `GetBackup` (line 1937)

- [ ] **Step 3: Create `ops_queries.go`**

Move from `extensions.go`:
- `ListAlerts` (line 1954)
- `ListEnforcementLogs` (line 1996)
- `GetTrafficTotals` (line 2027)

- [ ] **Step 4: Remove all moved code from `extensions.go`**

- [ ] **Step 5: Verify build and tests**

```bash
go build ./... && go test ./...
```

- [ ] **Step 6: Commit**

```bash
git add internal/repo/api_keys.go internal/repo/backup.go internal/repo/ops_queries.go internal/repo/extensions.go
git commit -m "refactor(repo): extract api_keys, backup, ops_queries"
```

---

### Task 9: Verify `extensions.go` and `store.go` are now minimal

After Tasks 1–8, verify:
- `store.go` contains only: `Store` struct, `New()`, model definitions, `usersSelectQuery`, `scanUserRow`, `RawDB()`
- `extensions.go` contains only: `RawDB()` and `parseOptionalRFC3339` (or is empty/deletable)

**Files:**
- Modify: `internal/repo/store.go`, `internal/repo/extensions.go`

- [ ] **Step 1: Audit remaining contents**

```bash
grep -n "^func " internal/repo/store.go
grep -n "^func " internal/repo/extensions.go
wc -l internal/repo/store.go internal/repo/extensions.go
```

Expected: `store.go` ~500 LOC (models + scan helpers), `extensions.go` ~50 LOC or deletable.

- [ ] **Step 2: Move any remaining `extensions.go` utilities to `helpers.go`**

If `parseOptionalRFC3339`, `randomTokenHex` remain — move them to `helpers.go`.

- [ ] **Step 3: Delete `extensions.go` if empty**

If all functions have been moved and only the `package repo` declaration remains, delete the file.

- [ ] **Step 4: Full test suite**

```bash
go test ./...
```

- [ ] **Step 5: Commit**

```bash
git add internal/repo/
git commit -m "refactor(repo): phase 1 complete — remove empty extensions.go"
```

---

## Phase 2: Introduce `internal/service` layer

Extract business orchestration from `internal/app/app.go` (962 LOC) into service objects. The `app` package keeps HTTP routing, template rendering, and middleware. Services own multi-step business logic.

### Target layout

```
internal/service/
  users.go      # User create/update/delete orchestration, sync trigger
  usage.go      # Usage recording orchestration, enforcement dispatch
  nodes.go      # Node lifecycle, job dispatch, reconciliation
  ops.go        # Aggregation queries for ops dashboard
```

---

### Task 10: Create `internal/service/users.go`

Extract user-mutating orchestration from `app.go`.

**Files:**
- Create: `internal/service/users.go`
- Modify: `internal/app/app.go`

- [ ] **Step 1: Define `UserService` struct**

```go
// internal/service/users.go
package service

import (
	"context"
	"neutrino/internal/repo"
)

type SyncRequester interface {
	RequestUsersSync(ctx context.Context)
	RequestUsersSyncNow(ctx context.Context)
}

type UserService struct {
	store *repo.Store
	sync  SyncRequester
}

func NewUserService(store *repo.Store, sync SyncRequester) *UserService {
	return &UserService{store: store, sync: sync}
}
```

- [ ] **Step 2: Move `refreshUserLifecycleState` logic**

Extract `app.go:749` (`refreshUserLifecycleState`) into `UserService`:

```go
func (s *UserService) RefreshLifecycleState(ctx context.Context) error {
	expired, err := s.store.SweepExpiredUsers(ctx)
	if err != nil {
		return err
	}
	reactivated, err := s.store.SweepQuotaWindows(ctx)
	if err != nil {
		return err
	}
	if expired > 0 || reactivated > 0 {
		s.sync.RequestUsersSyncNow(ctx)
	}
	return nil
}
```

- [ ] **Step 3: Wire into `App` — replace inline call**

In `app.go`, replace the inline sweep logic in `StartWorkers` with:

```go
if err := a.userService.RefreshLifecycleState(ctx); err != nil {
    log.Printf("lifecycle refresh error: %v", err)
}
```

- [ ] **Step 4: Verify build and tests**

```bash
go build ./... && go test ./...
```

- [ ] **Step 5: Commit**

```bash
git add internal/service/users.go internal/app/app.go
git commit -m "refactor: introduce UserService with lifecycle orchestration"
```

---

### Task 11: Create `internal/service/nodes.go`

Extract node orchestration — reconciler trigger, job dispatch, stale cleanup.

**Files:**
- Create: `internal/service/nodes.go`
- Modify: `internal/app/app.go`

- [ ] **Step 1: Define `NodeService` struct**

```go
// internal/service/nodes.go
package service

import (
	"context"
	"log"
	"time"

	"neutrino/internal/repo"
)

type NodeService struct {
	store              *repo.Store
	staleDeleteAfterSec int
	sync               SyncRequester
}

func NewNodeService(store *repo.Store, staleDeleteSec int, sync SyncRequester) *NodeService {
	return &NodeService{store: store, staleDeleteAfterSec: staleDeleteSec, sync: sync}
}
```

- [ ] **Step 2: Extract `DeleteStaleNodes` orchestration**

```go
func (s *NodeService) CleanupStaleNodes(ctx context.Context) error {
	if s.staleDeleteAfterSec <= 0 {
		return nil
	}
	cutoff := time.Now().UTC().Add(-time.Duration(s.staleDeleteAfterSec) * time.Second)
	disabled, err := s.store.DeleteStaleNodes(ctx, cutoff)
	if err != nil {
		return err
	}
	if len(disabled) > 0 {
		log.Printf("disabled stale nodes pending cleanup: %v (cutoff=%s)", disabled, cutoff.Format(time.RFC3339))
		for _, nodeID := range disabled {
			// trigger per-node sync
			_ = nodeID // will use sync.RequestUsersSyncForNode once wired
		}
	}
	return nil
}
```

- [ ] **Step 3: Wire into `App.StartWorkers`**

Replace the inline stale-node block with `a.nodeService.CleanupStaleNodes(ctx)`.

- [ ] **Step 4: Verify build and tests**

```bash
go build ./... && go test ./...
```

- [ ] **Step 5: Commit**

```bash
git add internal/service/nodes.go internal/app/app.go
git commit -m "refactor: introduce NodeService with stale cleanup"
```

---

### Task 12: Create `internal/service/usage.go`

Extract IP limit enforcement orchestration.

**Files:**
- Create: `internal/service/usage.go`
- Modify: `internal/app/app.go`

- [ ] **Step 1: Define `UsageService` struct**

```go
// internal/service/usage.go
package service

import (
	"context"

	"neutrino/internal/repo"
)

type UsageService struct {
	store           *repo.Store
	onlineWindowSec int
	ipLimitStrikes  int
	sync            SyncRequester
}

func NewUsageService(store *repo.Store, onlineWindowSec, ipLimitStrikes int, sync SyncRequester) *UsageService {
	return &UsageService{
		store:           store,
		onlineWindowSec: onlineWindowSec,
		ipLimitStrikes:  ipLimitStrikes,
		sync:            sync,
	}
}
```

- [ ] **Step 2: Extract IP limit enforcement**

```go
func (s *UsageService) EnforceIPLimits(ctx context.Context) error {
	affected, err := s.store.EnforceIPLimit(ctx, s.onlineWindowSec, s.ipLimitStrikes)
	if err != nil {
		return err
	}
	if len(affected) > 0 {
		s.sync.RequestUsersSync(ctx)
	}
	return nil
}
```

- [ ] **Step 3: Wire into `App.StartWorkers`**

Replace inline `EnforceIPLimit` block.

- [ ] **Step 4: Verify build and tests**

```bash
go build ./... && go test ./...
```

- [ ] **Step 5: Commit**

```bash
git add internal/service/usage.go internal/app/app.go
git commit -m "refactor: introduce UsageService with IP enforcement"
```

---

### Task 13: Create `internal/service/ops.go`

Thin aggregation layer for the ops dashboard.

**Files:**
- Create: `internal/service/ops.go`

- [ ] **Step 1: Define `OpsService`**

```go
// internal/service/ops.go
package service

import (
	"context"

	"neutrino/internal/repo"
)

type OpsService struct {
	store *repo.Store
}

func NewOpsService(store *repo.Store) *OpsService {
	return &OpsService{store: store}
}

type OpsSummary struct {
	Alerts         []repo.AlertListItem
	Enforcements   []repo.EnforcementLog
	TotalInbound   int64
	TotalOutbound  int64
}

func (s *OpsService) GetSummary(ctx context.Context, alertLimit, enfLimit int) (OpsSummary, error) {
	alerts, err := s.store.ListAlerts(ctx, alertLimit)
	if err != nil {
		return OpsSummary{}, err
	}
	enfs, err := s.store.ListEnforcementLogs(ctx, enfLimit)
	if err != nil {
		return OpsSummary{}, err
	}
	inb, outb, err := s.store.GetTrafficTotals(ctx)
	if err != nil {
		return OpsSummary{}, err
	}
	return OpsSummary{
		Alerts:        alerts,
		Enforcements:  enfs,
		TotalInbound:  inb,
		TotalOutbound: outb,
	}, nil
}
```

- [ ] **Step 2: Wire into `handleOps` handler**

In `app.go` or `handlers_ops.go`, replace direct store calls with `a.opsService.GetSummary(...)`.

- [ ] **Step 3: Verify build and tests**

```bash
go build ./... && go test ./...
```

- [ ] **Step 4: Commit**

```bash
git add internal/service/ops.go internal/app/handlers_ops.go
git commit -m "refactor: introduce OpsService for dashboard aggregation"
```

---

### Task 14: Wire all services into `App` constructor

**Files:**
- Modify: `internal/app/app.go`

- [ ] **Step 1: Add service fields to `App` struct**

```go
type App struct {
	// ... existing fields ...
	userService *service.UserService
	usageService *service.UsageService
	nodeService *service.NodeService
	opsService  *service.OpsService
}
```

- [ ] **Step 2: Initialize in `New()`**

```go
func New(cfg config.Config, store *repo.Store) *App {
	a := &App{cfg: cfg, store: store, ...}
	a.userService = service.NewUserService(store, a)
	a.usageService = service.NewUsageService(store, cfg.OnlineWindowSec, cfg.IPLimitStrikes, a)
	a.nodeService = service.NewNodeService(store, cfg.NodeStaleDeleteAfterSec, a)
	a.opsService = service.NewOpsService(store)
	return a
}
```

Note: `App` itself satisfies `SyncRequester` via its existing `requestUsersSync` / `requestUsersSyncNow` methods (export them or use the interface).

- [ ] **Step 3: Verify build and tests**

```bash
go build ./... && go test ./...
```

- [ ] **Step 4: Commit**

```bash
git add internal/app/app.go
git commit -m "refactor: wire service layer into App constructor"
```

---

## Phase 2b: Move `StartWorkers` loop to use services (already done incrementally in Tasks 10–12)

After Task 14, the `StartWorkers` loop should read:

```go
func (a *App) StartWorkers(ctx context.Context) {
	go a.hostMonitor.Start(ctx, 5*time.Second, a.store.GetTrafficTotals)
	go a.startHostNetMonthlyRecorder(ctx)
	go a.telegram.Start(ctx)
	go a.startNodeReconciler(ctx)
	go a.startNodeJobTimeoutSweeper(ctx)

	// ...ticker loop...
	if err := a.userService.RefreshLifecycleState(ctx); err != nil { ... }
	if err := a.usageService.EnforceIPLimits(ctx); err != nil { ... }
	if err := a.nodeService.CleanupStaleNodes(ctx); err != nil { ... }
	// prune + alert dispatch remain in app for now
}
```

---

## Execution order summary

| # | Task | Risk | LOC moved |
|---|------|------|-----------|
| 1 | helpers.go | Low | ~40 |
| 2 | users.go | Medium | ~400 |
| 3 | subscriptions.go | Medium | ~550 |
| 4 | usage.go | **High** | ~800 |
| 5 | online_sessions.go | Low | ~200 |
| 6 | traffic.go | Medium | ~350 |
| 7 | nodes.go | Medium | ~400 |
| 8 | api_keys + backup + ops | Low | ~250 |
| 9 | Verify & cleanup | Low | — |
| 10 | service/users.go | Low | ~30 new |
| 11 | service/nodes.go | Low | ~30 new |
| 12 | service/usage.go | Low | ~20 new |
| 13 | service/ops.go | Low | ~40 new |
| 14 | Wire into App | Medium | ~20 modified |

**Critical path:** Task 4 (usage.go) is highest risk because quota/enforcement logic is dense and has many internal cross-references. Run tests after every sub-step within that task.

---

## Dependency diagram after completion

```
cmd/server
  → internal/app          (HTTP, templates, auth, middleware)
    → internal/service    (UserService, UsageService, NodeService, OpsService)
      → internal/repo     (split into ~15 focused files, same package)
      → internal/notify
    → internal/config

cmd/node-agent
  → internal/agent
    → internal/templates
    → internal/hostnet
    → internal/config
```

No circular dependencies. `service` never imports `app`. `repo` never imports `service`.
