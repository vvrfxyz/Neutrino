package app

import (
	"context"
	"log"
	"time"

	"neutrino/internal/repo"
	"neutrino/internal/service"
)

func usersDesiredVersion(users []repo.User) string {
	return service.UsersDesiredVersion(users)
}

func (a *App) enqueueUsersSyncForEnabledNodes(ctx context.Context) {
	if err := a.nodes().EnqueueUsersSyncForEnabledNodes(ctx); err != nil {
		log.Printf("list nodes for users_sync: %v", err)
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
			if err := a.nodes().EnqueueUsersSyncForEnabledNodes(ctx); err != nil {
				log.Printf("list nodes for users_sync: %v", err)
			}
			if err := a.nodes().ReconcileManagedXray(ctx); err != nil {
				log.Printf("xray reconcile: %v", err)
			}
		}
	}
}

func (a *App) startNodeJobTimeoutSweeper(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			affected, err := a.nodes().SweepTimedOutJobs(ctx, a.cfg.NodeJobMaxAttempts)
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
