package xrayapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	handlercmd "github.com/xtls/xray-core/app/proxyman/command"
	statscmd "github.com/xtls/xray-core/app/stats/command"
	"github.com/xtls/xray-core/common/protocol"
	cserial "github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/proxy/vless"
)

// rpcTimeout bounds every xray API call so a hung xray cannot stall the
// agent's usage or heartbeat loops.
const rpcTimeout = 4 * time.Second

type Client struct {
	enabled    bool
	apiAddr    string
	inboundTag string
	flow       string

	mu   sync.Mutex
	conn *grpc.ClientConn
}

type OnlineIP struct {
	IP         string
	LastSeenAt time.Time
}

// NewRaw constructs a client without depending on config.Config.
// It is intended for node-agent usage.
func NewRaw(apiAddr, inboundTag, flow string) *Client {
	return &Client{
		enabled:    true,
		apiAddr:    strings.TrimSpace(apiAddr),
		inboundTag: strings.TrimSpace(inboundTag),
		flow:       strings.TrimSpace(flow),
	}
}

func (c *Client) Enabled() bool {
	return c != nil && c.enabled && c.apiAddr != ""
}

func (c *Client) UpsertUser(ctx context.Context, email, uuid string) error {
	if !c.Enabled() {
		return nil
	}
	if email == "" || uuid == "" {
		return errors.New("empty email or uuid")
	}

	_ = c.RemoveUser(ctx, email)
	return c.addUser(ctx, email, uuid)
}

func (c *Client) RemoveUser(ctx context.Context, email string) error {
	if !c.Enabled() {
		return nil
	}
	if email == "" {
		return nil
	}

	conn, err := c.getConn()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	client := handlercmd.NewHandlerServiceClient(conn)
	_, err = client.AlterInbound(ctx, &handlercmd.AlterInboundRequest{
		Tag: c.inboundTag,
		Operation: cserial.ToTypedMessage(&handlercmd.RemoveUserOperation{
			Email: email,
		}),
	})
	if err != nil && !isNotFoundErr(err) {
		return err
	}
	return nil
}

func (c *Client) PullUserTraffic(ctx context.Context, email string) (uplink, downlink int64, err error) {
	if !c.Enabled() {
		return 0, 0, nil
	}
	if email == "" {
		return 0, 0, errors.New("empty email")
	}

	conn, err := c.getConn()
	if err != nil {
		return 0, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	stats := statscmd.NewStatsServiceClient(conn)
	uplink, err = c.readStat(ctx, stats, "user>>>"+email+">>>traffic>>>uplink")
	if err != nil {
		return 0, 0, err
	}
	downlink, err = c.readStat(ctx, stats, "user>>>"+email+">>>traffic>>>downlink")
	if err != nil {
		return 0, 0, err
	}
	return uplink, downlink, nil
}

func (c *Client) PullOnlineIPs(ctx context.Context, email string) ([]OnlineIP, error) {
	if !c.Enabled() {
		return nil, nil
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, errors.New("empty email")
	}

	conn, err := c.getConn()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	stats := statscmd.NewStatsServiceClient(conn)
	resp, err := stats.GetStatsOnlineIpList(ctx, &statscmd.GetStatsRequest{
		Name:   "user>>>" + email + ">>>online",
		Reset_: false,
	})
	if err != nil {
		if isNotFoundErr(err) {
			return []OnlineIP{}, nil
		}
		return nil, err
	}
	ips := resp.GetIps()
	out := make([]OnlineIP, 0, len(ips))
	for rawIP, unixSec := range ips {
		ip, ok := normalizeOnlineIP(rawIP)
		if !ok {
			return nil, fmt.Errorf("xray online ip for %s is invalid: %q", email, rawIP)
		}
		if unixSec <= 0 {
			return nil, fmt.Errorf("xray online ip for %s has malformed timestamp: %d", email, unixSec)
		}
		out = append(out, OnlineIP{
			IP:         ip,
			LastSeenAt: time.Unix(unixSec, 0).UTC(),
		})
	}
	return out, nil
}

func normalizeOnlineIP(raw string) (string, bool) {
	ip := strings.TrimSpace(raw)
	if strings.HasPrefix(ip, "[") && strings.HasSuffix(ip, "]") {
		ip = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(ip, "["), "]"))
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "", false
	}
	return parsed.String(), true
}

func (c *Client) addUser(ctx context.Context, email, uuid string) error {
	conn, err := c.getConn()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	user := &protocol.User{
		Level: 0,
		Email: email,
		Account: cserial.ToTypedMessage(&vless.Account{
			Id:         uuid,
			Flow:       c.flow,
			Encryption: "none",
		}),
	}

	client := handlercmd.NewHandlerServiceClient(conn)
	_, err = client.AlterInbound(ctx, &handlercmd.AlterInboundRequest{
		Tag: c.inboundTag,
		Operation: cserial.ToTypedMessage(&handlercmd.AddUserOperation{
			User: user,
		}),
	})
	return err
}

func (c *Client) readStat(ctx context.Context, stats statscmd.StatsServiceClient, name string) (int64, error) {
	resp, err := stats.GetStats(ctx, &statscmd.GetStatsRequest{
		Name:   name,
		Reset_: false,
	})
	if err != nil {
		if isNotFoundErr(err) {
			return 0, nil
		}
		return 0, err
	}
	if resp == nil || resp.Stat == nil {
		return 0, nil
	}
	return resp.Stat.Value, nil
}

// getConn returns a shared gRPC client connection, creating it lazily. The
// connection is non-blocking: gRPC's channel state machine transparently
// reconnects after xray restarts, and per-RPC timeouts (rpcTimeout) bound each
// call instead of a blocking dial. This replaces the previous dial-per-call
// behavior, which opened (and tore down) a TCP connection for every stats or
// user-management RPC.
func (c *Client) getConn() (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return c.conn, nil
	}
	conn, err := grpc.NewClient(c.apiAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("create xray api client %s: %w", c.apiAddr, err)
	}
	c.conn = conn
	return conn, nil
}

// Close releases the shared connection. Safe to call multiple times.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

func isNotFoundErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not found")
}
