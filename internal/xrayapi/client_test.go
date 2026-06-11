package xrayapi

import (
	"context"
	"net"
	"testing"
	"time"

	statscmd "github.com/xtls/xray-core/app/stats/command"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeStatsServer struct {
	statscmd.UnimplementedStatsServiceServer
	ips map[string]int64
	err error
	got string
}

func (s *fakeStatsServer) GetStatsOnlineIpList(_ context.Context, req *statscmd.GetStatsRequest) (*statscmd.GetStatsOnlineIpListResponse, error) {
	s.got = req.GetName()
	if s.err != nil {
		return nil, s.err
	}
	return &statscmd.GetStatsOnlineIpListResponse{Name: req.GetName(), Ips: s.ips}, nil
}

func newFakeStatsClient(t *testing.T, srv *fakeStatsServer) *Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcSrv := grpc.NewServer()
	statscmd.RegisterStatsServiceServer(grpcSrv, srv)
	go func() {
		_ = grpcSrv.Serve(lis)
	}()
	t.Cleanup(func() {
		grpcSrv.Stop()
		_ = lis.Close()
	})
	return NewRaw(lis.Addr().String(), "vless-reality", "xtls-rprx-vision")
}

func TestPullOnlineIPsParsesIPsAndTimestamps(t *testing.T) {
	base := time.Date(2026, 5, 1, 12, 30, 0, 0, time.UTC)
	srv := &fakeStatsServer{ips: map[string]int64{
		"192.0.2.10":    base.Unix(),
		"2001:db8::1":   base.Add(time.Second).Unix(),
		"[2001:db8::2]": base.Add(2 * time.Second).Unix(),
	}}
	client := newFakeStatsClient(t, srv)

	got, err := client.PullOnlineIPs(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("PullOnlineIPs: %v", err)
	}
	if srv.got != "user>>>alice@example.com>>>online" {
		t.Fatalf("stat name=%q", srv.got)
	}
	if len(got) != 3 {
		t.Fatalf("len=%d, want 3: %+v", len(got), got)
	}
	seen := map[string]time.Time{}
	for _, item := range got {
		seen[item.IP] = item.LastSeenAt
	}
	if !seen["192.0.2.10"].Equal(base) {
		t.Fatalf("ipv4 timestamp=%s", seen["192.0.2.10"])
	}
	if !seen["2001:db8::1"].Equal(base.Add(time.Second)) {
		t.Fatalf("ipv6 timestamp=%s", seen["2001:db8::1"])
	}
	if !seen["2001:db8::2"].Equal(base.Add(2 * time.Second)) {
		t.Fatalf("bracketed ipv6 timestamp=%s", seen["2001:db8::2"])
	}
}

func TestPullOnlineIPsNotFoundReturnsEmpty(t *testing.T) {
	srv := &fakeStatsServer{err: status.Error(codes.NotFound, "missing not found.")}
	client := newFakeStatsClient(t, srv)

	got, err := client.PullOnlineIPs(context.Background(), "missing@example.com")
	if err != nil {
		t.Fatalf("PullOnlineIPs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len=%d, want empty", len(got))
	}
}

func TestPullOnlineIPsRejectsInvalidIPOrTimestamp(t *testing.T) {
	t.Run("invalid ip", func(t *testing.T) {
		client := newFakeStatsClient(t, &fakeStatsServer{ips: map[string]int64{"not-an-ip": time.Now().Unix()}})
		if _, err := client.PullOnlineIPs(context.Background(), "bad@example.com"); err == nil {
			t.Fatalf("expected invalid IP error")
		}
	})

	t.Run("malformed timestamp", func(t *testing.T) {
		client := newFakeStatsClient(t, &fakeStatsServer{ips: map[string]int64{"192.0.2.20": 0}})
		if _, err := client.PullOnlineIPs(context.Background(), "bad@example.com"); err == nil {
			t.Fatalf("expected malformed timestamp error")
		}
	})
}

func TestClientReusesConnectionAcrossCalls(t *testing.T) {
	srv := &fakeStatsServer{ips: map[string]int64{}}
	client := newFakeStatsClient(t, srv)
	defer client.Close()

	for i := 0; i < 3; i++ {
		if _, err := client.PullOnlineIPs(context.Background(), "alice@example.com"); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	client.mu.Lock()
	conn := client.conn
	client.mu.Unlock()
	if conn == nil {
		t.Fatalf("expected a cached shared connection after calls")
	}
}

func TestClientRecoversAfterServerRestart(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	srv := &fakeStatsServer{ips: map[string]int64{}}
	grpcSrv := grpc.NewServer()
	statscmd.RegisterStatsServiceServer(grpcSrv, srv)
	go func() { _ = grpcSrv.Serve(lis) }()

	client := NewRaw(addr, "vless-reality", "xtls-rprx-vision")
	defer client.Close()
	if _, err := client.PullOnlineIPs(context.Background(), "alice@example.com"); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Kill the server: the shared connection must surface an error, not hang.
	grpcSrv.Stop()
	_ = lis.Close()
	if _, err := client.PullOnlineIPs(context.Background(), "alice@example.com"); err == nil {
		t.Fatalf("expected error while server is down")
	}

	// Restart on the same address: the client must reconnect transparently.
	lis2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("could not rebind %s: %v", addr, err)
	}
	grpcSrv2 := grpc.NewServer()
	statscmd.RegisterStatsServiceServer(grpcSrv2, srv)
	go func() { _ = grpcSrv2.Serve(lis2) }()
	t.Cleanup(func() {
		grpcSrv2.Stop()
		_ = lis2.Close()
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := client.PullOnlineIPs(context.Background(), "alice@example.com"); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("client did not recover after server restart: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
