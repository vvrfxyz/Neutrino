package service

import (
	"context"
	"database/sql"
	"errors"

	"neutrino/internal/repo"
)

type SyncRequester interface {
	RequestUsersSync(ctx context.Context)
	RequestUsersSyncNow(ctx context.Context)
	RequestUsersSyncForNodeNow(ctx context.Context, nodeID int64)
	RequestManagedXrayReload(ctx context.Context)
}

type UserService struct {
	store *repo.Store
	sync  SyncRequester
}

func NewUserService(store *repo.Store, sync SyncRequester) *UserService {
	return &UserService{store: store, sync: sync}
}

func (s *UserService) requestUsersSync(ctx context.Context) {
	if s.sync != nil {
		s.sync.RequestUsersSync(ctx)
	}
}

func (s *UserService) requestUsersSyncNow(ctx context.Context) {
	if s.sync != nil {
		s.sync.RequestUsersSyncNow(ctx)
	}
}

func (s *UserService) requestManagedXrayReload(ctx context.Context) {
	if s.sync != nil {
		s.sync.RequestManagedXrayReload(ctx)
	}
}

type UserDetail struct {
	User               repo.User
	SubscriptionToken  *repo.SubscriptionToken
	AccessEvents       []repo.UserEvent
	OnlineSessions     []repo.OnlineUser
	OnlineDeviceCount  int
	OnlineSessionCount int
	AllNodes           []repo.Node
	AllowedNodeIDs     map[int64]bool
}

func (s *UserService) RefreshLifecycleState(ctx context.Context) error {
	res, err := s.RefreshLifecycleStateNoSync(ctx)
	if err != nil {
		return err
	}
	if res.Changed() {
		s.requestUsersSyncNow(ctx)
	}
	return nil
}

type LifecycleRefreshResult struct {
	Expired     int
	Reactivated int
}

func (r LifecycleRefreshResult) Changed() bool {
	return r.Expired > 0 || r.Reactivated > 0
}

// RefreshLifecycleStateNoSync runs the lifecycle sweeps without requesting
// any users sync. Sync-preparation paths use it so the sweeps (which open
// their own transactions) never run inside a prepare transaction; callers
// must check Changed() and schedule a global reconcile themselves.
func (s *UserService) RefreshLifecycleStateNoSync(ctx context.Context) (LifecycleRefreshResult, error) {
	expired, err := s.store.SweepExpiredUsers(ctx)
	if err != nil {
		return LifecycleRefreshResult{}, err
	}
	reactivated, err := s.store.SweepQuotaWindows(ctx)
	if err != nil {
		return LifecycleRefreshResult{Expired: expired}, err
	}
	return LifecycleRefreshResult{Expired: expired, Reactivated: reactivated}, nil
}

func (s *UserService) List(ctx context.Context) ([]repo.User, error) {
	if err := s.RefreshLifecycleState(ctx); err != nil {
		return nil, err
	}
	return s.store.ListUsers(ctx)
}

func (s *UserService) ListWithOnlineStats(ctx context.Context, onlineWindowSec int) ([]repo.User, error) {
	users, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	stats, err := s.store.ListUserOnlineStats(ctx, onlineWindowSec)
	if err != nil {
		return nil, err
	}
	for i := range users {
		stat, ok := stats[users[i].ID]
		if !ok {
			continue
		}
		users[i].OnlineSessionCount = stat.SessionCount
		users[i].OnlineDeviceCount = stat.DeviceCount
	}
	return users, nil
}

func (s *UserService) Get(ctx context.Context, userID int64) (repo.User, error) {
	if err := s.RefreshLifecycleState(ctx); err != nil {
		return repo.User{}, err
	}
	return s.store.GetUser(ctx, userID)
}

func (s *UserService) GetDetail(ctx context.Context, userID int64, eventLimit, onlineWindowSec int) (UserDetail, error) {
	if err := s.RefreshLifecycleState(ctx); err != nil {
		return UserDetail{}, err
	}
	user, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return UserDetail{}, err
	}
	accessEvents, err := s.store.ListUserEventsFiltered(ctx, userID, eventLimit, "xray-access", nil)
	if err != nil {
		return UserDetail{}, err
	}
	onlineSessions, err := s.store.ListUserOnlineSessions(ctx, userID, onlineWindowSec)
	if err != nil {
		return UserDetail{}, err
	}
	allNodes, err := s.store.ListNodes(ctx)
	if err != nil {
		return UserDetail{}, err
	}
	allowedNodeIDs, err := s.store.ListUserNodeIDs(ctx, userID)
	if err != nil {
		return UserDetail{}, err
	}

	detail := UserDetail{
		User:               user,
		AccessEvents:       accessEvents,
		OnlineSessions:     onlineSessions,
		OnlineSessionCount: len(onlineSessions),
		AllNodes:           allNodes,
		AllowedNodeIDs:     make(map[int64]bool, len(allowedNodeIDs)),
	}
	onlineDevices := make(map[string]struct{}, len(onlineSessions))
	for _, item := range onlineSessions {
		if item.ClientIP != "" {
			onlineDevices[item.ClientIP] = struct{}{}
		}
	}
	detail.OnlineDeviceCount = len(onlineDevices)
	for _, id := range allowedNodeIDs {
		detail.AllowedNodeIDs[id] = true
	}
	if tok, err := s.store.GetSubscriptionTokenByUserID(ctx, userID); err == nil {
		detail.SubscriptionToken = &tok
	} else if !errors.Is(err, repo.ErrUserNotFound) && !errors.Is(err, sql.ErrNoRows) {
		return UserDetail{}, err
	}
	return detail, nil
}

func (s *UserService) Create(ctx context.Context, in repo.CreateUserInput) (repo.User, error) {
	u, err := s.store.CreateUser(ctx, in)
	if err != nil {
		return repo.User{}, err
	}
	s.requestUsersSyncNow(ctx)
	return u, nil
}

func (s *UserService) RotateProxyLink(ctx context.Context, userID int64) (repo.User, error) {
	u, err := s.store.CreateProxyLink(ctx, userID)
	if err != nil {
		return repo.User{}, err
	}
	s.requestUsersSyncNow(ctx)
	return u, nil
}

func (s *UserService) SetStatus(ctx context.Context, userID int64, status string) (repo.User, error) {
	u, err := s.store.SetUserStatus(ctx, userID, status)
	if err != nil {
		return repo.User{}, err
	}
	if status == "disabled" {
		s.requestManagedXrayReload(ctx)
	}
	s.requestUsersSyncNow(ctx)
	return u, nil
}

func (s *UserService) Delete(ctx context.Context, userID int64) (string, error) {
	username, err := s.store.DeleteUser(ctx, userID)
	if err != nil {
		return "", err
	}
	s.requestManagedXrayReload(ctx)
	s.requestUsersSyncNow(ctx)
	return username, nil
}

func (s *UserService) SetDeviceLimit(ctx context.Context, userID int64, limit int) (repo.User, error) {
	return s.store.SetUserDeviceLimit(ctx, userID, limit)
}

func (s *UserService) ResetQuota(ctx context.Context, userID int64, reason string) error {
	if err := s.store.ResetUserQuota(ctx, userID, reason); err != nil {
		return err
	}
	s.requestUsersSync(ctx)
	return nil
}

func (s *UserService) CreditQuota(ctx context.Context, userID int64, creditBytes int64) error {
	if err := s.store.CreditUserQuota(ctx, userID, creditBytes); err != nil {
		return err
	}
	s.requestUsersSync(ctx)
	return nil
}

func (s *UserService) ExtendPlan(ctx context.Context, userID int64, days int) error {
	if err := s.store.ExtendUserPlan(ctx, userID, days); err != nil {
		return err
	}
	s.requestUsersSync(ctx)
	return nil
}

func (s *UserService) SetNodeAccess(ctx context.Context, userID int64, nodeIDs []int64) error {
	if err := s.store.SetUserNodeAccess(ctx, userID, nodeIDs); err != nil {
		return err
	}
	s.requestUsersSyncNow(ctx)
	return nil
}

func (s *UserService) GetOrCreateSubscriptionToken(ctx context.Context, userID int64) (repo.SubscriptionToken, error) {
	return s.store.GetOrCreateSubscriptionToken(ctx, userID)
}

func (s *UserService) GetSubscriptionToken(ctx context.Context, userID int64) (repo.SubscriptionToken, error) {
	return s.store.GetSubscriptionTokenByUserID(ctx, userID)
}

func (s *UserService) RotateSubscriptionToken(ctx context.Context, userID int64) (repo.SubscriptionToken, error) {
	return s.store.RotateSubscriptionToken(ctx, userID)
}
