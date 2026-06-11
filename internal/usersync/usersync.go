// Package usersync defines the canonical user-sync item, hash, diff, and
// apply semantics shared by the panel and the node-agent. The hash contract
// (field order user_id,email,status,uuid; sort by user_id; omitempty uuid;
// empty list marshals as []) is compatibility-critical: it must keep producing
// the same versions as the pre-refactor service.UsersDesiredVersion and the
// agent's local hashUsers. This package must stay independent from
// internal/repo (sync preparation reads users inside repo transactions).
package usersync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	ActionUpsert = "upsert"
	ActionRemove = "remove"

	// SchemaV1 is the structured users_sync payload schema. Empty/{}
	// payloads remain the legacy full contract.
	SchemaV1 = 1

	ModeFull  = "full"
	ModeDelta = "delta"

	// MaxDeltaChanges caps how many changes a delta may carry before full
	// sync is considered cheaper/safer. Conservative by design; not
	// configurable until production data demands it.
	MaxDeltaChanges = 200
)

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

// allowedStatuses mirrors the repo user-status state machine.
var allowedStatuses = map[string]bool{
	"active":        true,
	"disabled":      true,
	"expired":       true,
	"over_limit":    true,
	"over_ip_limit": true,
}

// ValidateItems rejects canonical lists that would make hashing or Xray
// email-keyed operations ambiguous. Email validation is trim-only: items are
// never mutated, so the legacy hash contract cannot drift. Active users with
// an empty uuid are allowed here; the Xray application layer owns that rule.
func ValidateItems(items []Item) error {
	seenID := make(map[int64]bool, len(items))
	seenEmail := make(map[string]bool, len(items))
	for i, it := range items {
		if it.UserID <= 0 {
			return fmt.Errorf("usersync: item %d: invalid user_id %d", i, it.UserID)
		}
		if seenID[it.UserID] {
			return fmt.Errorf("usersync: duplicate user_id %d", it.UserID)
		}
		seenID[it.UserID] = true
		email := strings.TrimSpace(it.Email)
		if email == "" {
			return fmt.Errorf("usersync: item %d (user_id %d): empty email", i, it.UserID)
		}
		if seenEmail[email] {
			return fmt.Errorf("usersync: duplicate email %q", email)
		}
		seenEmail[email] = true
		if !allowedStatuses[it.Status] {
			return fmt.Errorf("usersync: item %d (user_id %d): unknown status %q", i, it.UserID, it.Status)
		}
	}
	return nil
}

// HashItems returns the canonical SHA-256 content version of a user list.
// nil and empty input are the same canonical empty list (JSON []).
func HashItems(items []Item) string {
	sorted := make([]Item, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].UserID < sorted[j].UserID })
	b, _ := json.Marshal(sorted)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DiffItems computes the delta from base to target. safe=false means the
// caller must enqueue a full sync instead: same-ID email changes and cross-ID
// email ownership moves are unsafe (Xray operations are keyed by email), and
// oversized deltas are not worth shipping. Inputs are assumed validated.
// Changes are returned in ascending user_id order; remove changes carry the
// base item so the applier knows exactly which Xray email it deletes.
func DiffItems(base, target []Item) (changes []Change, safe bool) {
	baseByID := make(map[int64]Item, len(base))
	baseEmailToID := make(map[string]int64, len(base))
	for _, it := range base {
		baseByID[it.UserID] = it
		baseEmailToID[it.Email] = it.UserID
	}
	targetByID := make(map[int64]Item, len(target))
	for _, it := range target {
		if b, ok := baseByID[it.UserID]; ok && b.Email != it.Email {
			return nil, false
		}
		if owner, ok := baseEmailToID[it.Email]; ok && owner != it.UserID {
			return nil, false
		}
		targetByID[it.UserID] = it
	}

	changes = make([]Change, 0)
	for _, it := range base {
		if _, ok := targetByID[it.UserID]; !ok {
			changes = append(changes, Change{Action: ActionRemove, User: it})
		}
	}
	for _, it := range target {
		b, ok := baseByID[it.UserID]
		if !ok || b.Status != it.Status || b.UUID != it.UUID {
			changes = append(changes, Change{Action: ActionUpsert, User: it})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].User.UserID < changes[j].User.UserID })

	if len(changes) > MaxDeltaChanges {
		return nil, false
	}
	if len(changes) > 0 {
		deltaJSON, _ := json.Marshal(changes)
		fullJSON, _ := json.Marshal(target)
		if len(deltaJSON) >= len(fullJSON) {
			return nil, false
		}
	}
	return changes, true
}

// ApplyChanges materializes the next baseline from base + changes without
// mutating either input. Malformed deltas (duplicate change for one user_id,
// remove for a missing/mismatched base item, invalid upsert, unknown action)
// are errors: the agent must request full sync rather than guess.
func ApplyChanges(base []Item, changes []Change) ([]Item, error) {
	next := make(map[int64]Item, len(base)+len(changes))
	for _, it := range base {
		next[it.UserID] = it
	}
	seen := make(map[int64]bool, len(changes))
	for i, ch := range changes {
		id := ch.User.UserID
		if id <= 0 {
			return nil, fmt.Errorf("usersync: change %d: invalid user_id %d", i, id)
		}
		if seen[id] {
			return nil, fmt.Errorf("usersync: duplicate change for user_id %d", id)
		}
		seen[id] = true
		switch ch.Action {
		case ActionUpsert:
			if strings.TrimSpace(ch.User.Email) == "" {
				return nil, fmt.Errorf("usersync: upsert for user_id %d: empty email", id)
			}
			if !allowedStatuses[ch.User.Status] {
				return nil, fmt.Errorf("usersync: upsert for user_id %d: unknown status %q", id, ch.User.Status)
			}
			next[id] = ch.User
		case ActionRemove:
			b, ok := next[id]
			if !ok {
				return nil, fmt.Errorf("usersync: remove for user_id %d: not in base", id)
			}
			// A mismatched remove could compute the right target hash
			// while deleting the wrong Xray email — refuse it.
			if b.Email != ch.User.Email || b.Status != ch.User.Status || b.UUID != ch.User.UUID {
				return nil, fmt.Errorf("usersync: remove for user_id %d: change does not match base item", id)
			}
			delete(next, id)
		default:
			return nil, fmt.Errorf("usersync: change %d: unknown action %q", i, ch.Action)
		}
	}
	out := make([]Item, 0, len(next))
	for _, it := range next {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out, nil
}
