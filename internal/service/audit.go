package service

import (
	"context"

	"neutrino/internal/repo"
)

// AuditService records audit log entries. It exists so HTTP handlers go
// through the service layer for every write, including the cross-cutting
// audit trail.
type AuditService struct {
	store *repo.Store
}

func NewAuditService(store *repo.Store) *AuditService {
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
