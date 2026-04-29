package repo

import (
	"context"
	"testing"
	"time"
)

func TestTrafficSummary_OneHourBucketingAndTopDestinations(t *testing.T) {
	s := newTestStore(t)
	u := newActiveUser(t, s)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(5 * time.Minute).Add(-5 * time.Minute)
	start := now.Truncate(5 * time.Minute).Add(5 * time.Minute).Add(-1 * time.Hour)

	// 1h window in code: 5-min buckets.
	evts := []UsageInput{
		// Series buckets (bytes-based): inbound lands in first bucket, outbound in second.
		{UserID: u.ID, Direction: "inbound", Bytes: 100, At: start.Add(2 * time.Minute), Source: "manual", SourceEventID: "m1"},
		{UserID: u.ID, Direction: "outbound", Bytes: 200, At: start.Add(7 * time.Minute), Source: "manual", SourceEventID: "m2"},
		// Destination metadata: xray-access has bytes=0, but should count correctly.
		{UserID: u.ID, Direction: "outbound", Bytes: 0, At: start.Add(10 * time.Minute), Source: "xray-access", SourceEventID: "a1", TargetHost: "a.com", SNI: "a.com", Destination: "tcp:a.com:443"},
		{UserID: u.ID, Direction: "outbound", Bytes: 0, At: start.Add(11 * time.Minute), Source: "xray-access", SourceEventID: "a2", TargetHost: "a.com", SNI: "a.com", Destination: "tcp:a.com:443"},
		{UserID: u.ID, Direction: "outbound", Bytes: 0, At: start.Add(12 * time.Minute), Source: "xray-access", SourceEventID: "a3", TargetHost: "a.com", SNI: "a.com", Destination: "tcp:a.com:443"},
		{UserID: u.ID, Direction: "outbound", Bytes: 0, At: start.Add(13 * time.Minute), Source: "xray-access", SourceEventID: "b1", TargetHost: "b.com", SNI: "b.com", Destination: "tcp:b.com:443"},
	}
	for _, e := range evts {
		if _, err := s.RecordUsageIdempotent(ctx, e); err != nil {
			t.Fatalf("RecordUsageIdempotent: %v", err)
		}
	}

	sum, err := s.getTrafficSummaryAt(ctx, "1h", nil, nil, now)
	if err != nil {
		t.Fatalf("GetTrafficSummary: %v", err)
	}
	if len(sum.Series) != 12 {
		t.Fatalf("expected 12 buckets, got %d", len(sum.Series))
	}
	if got := sum.Series[0].BucketStart; !got.Equal(start) {
		t.Fatalf("unexpected first bucket start: %s", got.Format(time.RFC3339))
	}
	if sum.Series[0].InboundBytes != 100 || sum.Series[0].OutboundBytes != 0 {
		t.Fatalf("unexpected first bucket bytes in=%d out=%d", sum.Series[0].InboundBytes, sum.Series[0].OutboundBytes)
	}
	if sum.Series[1].OutboundBytes != 200 {
		t.Fatalf("unexpected second bucket outbound bytes=%d", sum.Series[1].OutboundBytes)
	}

	if len(sum.TopDestinations) == 0 || sum.TopDestinations[0].HostOrIP != "a.com" || sum.TopDestinations[0].Count != 3 {
		t.Fatalf("unexpected top destination: %+v", sum.TopDestinations)
	}
	if len(sum.TopSNI) == 0 || sum.TopSNI[0].SNI != "a.com" || sum.TopSNI[0].Count != 3 {
		t.Fatalf("unexpected top sni: %+v", sum.TopSNI)
	}
}

func TestTrafficSummary_SevenDaysUsesRollups(t *testing.T) {
	s := newTestStore(t)
	u := newActiveUser(t, s)
	ctx := context.Background()

	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	node, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "n1",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	nodeID := node.ID

	// Rollups are recorded from xray-stats events only.
	if _, err := s.RecordUsageIdempotent(ctx, UsageInput{
		UserID:        u.ID,
		NodeID:        &nodeID,
		Direction:     "outbound",
		Bytes:         1234,
		At:            dayStart,
		Source:        "xray-stats",
		SourceEventID: "stats-1",
	}); err != nil {
		t.Fatalf("RecordUsageIdempotent: %v", err)
	}

	sum, err := s.getTrafficSummaryAt(ctx, "7d", nil, nil, now)
	if err != nil {
		t.Fatalf("GetTrafficSummary: %v", err)
	}
	if len(sum.Series) != 7 {
		t.Fatalf("expected 7 buckets, got %d", len(sum.Series))
	}
	last := sum.Series[len(sum.Series)-1]
	if !last.BucketStart.Equal(dayStart) {
		t.Fatalf("unexpected last bucket start: %s", last.BucketStart.Format(time.RFC3339))
	}
	if last.OutboundBytes != 1234 {
		t.Fatalf("unexpected last bucket outbound bytes=%d", last.OutboundBytes)
	}
}

func TestTrafficSummary_FiltersByUserAndNode(t *testing.T) {
	s := newTestStore(t)
	u1 := newActiveUser(t, s)
	u2 := newActiveUser(t, s)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(5 * time.Minute).Add(-5 * time.Minute)
	n1, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "n-filter-1",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "n1.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateNode n1: %v", err)
	}
	n2, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "n-filter-2",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "n2.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateNode n2: %v", err)
	}

	events := []UsageInput{
		{UserID: u1.ID, NodeID: &n1.ID, Direction: "outbound", Bytes: 100, At: now.Add(-30 * time.Minute), Source: "xray-stats", SourceEventID: "f-1"},
		{UserID: u1.ID, NodeID: &n2.ID, Direction: "outbound", Bytes: 50, At: now.Add(-25 * time.Minute), Source: "xray-stats", SourceEventID: "f-2"},
		{UserID: u2.ID, NodeID: &n1.ID, Direction: "outbound", Bytes: 200, At: now.Add(-20 * time.Minute), Source: "xray-stats", SourceEventID: "f-3"},
	}
	for _, e := range events {
		if _, err := s.RecordUsageIdempotent(ctx, e); err != nil {
			t.Fatalf("RecordUsageIdempotent: %v", err)
		}
	}

	sumNode1, err := s.getTrafficSummaryAt(ctx, "1h", &n1.ID, nil, now)
	if err != nil {
		t.Fatalf("GetTrafficSummary node filter: %v", err)
	}
	if sumNode1.KPI.TotalOut != 300 {
		t.Fatalf("unexpected node filter total_out=%d", sumNode1.KPI.TotalOut)
	}

	sumUser1, err := s.getTrafficSummaryAt(ctx, "1h", nil, &u1.ID, now)
	if err != nil {
		t.Fatalf("GetTrafficSummary user filter: %v", err)
	}
	if sumUser1.KPI.TotalOut != 150 {
		t.Fatalf("unexpected user filter total_out=%d", sumUser1.KPI.TotalOut)
	}

	sumBoth, err := s.getTrafficSummaryAt(ctx, "1h", &n1.ID, &u1.ID, now)
	if err != nil {
		t.Fatalf("GetTrafficSummary user+node filter: %v", err)
	}
	if sumBoth.KPI.TotalOut != 100 {
		t.Fatalf("unexpected user+node filter total_out=%d", sumBoth.KPI.TotalOut)
	}
}

func TestTrafficSummary_TwentyFourHoursAggregatesByHour(t *testing.T) {
	s := newTestStore(t)
	u1 := newActiveUser(t, s)
	u2 := newActiveUser(t, s)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Hour).Add(-30 * time.Minute)
	firstBucket := now.Truncate(time.Hour).Add(-2 * time.Hour)
	secondBucket := firstBucket.Add(time.Hour)
	n1, err := s.CreateNode(ctx, CreateNodeInput{
		Name:     "n-24h",
		CoreType: "xray",
		Protocol: "vless_reality",
		Host:     "n24.example.com",
		Port:     443,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	events := []UsageInput{
		{UserID: u1.ID, NodeID: &n1.ID, Direction: "inbound", Bytes: 100, At: firstBucket.Add(5 * time.Minute), Source: "manual", SourceEventID: "24h-1"},
		{UserID: u1.ID, NodeID: &n1.ID, Direction: "outbound", Bytes: 200, At: firstBucket.Add(25 * time.Minute), Source: "manual", SourceEventID: "24h-2"},
		{UserID: u1.ID, NodeID: &n1.ID, Direction: "outbound", Bytes: 300, At: firstBucket.Add(55 * time.Minute), Source: "manual", SourceEventID: "24h-3"},
		{UserID: u1.ID, NodeID: &n1.ID, Direction: "inbound", Bytes: 400, At: secondBucket.Add(10 * time.Minute), Source: "manual", SourceEventID: "24h-4"},
		{UserID: u2.ID, NodeID: &n1.ID, Direction: "outbound", Bytes: 999, At: firstBucket.Add(35 * time.Minute), Source: "manual", SourceEventID: "24h-5"},
	}
	for _, e := range events {
		if _, err := s.RecordUsageIdempotent(ctx, e); err != nil {
			t.Fatalf("RecordUsageIdempotent: %v", err)
		}
	}

	sum, err := s.getTrafficSummaryAt(ctx, "24h", &n1.ID, &u1.ID, now)
	if err != nil {
		t.Fatalf("GetTrafficSummary 24h: %v", err)
	}
	if len(sum.Series) != 24 {
		t.Fatalf("expected 24 buckets, got %d", len(sum.Series))
	}

	var first, second *TrafficBucket
	for i := range sum.Series {
		b := &sum.Series[i]
		switch b.BucketStart {
		case firstBucket:
			first = b
		case secondBucket:
			second = b
		}
	}
	if first == nil || first.InboundBytes != 100 || first.OutboundBytes != 500 {
		t.Fatalf("unexpected first bucket: %+v", first)
	}
	if second == nil || second.InboundBytes != 400 || second.OutboundBytes != 0 {
		t.Fatalf("unexpected second bucket: %+v", second)
	}
	if sum.KPI.TotalIn != 500 || sum.KPI.TotalOut != 500 {
		t.Fatalf("unexpected 24h kpi: %+v", sum.KPI)
	}
}
