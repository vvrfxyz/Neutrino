package repo

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestApplyOnlineSnapshotUpsertsDeletesStaleAndKeepsOtherNodes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u1 := newActiveUser(t, s)
	u2 := newActiveUser(t, s)
	node1, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "online-snapshot-node-1",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "node1.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node1: %v", err)
	}
	node2, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "online-snapshot-node-2",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "node2.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node2: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	if err := s.ApplyOnlineSnapshot(ctx, node1.ID, OnlineSnapshotInput{
		ObservedAt: now,
		Items: []OnlineSnapshotItemInput{
			{UserID: u1.ID, ClientIP: "10.0.0.1", LastSeenAt: now.Add(-time.Second)},
			{UserID: u1.ID, ClientIP: "10.0.0.2", LastSeenAt: now},
		},
	}); err != nil {
		t.Fatalf("apply initial node1 snapshot: %v", err)
	}
	if err := s.ApplyOnlineSnapshot(ctx, node2.ID, OnlineSnapshotInput{
		ObservedAt: now,
		Items: []OnlineSnapshotItemInput{
			{UserID: u2.ID, ClientIP: "10.0.1.1", LastSeenAt: now},
		},
	}); err != nil {
		t.Fatalf("apply node2 snapshot: %v", err)
	}

	next := now.Add(30 * time.Second)
	if err := s.ApplyOnlineSnapshot(ctx, node1.ID, OnlineSnapshotInput{
		ObservedAt: next,
		Items: []OnlineSnapshotItemInput{
			{UserID: u1.ID, ClientIP: "10.0.0.2", LastSeenAt: next},
			{UserID: u2.ID, ClientIP: "2001:db8::1", LastSeenAt: next.Add(time.Second)},
		},
	}); err != nil {
		t.Fatalf("apply replacement node1 snapshot: %v", err)
	}

	var rows []struct {
		userID   int64
		clientIP string
		nodeID   int64
	}
	rs, err := s.RawDB().QueryContext(ctx, `
SELECT user_id, client_ip, node_id
FROM online_sessions
ORDER BY node_id, user_id, client_ip;
`)
	if err != nil {
		t.Fatalf("query online_sessions: %v", err)
	}
	defer rs.Close()
	for rs.Next() {
		var row struct {
			userID   int64
			clientIP string
			nodeID   int64
		}
		if err := rs.Scan(&row.userID, &row.clientIP, &row.nodeID); err != nil {
			t.Fatalf("scan online row: %v", err)
		}
		rows = append(rows, row)
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %+v", rows)
	}
	want := map[string]bool{
		"1:u1:10.0.0.2":    false,
		"1:u2:2001:db8::1": false,
		"2:u2:10.0.1.1":    false,
	}
	for _, row := range rows {
		userLabel := "u2"
		if row.userID == u1.ID {
			userLabel = "u1"
		}
		key := strings.Join([]string{
			func() string {
				if row.nodeID == node1.ID {
					return "1"
				}
				return "2"
			}(),
			userLabel,
			row.clientIP,
		}, ":")
		if _, ok := want[key]; !ok {
			t.Fatalf("unexpected row key=%s rows=%+v", key, rows)
		}
		want[key] = true
	}
	for key, seen := range want {
		if !seen {
			t.Fatalf("missing row %s rows=%+v", key, rows)
		}
	}
}

func TestApplyOnlineSnapshotValidatesInputs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newActiveUser(t, s)
	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "online-snapshot-validate-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "node.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	if err := s.ApplyOnlineSnapshot(ctx, 0, OnlineSnapshotInput{}); err == nil {
		t.Fatalf("expected invalid node id error")
	}
	if err := s.ApplyOnlineSnapshot(ctx, node.ID, OnlineSnapshotInput{
		Items: []OnlineSnapshotItemInput{{UserID: 0, ClientIP: "10.0.0.1"}},
	}); err == nil {
		t.Fatalf("expected invalid user id error")
	}
	if err := s.ApplyOnlineSnapshot(ctx, node.ID, OnlineSnapshotInput{
		Items: []OnlineSnapshotItemInput{{UserID: u.ID, ClientIP: "not-an-ip"}},
	}); err == nil {
		t.Fatalf("expected invalid client ip error")
	}
}

func TestApplyOnlineSnapshotEmptySnapshotClearsNodeRows(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newActiveUser(t, s)
	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "online-snapshot-empty-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "node.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.ApplyOnlineSnapshot(ctx, node.ID, OnlineSnapshotInput{
		ObservedAt: now,
		Items: []OnlineSnapshotItemInput{
			{UserID: u.ID, ClientIP: "10.0.0.1", LastSeenAt: now},
		},
	}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	if err := s.ApplyOnlineSnapshot(ctx, node.ID, OnlineSnapshotInput{
		ObservedAt: now.Add(time.Second),
		Items:      []OnlineSnapshotItemInput{},
	}); err != nil {
		t.Fatalf("apply empty snapshot: %v", err)
	}
	sessions, err := s.ListUserOnlineSessions(ctx, u.ID, 120)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("empty snapshot should clear node rows, got %+v", sessions)
	}
}

func TestEnforceIPLimitUsesSnapshotOnlineSessions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, err := s.CreateUser(ctx, CreateUserInput{
		Username:       "snapshot-ip-limit-user",
		MonthlyLimitGB: 500,
		CountingMode:   "double",
		QuotaCycle:     "month",
		QuotaTZ:        "UTC",
		DeviceLimit:    1,
		PlanDays:       30,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "snapshot-ip-limit-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "node.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.ApplyOnlineSnapshot(ctx, node.ID, OnlineSnapshotInput{
		ObservedAt: now,
		Items: []OnlineSnapshotItemInput{
			{UserID: u.ID, ClientIP: "10.0.0.1", LastSeenAt: now},
			{UserID: u.ID, ClientIP: "10.0.0.2", LastSeenAt: now},
		},
	}); err != nil {
		t.Fatalf("apply snapshot: %v", err)
	}

	affected, err := s.EnforceIPLimit(ctx, 120, 1)
	if err != nil {
		t.Fatalf("enforce ip limit: %v", err)
	}
	if len(affected) != 1 {
		t.Fatalf("expected one affected user, got %d", len(affected))
	}
	if affected[0].Status != "over_ip_limit" {
		t.Fatalf("status=%s, want over_ip_limit", affected[0].Status)
	}
}

func TestRecordUsageDoesNotUpdateOnlineSessions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newActiveUser(t, s)
	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "usage-no-online-node",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "node.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	nodeID := node.ID

	if _, err := s.RecordUsage(ctx, UsageInput{
		UserID:        u.ID,
		NodeID:        &nodeID,
		Direction:     "outbound",
		Bytes:         1,
		Source:        "test",
		SourceEventID: "usage-no-online",
		ClientIP:      "10.0.0.9",
		At:            time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	sessions, err := s.ListUserOnlineSessions(ctx, u.ID, 120)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("usage should not create online sessions, got %+v", sessions)
	}
}
