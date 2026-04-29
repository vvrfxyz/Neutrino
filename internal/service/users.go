package service

import (
	"context"
	"neutrino/internal/repo"
)

type SyncRequester interface {
	RequestUsersSync(ctx context.Context)
	RequestUsersSyncNow(ctx context.Context)
	RequestUsersSyncForNodeNow(ctx context.Context, nodeID int64)
}

type UserService struct {
	store *repo.Store
	sync  SyncRequester
}

func NewUserService(store *repo.Store, sync SyncRequester) *UserService {
	return &UserService{store: store, sync: sync}
}

func (s *UserService) RefreshLifecycleState(ctx context.Context) error {
	expired, err := s.store.SweepExpiredUsers(ctx)
	if err != nil {
		return err
	}
	reactivated, err := s.store.SweepQuotaWindows(ctx)
	if err != nil {
		return err
	}
	if expired > 0 || reactivated > 0 {
		s.sync.RequestUsersSyncNow(ctx)
	}
	return nil
}
