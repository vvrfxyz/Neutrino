package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"neutrino/internal/usersync"
)

type UserSyncItem struct {
	UserID int64  `json:"user_id"`
	Email  string `json:"email"`
	UUID   string `json:"uuid"`
	Status string `json:"status"`
}

type userSyncApplier interface {
	UpsertUser(ctx context.Context, email, uuid string) error
	RemoveUser(ctx context.Context, email string) error
}

func (a *Agent) restoreSyncedUsersOnce(ctx context.Context) {
	restoreCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	synced, err := a.restoreSyncedUsers(restoreCtx)
	if err != nil {
		log.Printf("restore synced xray users failed reason=startup synced=%d err=%v", synced, err)
		return
	}
	if synced > 0 {
		log.Printf("restored synced xray users reason=startup synced=%d", synced)
	}
}

func (a *Agent) restoreSyncedUsers(ctx context.Context) (synced int, err error) {
	if a == nil || a.state == nil || a.xray == nil {
		return 0, nil
	}
	a.xrayUsersMu.Lock()
	defer a.xrayUsersMu.Unlock()

	users := a.state.Snapshot().SyncedUsers
	if len(users) == 0 {
		return 0, nil
	}
	return restoreActiveUsers(ctx, a.xray, users)
}

// effectiveSyncedUsers returns the committed baseline plus, while a pending
// users-sync journal exists, its target users and possible runtime users.
// Any path mapping runtime Xray users back to panel users (online snapshot,
// stats, access attribution, IP-limit mapping, stale removal) must use this:
// a failed Xray mutation may have hot-added users before the state commit.
func effectiveSyncedUsers(s State) []UserSyncItem {
	if s.PendingUsersSync == nil {
		return s.SyncedUsers
	}
	out := make([]UserSyncItem, 0, len(s.SyncedUsers)+len(s.PendingUsersSync.TargetUsers)+len(s.PendingUsersSync.PossibleRuntimeUsers))
	seen := make(map[string]struct{}, cap(out))
	add := func(users []UserSyncItem) {
		for _, u := range users {
			email := strings.TrimSpace(u.Email)
			if email == "" {
				continue
			}
			if _, ok := seen[email]; ok {
				continue
			}
			seen[email] = struct{}{}
			out = append(out, u)
		}
	}
	add(s.SyncedUsers)
	add(s.PendingUsersSync.TargetUsers)
	add(s.PendingUsersSync.PossibleRuntimeUsers)
	return out
}

// usersSyncOutcome is what execUsersSync hands back to the job runner: the
// structured result JSON for the panel plus the applied version (empty when
// the job did not commit).
type usersSyncOutcome struct {
	AppliedVersion string
	Result         map[string]any
}

func usersSyncFailure(retryable bool, msg string, result map[string]any) error {
	if result == nil {
		result = map[string]any{}
	}
	if _, ok := result["synced"]; !ok {
		result["synced"] = 0
	}
	if _, ok := result["removed"]; !ok {
		result["removed"] = 0
	}
	if _, ok := result["failed"]; !ok {
		result["failed"] = 0
	}
	return JobError{Retryable: retryable, Msg: msg, ResultJSON: result}
}

func needFullSyncFailure(mode, reason string) error {
	return usersSyncFailure(false, "users sync needs full repair: "+reason, map[string]any{
		"mode":           mode,
		"need_full_sync": true,
		"reason":         reason,
	})
}

// execUsersSync executes a users_sync job payload: legacy ("" / "{}") and
// schema-1 full payloads fetch the panel's full snapshot; schema-1 delta
// payloads apply changes against the persisted local baseline. Unknown
// schema/mode payloads are permanent failures.
func (a *Agent) execUsersSync(ctx context.Context, payloadJSON string) (usersSyncOutcome, error) {
	payload, class, perr := parseUsersSyncJobPayload(payloadJSON)
	if perr != nil {
		return usersSyncOutcome{}, usersSyncFailure(false, perr.Error(), map[string]any{
			"need_full_sync": true,
			"reason":         "invalid_delta_payload",
		})
	}
	switch class {
	case usersSyncJobDelta:
		return a.execUsersSyncDelta(ctx, payload)
	default:
		return a.execUsersSyncFull(ctx)
	}
}

type usersSyncJobClass int

const (
	usersSyncJobLegacyFull usersSyncJobClass = iota
	usersSyncJobFull
	usersSyncJobDelta
)

func parseUsersSyncJobPayload(raw string) (usersync.JobPayload, usersSyncJobClass, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return usersync.JobPayload{}, usersSyncJobLegacyFull, nil
	}
	var p usersync.JobPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return usersync.JobPayload{}, usersSyncJobLegacyFull, fmt.Errorf("users_sync payload unparsable: %v", err)
	}
	if p.Schema != usersync.SchemaV1 {
		return p, usersSyncJobLegacyFull, fmt.Errorf("users_sync payload has unknown schema %d", p.Schema)
	}
	switch p.Mode {
	case usersync.ModeFull:
		if strings.TrimSpace(p.TargetVersion) == "" {
			return p, usersSyncJobFull, fmt.Errorf("schema-1 full payload missing target_version")
		}
		return p, usersSyncJobFull, nil
	case usersync.ModeDelta:
		if strings.TrimSpace(p.BaseVersion) == "" || strings.TrimSpace(p.TargetVersion) == "" || len(p.Changes) == 0 {
			return p, usersSyncJobDelta, fmt.Errorf("schema-1 delta payload missing base/target/changes")
		}
		for _, ch := range p.Changes {
			if ch.Action != usersync.ActionUpsert && ch.Action != usersync.ActionRemove {
				return p, usersSyncJobDelta, fmt.Errorf("schema-1 delta payload has unknown action %q", ch.Action)
			}
			if ch.User.UserID <= 0 || strings.TrimSpace(ch.User.Email) == "" || strings.TrimSpace(ch.User.Status) == "" {
				return p, usersSyncJobDelta, fmt.Errorf("schema-1 delta change for user_id %d invalid", ch.User.UserID)
			}
		}
		return p, usersSyncJobDelta, nil
	default:
		return p, usersSyncJobLegacyFull, fmt.Errorf("users_sync payload has unknown mode %q", p.Mode)
	}
}

// journalPendingUsersSync persists the pre-operation journal, merging any
// previous journal's target and possible-runtime users so users hot-added by
// earlier failed attempts stay tracked for stale removal.
func (a *Agent) journalPendingUsersSync(mode, baseVersion, targetVersion string, targetUsers []UserSyncItem, prev *PendingUsersSyncState) error {
	possible := make([]UserSyncItem, 0)
	if prev != nil {
		possible = append(possible, prev.TargetUsers...)
		possible = append(possible, prev.PossibleRuntimeUsers...)
	}
	return a.state.Update(func(s *State) {
		s.PendingUsersSync = &PendingUsersSyncState{
			Mode:                 mode,
			BaseVersion:          baseVersion,
			TargetVersion:        targetVersion,
			TargetUsers:          targetUsers,
			PossibleRuntimeUsers: dedupeUsersByEmail(possible),
			CreatedAt:            time.Now().UTC().Format(time.RFC3339),
		}
	})
}

func dedupeUsersByEmail(users []UserSyncItem) []UserSyncItem {
	out := make([]UserSyncItem, 0, len(users))
	seen := make(map[string]struct{}, len(users))
	for _, u := range users {
		email := strings.TrimSpace(u.Email)
		if email == "" {
			continue
		}
		if _, ok := seen[email]; ok {
			continue
		}
		seen[email] = struct{}{}
		out = append(out, u)
	}
	return out
}

func (a *Agent) execUsersSyncFull(ctx context.Context) (usersSyncOutcome, error) {
	pc := a.panelClient()
	if pc == nil {
		return usersSyncOutcome{}, usersSyncFailure(true, "panel client not ready", map[string]any{"mode": "full"})
	}
	snap := a.state.Snapshot()
	users, responseVersion, err := pc.FetchUsers(ctx)
	if err != nil {
		return usersSyncOutcome{}, usersSyncFailure(true, err.Error(), map[string]any{"mode": "full"})
	}
	if err := usersync.ValidateItems(syncItems(users)); err != nil {
		// The panel returned a canonical list we cannot trust; retry rather
		// than apply ambiguous email-keyed operations.
		return usersSyncOutcome{}, usersSyncFailure(true, "fetched users invalid: "+err.Error(), map[string]any{"mode": "full"})
	}
	computed := hashUsers(users)
	if responseVersion != "" && responseVersion != computed {
		// A versioned full response must hash-match its own users; an
		// internally inconsistent response must never touch Xray or state.
		return usersSyncOutcome{}, usersSyncFailure(true, "full response version does not match its users", map[string]any{
			"mode":   "full",
			"reason": "full_response_hash_mismatch",
		})
	}
	appliedVersion := responseVersion
	if appliedVersion == "" {
		// Old panel: persist the locally computed hash so later versioned
		// delta jobs have a concrete baseline.
		appliedVersion = computed
	}

	a.xrayUsersMu.Lock()
	defer a.xrayUsersMu.Unlock()

	prevPending := snap.PendingUsersSync
	if err := a.journalPendingUsersSync(usersync.ModeFull, "", appliedVersion, users, prevPending); err != nil {
		return usersSyncOutcome{}, usersSyncFailure(true, "journal users sync: "+err.Error(), map[string]any{"mode": "full"})
	}

	// Remove stale users against everything that may exist in the runtime:
	// the committed baseline plus any earlier failed attempt's journal.
	prevUsers := snap.SyncedUsers
	if prevPending != nil {
		prevUsers = append(append([]UserSyncItem(nil), prevUsers...), prevPending.TargetUsers...)
		prevUsers = append(prevUsers, prevPending.PossibleRuntimeUsers...)
		prevUsers = dedupeUsersByEmail(prevUsers)
	}
	synced, removed, err := applyUsersSync(ctx, a.xray, prevUsers, users)
	if err != nil {
		// Journal stays: a later attempt must keep treating these users as
		// possibly present in the runtime.
		return usersSyncOutcome{}, err
	}
	if err := a.state.Update(func(s *State) {
		s.SyncedUsers = users
		s.SyncedUsersVersion = appliedVersion
		s.PendingUsersSync = nil
	}); err != nil {
		// Xray applied but the baseline did not persist: the job is not
		// successful. The retained journal covers the next attempt.
		return usersSyncOutcome{}, usersSyncFailure(true, "persist users sync state: "+err.Error(), map[string]any{
			"mode":    "full",
			"synced":  synced,
			"removed": removed,
		})
	}
	return usersSyncOutcome{
		AppliedVersion: appliedVersion,
		Result: map[string]any{
			"mode": "full",
			// Always the actually fetched/applied version: a stale full job's
			// payload target may differ, and finish validation compares
			// against what the agent really applied.
			"target_version": appliedVersion,
			"synced":         synced,
			"removed":        removed,
			"failed":         0,
		},
	}, nil
}

func (a *Agent) execUsersSyncDelta(ctx context.Context, payload usersync.JobPayload) (usersSyncOutcome, error) {
	a.xrayUsersMu.Lock()
	defer a.xrayUsersMu.Unlock()

	snap := a.state.Snapshot()
	if snap.SyncedUsersVersion != payload.BaseVersion {
		return usersSyncOutcome{}, needFullSyncFailure("delta", "base_version_mismatch")
	}
	baseItems := syncItems(snap.SyncedUsers)
	if err := usersync.ValidateItems(baseItems); err != nil {
		return usersSyncOutcome{}, needFullSyncFailure("delta", "invalid_local_baseline")
	}
	if usersync.HashItems(baseItems) != snap.SyncedUsersVersion {
		return usersSyncOutcome{}, needFullSyncFailure("delta", "invalid_local_baseline")
	}
	// Remove changes must match the local base item exactly: a mismatched
	// remove could compute the right target hash while deleting the wrong
	// Xray email.
	baseByID := make(map[int64]UserSyncItem, len(snap.SyncedUsers))
	for _, u := range snap.SyncedUsers {
		baseByID[u.UserID] = u
	}
	for _, ch := range payload.Changes {
		if ch.Action != usersync.ActionRemove {
			continue
		}
		b, ok := baseByID[ch.User.UserID]
		if !ok || b.Email != ch.User.Email || b.Status != ch.User.Status || b.UUID != ch.User.UUID {
			return usersSyncOutcome{}, needFullSyncFailure("delta", "remove_base_mismatch")
		}
	}

	nextItems, err := usersync.ApplyChanges(baseItems, payload.Changes)
	if err != nil {
		return usersSyncOutcome{}, needFullSyncFailure("delta", "invalid_delta_payload")
	}
	if err := usersync.ValidateItems(nextItems); err != nil {
		return usersSyncOutcome{}, needFullSyncFailure("delta", "invalid_delta_payload")
	}
	if usersync.HashItems(nextItems) != payload.TargetVersion {
		return usersSyncOutcome{}, needFullSyncFailure("delta", "local_hash_mismatch")
	}
	next := fromSyncItems(nextItems)

	// An unrelated pending journal means an earlier different sync attempt
	// may have partially mutated Xray; a delta on top of that is unsafe.
	if pj := snap.PendingUsersSync; pj != nil &&
		(pj.Mode != usersync.ModeDelta || pj.BaseVersion != payload.BaseVersion || pj.TargetVersion != payload.TargetVersion) {
		return usersSyncOutcome{}, needFullSyncFailure("delta", "pending_journal_mismatch")
	}
	if err := a.journalPendingUsersSync(usersync.ModeDelta, payload.BaseVersion, payload.TargetVersion, next, snap.PendingUsersSync); err != nil {
		return usersSyncOutcome{}, usersSyncFailure(true, "journal users sync: "+err.Error(), map[string]any{"mode": "delta"})
	}

	synced, removed := 0, 0
	failures := make([]string, 0, 2)
	for _, ch := range payload.Changes {
		email := strings.TrimSpace(ch.User.Email)
		switch {
		case ch.Action == usersync.ActionUpsert && ch.User.Status == "active" && strings.TrimSpace(ch.User.UUID) != "":
			if err := a.xray.UpsertUser(ctx, email, ch.User.UUID); err != nil {
				failures = append(failures, fmt.Sprintf("upsert %s: %v", email, err))
				continue
			}
			synced++
		default:
			// Inactive or uuid-less upserts and removes both translate to a
			// runtime removal, mirroring full-sync semantics.
			if err := a.xray.RemoveUser(ctx, email); err != nil {
				failures = append(failures, fmt.Sprintf("remove %s: %v", email, err))
				continue
			}
			removed++
		}
	}
	if len(failures) > 0 {
		// Retryable: idempotent Upsert/Remove plus the retained journal and
		// the unchanged committed baseline make the retry safe.
		return usersSyncOutcome{}, usersSyncFailure(true,
			fmt.Sprintf("delta sync incomplete: %d operation(s) failed (%s)", len(failures), strings.Join(failures, "; ")),
			map[string]any{
				"mode":           "delta",
				"base_version":   payload.BaseVersion,
				"target_version": payload.TargetVersion,
				"synced":         synced,
				"removed":        removed,
				"failed":         len(failures),
				"failures":       failures,
			})
	}
	if err := a.state.Update(func(s *State) {
		s.SyncedUsers = next
		s.SyncedUsersVersion = payload.TargetVersion
		s.PendingUsersSync = nil
	}); err != nil {
		return usersSyncOutcome{}, usersSyncFailure(true, "persist users sync state: "+err.Error(), map[string]any{
			"mode":           "delta",
			"base_version":   payload.BaseVersion,
			"target_version": payload.TargetVersion,
			"synced":         synced,
			"removed":        removed,
		})
	}
	return usersSyncOutcome{
		AppliedVersion: payload.TargetVersion,
		Result: map[string]any{
			"mode":           "delta",
			"base_version":   payload.BaseVersion,
			"target_version": payload.TargetVersion,
			"synced":         synced,
			"removed":        removed,
			"failed":         0,
		},
	}, nil
}

func applyUsersSync(ctx context.Context, xray userSyncApplier, prevUsers []UserSyncItem, currentUsers []UserSyncItem) (synced int, removed int, err error) {
	failures := make([]string, 0, 4)
	recordFailure := func(action string, email string, err error) {
		log.Printf("%s user %s failed: %v", action, email, err)
		failures = append(failures, fmt.Sprintf("%s %s: %v", action, email, err))
	}

	for _, stale := range staleSyncedUsers(prevUsers, currentUsers) {
		email := strings.TrimSpace(stale.Email)
		if email == "" {
			continue
		}
		if err := xray.RemoveUser(ctx, email); err != nil {
			recordFailure("remove stale", email, err)
			continue
		}
		removed++
	}

	for _, u := range currentUsers {
		email := strings.TrimSpace(u.Email)
		if email == "" {
			continue
		}
		if u.Status == "active" && strings.TrimSpace(u.UUID) != "" {
			if err := xray.UpsertUser(ctx, email, u.UUID); err != nil {
				recordFailure("upsert", email, err)
				continue
			}
			synced++
			continue
		}
		if err := xray.RemoveUser(ctx, email); err != nil {
			recordFailure("remove", email, err)
			continue
		}
		removed++
	}

	if len(failures) > 0 {
		details := failures
		if len(details) > 3 {
			details = details[:3]
		}
		msg := fmt.Sprintf("users sync incomplete: %d operation(s) failed", len(failures))
		msg = msg + " (" + strings.Join(details, "; ") + ")"
		if len(failures) > len(details) {
			msg = msg + fmt.Sprintf("; +%d more", len(failures)-len(details))
		}
		return synced, removed, JobError{
			Retryable: true,
			Msg:       msg,
			ResultJSON: map[string]any{
				"synced":   synced,
				"removed":  removed,
				"failed":   len(failures),
				"failures": failures,
			},
		}
	}
	return synced, removed, nil
}

func restoreActiveUsers(ctx context.Context, xray userSyncApplier, users []UserSyncItem) (synced int, err error) {
	failures := make([]string, 0, 4)
	for _, u := range users {
		email := strings.TrimSpace(u.Email)
		uuid := strings.TrimSpace(u.UUID)
		if email == "" || uuid == "" || u.Status != "active" {
			continue
		}
		if err := xray.UpsertUser(ctx, email, uuid); err != nil {
			log.Printf("restore user %s failed: %v", email, err)
			failures = append(failures, fmt.Sprintf("restore %s: %v", email, err))
			continue
		}
		synced++
	}
	if len(failures) > 0 {
		details := failures
		if len(details) > 3 {
			details = details[:3]
		}
		msg := fmt.Sprintf("restore users incomplete: %d operation(s) failed", len(failures))
		msg += " (" + strings.Join(details, "; ") + ")"
		if len(failures) > len(details) {
			msg += fmt.Sprintf("; +%d more", len(failures)-len(details))
		}
		return synced, JobError{Retryable: true, Msg: msg, ResultJSON: map[string]any{"synced": synced, "failed": len(failures), "failures": failures}}
	}
	return synced, nil
}

func staleSyncedUsers(prevUsers []UserSyncItem, currentUsers []UserSyncItem) []UserSyncItem {
	currentByEmail := make(map[string]struct{}, len(currentUsers))
	for _, u := range currentUsers {
		email := strings.TrimSpace(u.Email)
		if email == "" {
			continue
		}
		currentByEmail[email] = struct{}{}
	}
	stale := make([]UserSyncItem, 0, len(prevUsers))
	seen := make(map[string]struct{}, len(prevUsers))
	for _, u := range prevUsers {
		email := strings.TrimSpace(u.Email)
		if email == "" {
			continue
		}
		if _, ok := currentByEmail[email]; ok {
			continue
		}
		if _, ok := seen[email]; ok {
			continue
		}
		seen[email] = struct{}{}
		stale = append(stale, u)
	}
	return stale
}

// syncItems converts agent user structs to the canonical usersync items.
// Conversion must happen before hashing, diffing, or applying changes.
func syncItems(users []UserSyncItem) []usersync.Item {
	items := make([]usersync.Item, 0, len(users))
	for _, u := range users {
		items = append(items, usersync.Item{UserID: u.UserID, Email: u.Email, Status: u.Status, UUID: u.UUID})
	}
	return items
}

func fromSyncItems(items []usersync.Item) []UserSyncItem {
	users := make([]UserSyncItem, 0, len(items))
	for _, it := range items {
		users = append(users, UserSyncItem{UserID: it.UserID, Email: it.Email, Status: it.Status, UUID: it.UUID})
	}
	return users
}

func hashUsers(users []UserSyncItem) string {
	return usersync.HashItems(syncItems(users))
}
