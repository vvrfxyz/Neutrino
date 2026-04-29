package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"sort"
	"strings"
	"time"

	"neutrino/internal/repo"
)

func usersDesiredVersion(users []repo.User) string {
	// Stable, minimal payload: user_id/email/status/uuid (only what node needs).
	type item struct {
		UserID int64  `json:"user_id"`
		Email  string `json:"email"`
		Status string `json:"status"`
		UUID   string `json:"uuid,omitempty"`
	}
	items := make([]item, 0, len(users))
	for _, u := range users {
		it := item{UserID: u.ID, Email: u.Username, Status: u.Status}
		if u.ActiveLink != nil {
			it.UUID = u.ActiveLink.UUID
		}
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UserID < items[j].UserID })
	b, _ := json.Marshal(items)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (a *App) enqueueUsersSyncForEnabledNodes(ctx context.Context) {
	nodes, err := a.store.ListEnabledNodes(ctx)
	if err != nil {
		log.Printf("list nodes for users_sync: %v", err)
		return
	}
	for _, n := range nodes {
		users, err := a.store.ListUsersForNode(ctx, n.ID)
		if err != nil {
			log.Printf("list users for node=%d: %v", n.ID, err)
			continue
		}
		desiredVersion := usersDesiredVersion(users)
		if err := a.store.SetNodeDesiredUsersVersion(ctx, n.ID, desiredVersion); err != nil {
			log.Printf("set desired users version node=%d err=%v", n.ID, err)
			continue
		}
		if strings.TrimSpace(n.AppliedUsersVersion) == strings.TrimSpace(desiredVersion) {
			continue
		}
		jobID, enqueued, err := a.store.EnqueueNodeJob(ctx, n.ID, "users_sync", desiredVersion, "{}", 20, "users_sync")
		if err != nil {
			log.Printf("enqueue users_sync node=%d desired=%s err=%v", n.ID, desiredVersion, err)
			continue
		}
		log.Printf("users_sync reconcile node=%d desired=%s job_id=%d enqueued=%t", n.ID, desiredVersion, jobID, enqueued)
	}
}

func (a *App) startNodeReconciler(ctx context.Context) {
	sec := a.cfg.NodeReconcileEverySec
	if sec <= 0 {
		sec = 30
	}
	ticker := time.NewTicker(time.Duration(sec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.enqueueUsersSyncForEnabledNodes(ctx)

			// Reconcile managed xray desired state.
			nodes, err := a.store.ListEnabledNodes(ctx)
			if err != nil {
				continue
			}
			for _, n := range nodes {
				if !n.Managed {
					log.Printf("skip xray reconcile node=%d reason=unmanaged", n.ID)
					continue
				}
				ds, ok, err := a.store.GetNodeDesiredState(ctx, n.ID)
				if err != nil {
					log.Printf("get desired xray state node=%d err=%v", n.ID, err)
					continue
				}
				if !ok {
					log.Printf("skip xray reconcile node=%d reason=no_desired_state", n.ID)
					continue
				}
				if err := a.store.SetNodeDesiredXrayVersion(ctx, n.ID, ds.DesiredVersion); err != nil {
					log.Printf("set desired xray version node=%d desired=%s err=%v", n.ID, ds.DesiredVersion, err)
					continue
				}
				if strings.TrimSpace(n.AppliedXrayVersion) == strings.TrimSpace(ds.DesiredVersion) {
					log.Printf("skip xray reconcile node=%d reason=already_applied desired=%s", n.ID, ds.DesiredVersion)
					continue
				}
				has, err := a.store.HasPendingOrRunningNodeJob(ctx, n.ID, "xray_apply")
				if err != nil {
					log.Printf("check pending xray job node=%d err=%v", n.ID, err)
					continue
				}
				if has {
					log.Printf("skip xray reconcile node=%d reason=job_inflight desired=%s", n.ID, ds.DesiredVersion)
					continue
				}
				jobID, enqueued, err := a.store.EnqueueNodeJob(ctx, n.ID, "xray_apply", ds.DesiredVersion, ds.PayloadJSON, 120, "xray_apply")
				if err != nil {
					log.Printf("enqueue xray_apply node=%d desired=%s err=%v", n.ID, ds.DesiredVersion, err)
					continue
				}
				log.Printf("xray reconcile node=%d desired=%s job_id=%d enqueued=%t", n.ID, ds.DesiredVersion, jobID, enqueued)
			}
		}
	}
}

func (a *App) startNodeJobTimeoutSweeper(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	timeoutByKind := map[string]time.Duration{
		"users_sync":    20 * time.Second,
		"xray_apply":    120 * time.Second,
		"xray_rollback": 60 * time.Second,
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			affected, err := a.store.SweepTimedOutRunningJobs(ctx, timeoutByKind, a.cfg.NodeJobMaxAttempts)
			if err != nil {
				log.Printf("sweep timed out node jobs: %v", err)
				continue
			}
			if affected > 0 {
				log.Printf("swept timed out node jobs affected=%d", affected)
			}
		}
	}
}
