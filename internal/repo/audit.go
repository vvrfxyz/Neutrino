package repo

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (s *Store) InsertAuditLog(ctx context.Context, actorType, actor, action, targetType, targetID, detailJSON, clientIP, userAgent, requestID string) error {
	actorType = strings.TrimSpace(actorType)
	actor = strings.TrimSpace(actor)
	action = strings.TrimSpace(action)
	targetType = strings.TrimSpace(targetType)
	targetID = strings.TrimSpace(targetID)
	if actorType == "" || actor == "" || action == "" || targetType == "" || targetID == "" {
		return fmt.Errorf("invalid audit log")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO audit_logs(actor_type, actor, action, target_type, target_id, detail_json, client_ip, user_agent, request_id, created_at)
VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?);
`, actorType, actor, action, targetType, targetID, strings.TrimSpace(detailJSON), strings.TrimSpace(clientIP), strings.TrimSpace(userAgent), strings.TrimSpace(requestID), now)
	return err
}
