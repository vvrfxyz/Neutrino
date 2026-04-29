package xrayapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	handlercmd "github.com/xtls/xray-core/app/proxyman/command"
	statscmd "github.com/xtls/xray-core/app/stats/command"
	"github.com/xtls/xray-core/common/protocol"
	cserial "github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/proxy/vless"
)

type Client struct {
	enabled    bool
	apiAddr    string
	inboundTag string
	flow       string
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

	conn, closeFn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

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

	conn, closeFn, err := c.dial(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer closeFn()

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

func (c *Client) addUser(ctx context.Context, email, uuid string) error {
	conn, closeFn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

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

func (c *Client) dial(parent context.Context) (*grpc.ClientConn, func(), error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	conn, err := grpc.DialContext(ctx, c.apiAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("dial xray api %s: %w", c.apiAddr, err)
	}
	return conn, func() {
		_ = conn.Close()
		cancel()
	}, nil
}

func isNotFoundErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not found")
}
