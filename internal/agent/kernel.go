// kernel.go names the kernel boundary (module 6): the agent talks to the
// local proxy core only through these small interfaces. internal/xrayapi is
// today's only implementation; a future core (e.g. sing-box) implements the
// same surface and nothing above this seam changes. Panel wire formats (job
// payload structs, usage contracts) are deliberately NOT part of the
// boundary — they belong to the panel/agent protocol, not the core.
package agent

import (
	"context"

	"neutrino/internal/xrayapi"
)

// OnlineIP mirrors xrayapi.OnlineIP at the boundary. It stays an alias until
// a second core implementation exists; converting it earlier would be
// adapter ceremony with no second caller.
type OnlineIP = xrayapi.OnlineIP

// UserApplier mutates the core's runtime user set. The runtime is
// email-keyed (see the delta_email_move rejection in users_sync.go).
type UserApplier interface {
	UpsertUser(ctx context.Context, email, uuid string) error
	RemoveUser(ctx context.Context, email string) error
}

// TrafficSampler reads per-user counters and online IPs from the core.
type TrafficSampler interface {
	PullUserTraffic(ctx context.Context, email string) (uplink, downlink int64, err error)
	PullOnlineIPs(ctx context.Context, email string) ([]OnlineIP, error)
}

// UptimeProber is an optional capability detected by type assertion: the
// runtime restart guard probes core uptime when the client offers it; test
// fakes and minimal cores need not.
type UptimeProber interface {
	SysUptime(ctx context.Context) (uptime uint32, ok bool, err error)
}

// RuntimeClient is the composite surface the Agent requires from the core.
type RuntimeClient interface {
	UserApplier
	TrafficSampler
}

// ConfigApplier executes a panel config job against the core. The xray
// implementation is the agent's file pipeline (render → validate/merge →
// backup → test → install → reload), reached through xrayConfigApplier.
type ConfigApplier interface {
	ApplyConfig(ctx context.Context, req XrayApplyRequest) (map[string]any, error)
	RollbackConfig(ctx context.Context, req XrayRollbackRequest) (map[string]any, error)
}

// xrayConfigApplier adapts the Agent's existing xray file pipeline to
// ConfigApplier. It is the default (and only) wiring today.
type xrayConfigApplier struct{ a *Agent }

func (x xrayConfigApplier) ApplyConfig(ctx context.Context, req XrayApplyRequest) (map[string]any, error) {
	return x.a.execXrayApply(ctx, req)
}

func (x xrayConfigApplier) RollbackConfig(ctx context.Context, req XrayRollbackRequest) (map[string]any, error) {
	return x.a.execXrayRollback(ctx, req)
}

// configApplier returns the wired ConfigApplier, defaulting to the xray file
// pipeline so directly-constructed Agents (tests, neutrinoctl helpers) work
// without explicit wiring.
func (a *Agent) configApplier() ConfigApplier {
	if a.config != nil {
		return a.config
	}
	return xrayConfigApplier{a: a}
}

// The production xray client must satisfy the full boundary.
var (
	_ RuntimeClient = (*xrayapi.Client)(nil)
	_ UptimeProber  = (*xrayapi.Client)(nil)
	_ ConfigApplier = xrayConfigApplier{}
)
