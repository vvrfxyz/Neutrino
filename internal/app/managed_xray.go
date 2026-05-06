package app

import (
	"context"
	"log"

	"neutrino/internal/repo"
	"neutrino/internal/service"
)

type nodeExtra = service.NodeExtra
type nodeExtraXray = service.NodeExtraXray

func buildManagedXrayApplyPayload(node repo.Node) (string, error) {
	return service.BuildManagedXrayApplyPayload(node)
}

func buildManagedXrayRollbackPayload(node repo.Node, backupName string) (string, error) {
	return service.BuildManagedXrayRollbackPayload(node, backupName)
}

func ensureManagedXrayDesiredAndMaybeEnqueue(ctx context.Context, store *repo.Store, node repo.Node) (desiredVersion string, enqueued bool, jobID int64, err error) {
	result, err := service.NewNodeService(store, 0, nil).EnsureManagedXrayDesiredAndMaybeEnqueue(ctx, node)
	return result.DesiredVersion, result.Enqueued, result.JobID, err
}

func (a *App) enqueueManagedXrayReloadForEnabledNodes(ctx context.Context) {
	if err := a.nodes().EnqueueManagedXrayReloadForEnabledNodes(ctx); err != nil {
		log.Printf("list enabled nodes for xray reload: %v", err)
	}
}

func parseNodeExtra(raw string) (nodeExtra, error) {
	return service.ParseNodeExtra(raw)
}
