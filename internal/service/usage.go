package service

import (
	"context"
	"neutrino/internal/repo"
)

// UsageStore is the narrow repo surface UsageService depends on. *repo.Store
// satisfies it.
type UsageStore interface {
	EnforceIPLimit(ctx context.Context, windowSec, strikes int) ([]repo.User, error)
	RecordUsageBatchIdempotent(ctx context.Context, in []repo.UsageInput) ([]repo.UsageBatchItemResult, error)
	GetTrafficSummary(ctx context.Context, rangeParam string, nodeID *int64, userID *int64) (repo.TrafficSummary, error)
	GetTrafficSeries(ctx context.Context, userID int64, period string) ([]repo.TrafficBucket, error)
	ListUserEventsFiltered(ctx context.Context, userID int64, limit int, source string, nodeID *int64) ([]repo.UserEvent, error)
}

var _ UsageStore = (*repo.Store)(nil)

type UsageService struct {
	store           UsageStore
	onlineWindowSec int
	ipLimitStrikes  int
	sync            SyncRequester
}

func NewUsageService(store UsageStore, onlineWindowSec, ipLimitStrikes int, sync SyncRequester) *UsageService {
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

func (s *UsageService) TrafficSummary(ctx context.Context, rangeParam string, nodeID, userID *int64) (repo.TrafficSummary, error) {
	return s.store.GetTrafficSummary(ctx, rangeParam, nodeID, userID)
}

func (s *UsageService) TrafficSeries(ctx context.Context, userID int64, period string) ([]repo.TrafficBucket, error) {
	return s.store.GetTrafficSeries(ctx, userID, period)
}

func (s *UsageService) UserEvents(ctx context.Context, userID int64, limit int, source string, nodeID *int64) ([]repo.UserEvent, error) {
	return s.store.ListUserEventsFiltered(ctx, userID, limit, source, nodeID)
}
