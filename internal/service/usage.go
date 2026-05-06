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
	if len(affected) > 0 && s.sync != nil {
		s.sync.RequestUsersSync(ctx)
	}
	return nil
}

func (s *UsageService) RecordBatch(ctx context.Context, batch []repo.UsageInput) ([]repo.UsageBatchItemResult, error) {
	results, err := s.store.RecordUsageBatchIdempotent(ctx, batch)
	if err != nil {
		return nil, err
	}
	needUsersSync := false
	for _, result := range results {
		if result.Err != nil || result.Duplicate || result.User == nil {
			continue
		}
		if result.User.Status != "active" {
			needUsersSync = true
			break
		}
	}
	if needUsersSync && s.sync != nil {
		s.sync.RequestUsersSync(ctx)
	}
	return results, nil
}
