package agent

import (
	"context"
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

func (a *Agent) execUsersSync(ctx context.Context) (appliedVersion string, synced int, removed int, err error) {
	pc := a.panelClient()
	if pc == nil {
		return "", 0, 0, JobError{Retryable: true, Msg: "panel client not ready", ResultJSON: map[string]any{"synced": 0, "removed": 0, "failed": 0}}
	}
	prevUsers := a.state.Snapshot().SyncedUsers
	users, err := pc.FetchUsers(ctx)
	if err != nil {
		return "", 0, 0, JobError{Retryable: true, Msg: err.Error(), ResultJSON: map[string]any{"synced": 0, "removed": 0, "failed": 0}}
	}
	a.xrayUsersMu.Lock()
	defer a.xrayUsersMu.Unlock()
	synced, removed, err = applyUsersSync(ctx, a.xray, prevUsers, users)
	if err != nil {
		return "", synced, removed, err
	}
	_ = a.state.Update(func(s *State) { s.SyncedUsers = users })

	appliedVersion = hashUsers(users)
	return appliedVersion, synced, removed, nil
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
