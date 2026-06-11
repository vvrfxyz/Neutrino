package usersync

import (
	"encoding/json"
	"strings"
	"testing"
)

// Exact fixtures pin the legacy hash contract shared with the pre-refactor
// service.UsersDesiredVersion and agent hashUsers implementations
// (field order user_id,email,status,uuid; sorted by user_id; omitempty uuid).
const (
	emptyListHash = "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945" // sha256("[]")
	mixedListHash = "50763c1ad563ad10f3bc1cfbb33ad5bebf7b067620b041ea3fa277dfe0936b1b"
)

func mixedItems() []Item {
	return []Item{
		{UserID: 1, Email: "alice@example.com", Status: "active", UUID: "11111111-1111-1111-1111-111111111111"},
		{UserID: 2, Email: "bob@example.com", Status: "disabled"},
		{UserID: 3, Email: "carol@example.com", Status: "over_limit", UUID: "33333333-3333-3333-3333-333333333333"},
	}
}

func TestHashItemsLegacyFixtures(t *testing.T) {
	if got := HashItems(nil); got != emptyListHash {
		t.Fatalf("HashItems(nil) = %s, want %s", got, emptyListHash)
	}
	if got := HashItems([]Item{}); got != emptyListHash {
		t.Fatalf("HashItems(empty) = %s, want %s", got, emptyListHash)
	}
	if got := HashItems(mixedItems()); got != mixedListHash {
		t.Fatalf("HashItems(mixed) = %s, want %s", got, mixedListHash)
	}
	// Order-independent: hashing sorts by user_id without mutating input.
	shuffled := []Item{mixedItems()[2], mixedItems()[0], mixedItems()[1]}
	if got := HashItems(shuffled); got != mixedListHash {
		t.Fatalf("HashItems(shuffled) = %s, want %s", got, mixedListHash)
	}
	if shuffled[0].UserID != 3 {
		t.Fatalf("HashItems mutated its input")
	}
}

func TestHashItemsFieldOrderContract(t *testing.T) {
	b, err := json.Marshal(Item{UserID: 1, Email: "a@b", Status: "active", UUID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"user_id":1,"email":"a@b","status":"active","uuid":"u"}`
	if string(b) != want {
		t.Fatalf("Item JSON = %s, want %s (field order is part of the hash contract)", b, want)
	}
	b, _ = json.Marshal(Item{UserID: 2, Email: "c@d", Status: "disabled"})
	if strings.Contains(string(b), "uuid") {
		t.Fatalf("empty uuid must be omitted, got %s", b)
	}
}

func TestValidateItems(t *testing.T) {
	if err := ValidateItems(nil); err != nil {
		t.Fatalf("nil list: %v", err)
	}
	if err := ValidateItems(mixedItems()); err != nil {
		t.Fatalf("mixed list: %v", err)
	}
	cases := []struct {
		name  string
		items []Item
	}{
		{"duplicate user_id", []Item{
			{UserID: 1, Email: "a@b", Status: "active"},
			{UserID: 1, Email: "c@d", Status: "active"},
		}},
		{"duplicate email", []Item{
			{UserID: 1, Email: "a@b", Status: "active"},
			{UserID: 2, Email: "a@b", Status: "active"},
		}},
		{"duplicate email after trim", []Item{
			{UserID: 1, Email: "a@b", Status: "active"},
			{UserID: 2, Email: " a@b ", Status: "active"},
		}},
		{"empty email", []Item{{UserID: 1, Email: "  ", Status: "active"}}},
		{"zero user_id", []Item{{UserID: 0, Email: "a@b", Status: "active"}}},
		{"negative user_id", []Item{{UserID: -1, Email: "a@b", Status: "active"}}},
		{"unknown status", []Item{{UserID: 1, Email: "a@b", Status: "banned"}}},
	}
	for _, tc := range cases {
		if err := ValidateItems(tc.items); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
	// Validation must not mutate items (trim-only inspection).
	items := []Item{{UserID: 1, Email: " a@b ", Status: "active"}}
	_ = ValidateItems(items)
	if items[0].Email != " a@b " {
		t.Fatalf("ValidateItems mutated email to %q", items[0].Email)
	}
}

func TestValidateItemsAllowsActiveWithoutUUID(t *testing.T) {
	// uuid-less active users are rejected at the Xray layer, not here.
	if err := ValidateItems([]Item{{UserID: 1, Email: "a@b", Status: "active"}}); err != nil {
		t.Fatalf("active without uuid must pass canonical validation: %v", err)
	}
}

func TestDiffItemsBasic(t *testing.T) {
	base := []Item{
		{UserID: 1, Email: "a@b", Status: "active", UUID: "u1"},
		{UserID: 2, Email: "c@d", Status: "active", UUID: "u2"},
		{UserID: 3, Email: "e@f", Status: "active", UUID: "u3"},
		{UserID: 5, Email: "i@j", Status: "active", UUID: "u5"},
		{UserID: 6, Email: "k@l", Status: "active", UUID: "u6"},
		{UserID: 7, Email: "m@n", Status: "active", UUID: "u7"},
		{UserID: 8, Email: "o@p", Status: "active", UUID: "u8"},
	}
	target := []Item{
		{UserID: 1, Email: "a@b", Status: "disabled", UUID: "u1"}, // status change
		{UserID: 3, Email: "e@f", Status: "active", UUID: "u3x"},  // uuid change
		{UserID: 4, Email: "g@h", Status: "active", UUID: "u4"},   // add
		{UserID: 5, Email: "i@j", Status: "active", UUID: "u5"},
		{UserID: 6, Email: "k@l", Status: "active", UUID: "u6"},
		{UserID: 7, Email: "m@n", Status: "active", UUID: "u7"},
		{UserID: 8, Email: "o@p", Status: "active", UUID: "u8"},
		// user 2 removed
	}
	changes, safe := DiffItems(base, target)
	if !safe {
		t.Fatalf("expected safe diff")
	}
	if len(changes) != 4 {
		t.Fatalf("expected 4 changes, got %d: %+v", len(changes), changes)
	}
	// Deterministic user_id order.
	for i := 1; i < len(changes); i++ {
		if changes[i-1].User.UserID > changes[i].User.UserID {
			t.Fatalf("changes not sorted by user_id: %+v", changes)
		}
	}
	byID := map[int64]Change{}
	for _, ch := range changes {
		byID[ch.User.UserID] = ch
	}
	if byID[1].Action != ActionUpsert || byID[1].User.Status != "disabled" {
		t.Fatalf("user 1: %+v", byID[1])
	}
	if byID[2].Action != ActionRemove || byID[2].User.Email != "c@d" {
		t.Fatalf("user 2 remove must carry base item: %+v", byID[2])
	}
	if byID[3].Action != ActionUpsert || byID[3].User.UUID != "u3x" {
		t.Fatalf("user 3: %+v", byID[3])
	}
	if byID[4].Action != ActionUpsert {
		t.Fatalf("user 4: %+v", byID[4])
	}
	// Round-trip: applying the diff to base yields the target hash.
	next, err := ApplyChanges(base, changes)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if HashItems(next) != HashItems(target) {
		t.Fatalf("apply(diff) does not reproduce target")
	}
}

func TestDiffItemsNoChanges(t *testing.T) {
	base := mixedItems()
	changes, safe := DiffItems(base, mixedItems())
	if !safe || len(changes) != 0 {
		t.Fatalf("expected empty safe diff, got safe=%v changes=%+v", safe, changes)
	}
}

func TestDiffItemsEmailChangeForcesFull(t *testing.T) {
	base := []Item{{UserID: 1, Email: "a@b", Status: "active", UUID: "u1"}}
	target := []Item{{UserID: 1, Email: "renamed@b", Status: "active", UUID: "u1"}}
	if _, safe := DiffItems(base, target); safe {
		t.Fatalf("same-ID email change must be unsafe")
	}
}

func TestDiffItemsCrossIDEmailMoveForcesFull(t *testing.T) {
	base := []Item{
		{UserID: 1, Email: "a@b", Status: "active", UUID: "u1"},
	}
	target := []Item{
		{UserID: 2, Email: "a@b", Status: "active", UUID: "u2"}, // email moved 1 -> 2
	}
	if _, safe := DiffItems(base, target); safe {
		t.Fatalf("cross-ID email ownership move must be unsafe")
	}
}

func TestDiffItemsTooManyChangesForcesFull(t *testing.T) {
	base := []Item{}
	target := make([]Item, 0, MaxDeltaChanges+1)
	for i := 0; i < MaxDeltaChanges+1; i++ {
		target = append(target, Item{UserID: int64(i + 1), Email: "u" + string(rune('a'+i%26)) + itoa(i) + "@x", Status: "active", UUID: "u"})
	}
	if _, safe := DiffItems(base, target); safe {
		t.Fatalf("delta above MaxDeltaChanges must be unsafe")
	}
}

func TestDiffItemsDeltaNotSmallerThanFullForcesFull(t *testing.T) {
	// Every user changes: the encoded delta cannot be smaller than the full
	// snapshot (each change wraps the full item), so full must win.
	base := []Item{
		{UserID: 1, Email: "a@b", Status: "active", UUID: "u1"},
		{UserID: 2, Email: "c@d", Status: "active", UUID: "u2"},
	}
	target := []Item{
		{UserID: 1, Email: "a@b", Status: "disabled", UUID: "u1"},
		{UserID: 2, Email: "c@d", Status: "disabled", UUID: "u2"},
	}
	if _, safe := DiffItems(base, target); safe {
		t.Fatalf("delta >= full snapshot size must be unsafe")
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func TestApplyChangesDoesNotMutateInputs(t *testing.T) {
	base := mixedItems()
	changes := []Change{
		{Action: ActionRemove, User: base[1]},
		{Action: ActionUpsert, User: Item{UserID: 9, Email: "new@x", Status: "active", UUID: "u9"}},
	}
	baseCopy := mixedItems()
	next, err := ApplyChanges(base, changes)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	for i := range base {
		if base[i] != baseCopy[i] {
			t.Fatalf("base mutated at %d", i)
		}
	}
	if len(next) != 3 {
		t.Fatalf("expected 3 items, got %+v", next)
	}
	if next[0].UserID != 1 || next[1].UserID != 3 || next[2].UserID != 9 {
		t.Fatalf("unexpected next baseline: %+v", next)
	}
}

func TestApplyChangesRejectsMalformedDeltas(t *testing.T) {
	base := []Item{{UserID: 1, Email: "a@b", Status: "active", UUID: "u1"}}
	cases := []struct {
		name    string
		changes []Change
	}{
		{"duplicate change for user_id", []Change{
			{Action: ActionUpsert, User: Item{UserID: 1, Email: "a@b", Status: "disabled", UUID: "u1"}},
			{Action: ActionRemove, User: Item{UserID: 1, Email: "a@b", Status: "disabled", UUID: "u1"}},
		}},
		{"remove missing base user", []Change{
			{Action: ActionRemove, User: Item{UserID: 5, Email: "x@y", Status: "active"}},
		}},
		{"remove base mismatch email", []Change{
			{Action: ActionRemove, User: Item{UserID: 1, Email: "wrong@b", Status: "active", UUID: "u1"}},
		}},
		{"remove base mismatch uuid", []Change{
			{Action: ActionRemove, User: Item{UserID: 1, Email: "a@b", Status: "active", UUID: "other"}},
		}},
		{"upsert empty email", []Change{
			{Action: ActionUpsert, User: Item{UserID: 2, Email: " ", Status: "active"}},
		}},
		{"upsert unknown status", []Change{
			{Action: ActionUpsert, User: Item{UserID: 2, Email: "b@c", Status: "banned"}},
		}},
		{"upsert invalid user_id", []Change{
			{Action: ActionUpsert, User: Item{UserID: 0, Email: "b@c", Status: "active"}},
		}},
		{"unknown action", []Change{
			{Action: "replace", User: Item{UserID: 1, Email: "a@b", Status: "active", UUID: "u1"}},
		}},
	}
	for _, tc := range cases {
		if _, err := ApplyChanges(base, tc.changes); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}
