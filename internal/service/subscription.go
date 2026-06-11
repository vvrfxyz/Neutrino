package service

import (
	"context"

	"neutrino/internal/repo"
)

// SubscriptionResolution is everything the /sub renderer needs from the DB:
// the resolved user and the set of nodes the user may use. When Restricted is
// true the user has an explicit node allowlist and must never fall back to the
// env-configured default proxy.
type SubscriptionResolution struct {
	User       repo.User
	Nodes      []repo.Node
	Restricted bool
}

// ResolveSubscription looks up a subscription token and assembles the node
// list for rendering. Token validity (not found / inactive user) surfaces as
// repo.ErrUserNotFound / repo.ErrUserInactive.
func (s *UserService) ResolveSubscription(ctx context.Context, token string) (SubscriptionResolution, error) {
	u, _, err := s.store.GetUserBySubscriptionToken(ctx, token)
	if err != nil {
		return SubscriptionResolution{}, err
	}
	userNodeIDs, err := s.store.ListUserNodeIDs(ctx, u.ID)
	if err != nil {
		return SubscriptionResolution{}, err
	}
	nodes, err := s.store.ListEnabledNodesForUser(ctx, u.ID)
	if err != nil {
		return SubscriptionResolution{}, err
	}
	return SubscriptionResolution{
		User:       u,
		Nodes:      nodes,
		Restricted: len(userNodeIDs) > 0,
	}, nil
}
