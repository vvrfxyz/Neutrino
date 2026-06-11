package functional_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"neutrino/internal/usersync"
)

// Drives the delta user-sync rollout end-to-end over real mTLS HTTP: a node
// bootstraps with a schema-1 full job, proves its baseline (delta-ready
// marker), then receives a delta job for an incremental user change, and a
// stale-base delta is repaired with a forced full instead of looping.
func TestFunctional_UsersSyncFullThenDeltaLifecycle(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	user1 := createUserDirect(t, env)
	node := createManagedXrayNodeDirect(t, env, fmt.Sprintf("delta-sync-%d", time.Now().UnixNano()))
	clearNodeJobs(t, env, node.ID)

	// Versioned /agent/users: old agents read only "users"; new agents also
	// get schema+version, and the version must hash-match the users.
	resp := env.requestMTLS(t, node.ID, http.MethodGet, fmt.Sprintf("/api/v1/nodes/%d/agent/users", node.ID), nil)
	mustStatus(t, resp, http.StatusOK)
	snapshot := decodeBodyMap(t, resp)
	if schema, _ := snapshot["schema"].(float64); int(schema) != 1 {
		t.Fatalf("expected schema 1, got %v", snapshot["schema"])
	}
	version, _ := snapshot["version"].(string)
	if version == "" {
		t.Fatalf("expected non-empty version, got %v", snapshot)
	}
	rawUsers, _ := json.Marshal(snapshot["users"])
	var items []usersync.Item
	if err := json.Unmarshal(rawUsers, &items); err != nil {
		t.Fatalf("decode users: %v", err)
	}
	if usersync.HashItems(items) != version {
		t.Fatalf("response version does not hash-match its users")
	}
	foundUser1 := false
	for _, it := range items {
		if it.UserID == user1.ID {
			foundUser1 = true
		}
	}
	if !foundUser1 {
		t.Fatalf("user %d missing from snapshot: %+v", user1.ID, items)
	}

	// Bootstrap full job: prepare → claim → schema-1 result → delta-ready.
	res, err := env.store.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !res.Enqueued || res.Mode != "full" {
		t.Fatalf("bootstrap prepare: %+v", res)
	}

	claimJob := func() map[string]any {
		t.Helper()
		resp := env.requestMTLS(t, node.ID, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/jobs/claim?wait=0", node.ID), nil)
		mustStatus(t, resp, http.StatusOK)
		claim := decodeBodyMap(t, resp)
		job, ok := claim["job"].(map[string]any)
		if !ok {
			t.Fatalf("expected job, got %v", claim)
		}
		return job
	}
	finishJob := func(job map[string]any, applied string, result map[string]any) map[string]any {
		t.Helper()
		resp := env.requestMTLS(t, node.ID, http.MethodPost,
			fmt.Sprintf("/api/v1/nodes/%d/jobs/%d/finish", node.ID, int64(job["id"].(float64))),
			map[string]any{
				"status":          "succeeded",
				"retryable":       false,
				"applied_version": applied,
				"attempt":         int(job["attempts"].(float64)),
				"result_json":     result,
			})
		mustStatus(t, resp, http.StatusOK)
		return decodeBodyMap(t, resp)
	}

	job := claimJob()
	if job["kind"].(string) != "users_sync" {
		t.Fatalf("expected users_sync, got %v", job)
	}
	var payload usersync.JobPayload
	if err := json.Unmarshal([]byte(job["payload_json"].(string)), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Schema != 1 || payload.Mode != "full" || payload.TargetVersion != res.TargetVersion {
		t.Fatalf("bootstrap payload: %+v", payload)
	}
	finishJob(job, res.TargetVersion, map[string]any{
		"mode": "full", "target_version": res.TargetVersion, "synced": 1, "removed": 0, "failed": 0,
	})

	rollout, err := env.store.GetNodeUsersSyncRollout(ctx, node.ID)
	if err != nil || rollout.BaselineSchema != 1 {
		t.Fatalf("delta-ready marker not set: %+v err=%v", rollout, err)
	}

	// Incremental change → delta job over HTTP.
	user2 := createUserDirect(t, env)
	res2, err := env.store.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	if err != nil {
		t.Fatalf("prepare delta: %v", err)
	}
	if res2.Mode != "delta" {
		t.Fatalf("expected delta, got %+v", res2)
	}
	job = claimJob()
	if err := json.Unmarshal([]byte(job["payload_json"].(string)), &payload); err != nil {
		t.Fatalf("decode delta payload: %v", err)
	}
	if payload.Mode != "delta" || payload.BaseVersion != res.TargetVersion || len(payload.Changes) != 1 {
		t.Fatalf("delta payload: %+v", payload)
	}
	if payload.Changes[0].User.UserID != user2.ID || payload.Changes[0].Action != "upsert" {
		t.Fatalf("delta change: %+v", payload.Changes[0])
	}
	finishJob(job, res2.TargetVersion, map[string]any{
		"mode": "delta", "base_version": payload.BaseVersion, "target_version": res2.TargetVersion,
		"synced": 1, "removed": 0, "failed": 0,
	})
	nodeRow, err := env.store.GetNode(ctx, node.ID)
	if err != nil || nodeRow.AppliedUsersVersion != res2.TargetVersion {
		t.Fatalf("applied=%s want %s err=%v", nodeRow.AppliedUsersVersion, res2.TargetVersion, err)
	}

	// Agent-side base mismatch: a need_full_sync failure over HTTP must
	// enqueue a forced full repair, not retry the delta.
	user3 := createUserDirect(t, env)
	_ = user3
	res3, err := env.store.PrepareUsersSync(ctx, node.ID, false, "reconcile")
	if err != nil || res3.Mode != "delta" {
		t.Fatalf("prepare second delta: %+v err=%v", res3, err)
	}
	job = claimJob()
	resp = env.requestMTLS(t, node.ID, http.MethodPost,
		fmt.Sprintf("/api/v1/nodes/%d/jobs/%d/finish", node.ID, int64(job["id"].(float64))),
		map[string]any{
			"status":    "failed",
			"retryable": false,
			"attempt":   int(job["attempts"].(float64)),
			"error":     "base mismatch",
			"result_json": map[string]any{
				"mode": "delta", "need_full_sync": true, "reason": "base_version_mismatch",
			},
		})
	mustStatus(t, resp, http.StatusOK)
	_ = decodeBodyMap(t, resp)

	job = claimJob()
	if err := json.Unmarshal([]byte(job["payload_json"].(string)), &payload); err != nil {
		t.Fatalf("decode repair payload: %v", err)
	}
	if payload.Mode != "full" || payload.Reason != "base_version_mismatch" {
		t.Fatalf("expected forced full repair, got %+v", payload)
	}
	finishJob(job, res3.TargetVersion, map[string]any{
		"mode": "full", "target_version": res3.TargetVersion, "synced": 3, "removed": 0, "failed": 0,
	})
	nodeRow, _ = env.store.GetNode(ctx, node.ID)
	if nodeRow.AppliedUsersVersion != res3.TargetVersion {
		t.Fatalf("repair did not converge: applied=%s want %s", nodeRow.AppliedUsersVersion, res3.TargetVersion)
	}
}
