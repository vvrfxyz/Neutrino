package service

import (
	"context"
	"log"
	"time"

	"neutrino/internal/repo"
)

type NodeService struct {
	store               *repo.Store
	staleDeleteAfterSec int
	sync                SyncRequester
}

func NewNodeService(store *repo.Store, staleDeleteSec int, sync SyncRequester) *NodeService {
	return &NodeService{store: store, staleDeleteAfterSec: staleDeleteSec, sync: sync}
}

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
			s.sync.RequestUsersSyncForNodeNow(ctx, nodeID)
		}
	}
	return nil
}
