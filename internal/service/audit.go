package service

import (
	"context"

	"neutrino/internal/repo"
)

// AuditStore is the narrow repo surface AuditService depends on. *repo.Store
// satisfies it.
type AuditStore interface {
	InsertAuditLog(ctx context.Context, actorType, actor, action, targetType, targetID, detailJSON, clientIP, userAgent, requestID string) error
}

var _ AuditStore = (*repo.Store)(nil)

// AuditService records audit log entries. It exists so HTTP handlers go
// through the service layer for every write, including the cross-cutting
// audit trail.
type AuditService struct {
	store AuditStore
}

func NewAuditService(store AuditStore) *AuditService {
	return &AuditService{store: store}
}

type AuditEntry struct {
	ActorType  string
	ActorID    string
	Action     string
	TargetType string
	TargetID   string
	DetailJSON string
	ClientIP   string
	UserAgent  string
	RequestID  string
}

func (s *AuditService) Record(ctx context.Context, e AuditEntry) error {
	return s.store.InsertAuditLog(ctx, e.ActorType, e.ActorID, e.Action, e.TargetType, e.TargetID, e.DetailJSON, e.ClientIP, e.UserAgent, e.RequestID)
}
