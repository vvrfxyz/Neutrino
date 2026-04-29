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
