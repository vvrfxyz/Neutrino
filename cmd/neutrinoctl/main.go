// neutrinoctl is the node-side operations CLI (module 5): read-only
// inspection of agent-local state plus the existing fixed apply/rollback/test
// logic. It reads the same environment the node-agent container uses, so
// running it inside (or alongside) the agent container needs no extra flags:
//
//	docker exec neutrino-agent /app/neutrinoctl status
//
// Commands accept no shell, no external argv, no panel input — every action
// is a fixed local code path over agent-owned files.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"neutrino/internal/agent"
)

const usage = `neutrinoctl — Neutrino node-agent local operations

Usage: neutrinoctl [-json] <command> [args]

Commands:
  status            agent-local snapshot: state, users, queue, cert, xray config
  queue             usage disk-queue details (batches, bytes, quarantine, files)
  cert              mTLS client certificate expiry and renew window
  enroll-info       identity/connectivity material presence (no secrets printed)
  test-xray         run the configured XRAY_TEST_ARGS_JSON against the live config
  apply-preview     render the bootstrap template locally and check JSON validity
  backups           list on-disk xray config backups (newest last)
  rollback [name]   restore a config backup (latest if omitted) and reload xray

Flags:
  -json             machine-readable output

Configuration comes from the node-agent environment (STATE_PATH, QUEUE_DIR,
XRAY_CONFIG_PATH, PANEL_MTLS_*, XRAY_TEST_ARGS_JSON, XRAY_RELOAD_ARGS_JSON, …).
`

func main() {
	jsonOut := flag.Bool("json", false, "machine-readable output")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cfg := agent.ConfigFromEnv()
	cmd, rest := args[0], args[1:]

	var err error
	switch cmd {
	case "status":
		err = emit(*jsonOut, agent.CollectCtlStatus(cfg), printStatus)
	case "queue":
		err = emit(*jsonOut, agent.CollectCtlQueue(cfg, true), printQueue)
	case "cert":
		c := agent.CollectCtlCert(cfg)
		err = emit(*jsonOut, c, printCert)
		if err == nil && (!c.Present || c.RenewDue) {
			os.Exit(1)
		}
	case "enroll-info":
		err = emit(*jsonOut, agent.CollectCtlEnrollInfo(cfg), printEnrollInfo)
	case "test-xray":
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err = agent.RunXrayConfigTest(ctx, cfg); err == nil {
			fmt.Println("ok: xray config test passed")
		}
	case "apply-preview":
		var rendered string
		var valid bool
		rendered, valid, err = agent.RenderBootstrapPreview(cfg)
		if err == nil {
			fmt.Println(rendered)
			if !valid {
				fmt.Fprintln(os.Stderr, "error: rendered config is NOT valid json (unresolved placeholders?)")
				os.Exit(1)
			}
		}
	case "backups":
		var names []string
		names, err = agent.ListXrayBackups(cfg)
		if err == nil {
			if *jsonOut {
				err = emitJSON(names)
			} else if len(names) == 0 {
				fmt.Println("no backups")
			} else {
				for _, n := range names {
					fmt.Println(n)
				}
			}
		}
	case "rollback":
		name := ""
		if len(rest) > 0 {
			name = rest[0]
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		var out map[string]any
		out, err = agent.RollbackXrayLocal(ctx, cfg, name)
		if err == nil {
			err = emitJSON(out)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func emit[T any](jsonOut bool, v T, human func(T)) error {
	if jsonOut {
		return emitJSON(v)
	}
	human(v)
	return nil
}

func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func boolMark(b bool) string {
	if b {
		return "ok"
	}
	return "MISSING"
}

func printStatus(s agent.CtlStatus) {
	fmt.Printf("node_id:              %d\n", s.NodeID)
	if s.StateOK {
		fmt.Printf("state:                ok (%s)\n", s.StatePath)
	} else {
		fmt.Printf("state:                ERROR (%s): %s\n", s.StatePath, s.StateError)
	}
	fmt.Printf("synced_users:         %d (version %s)\n", s.SyncedUsers, short(s.SyncedUsersVersion))
	if s.PendingUsersSync != "" {
		fmt.Printf("pending_users_sync:   %s\n", s.PendingUsersSync)
	}
	fmt.Printf("xray_reload_pending:  %t\n", s.XrayReloadPending)
	fmt.Printf("stats:                epoch=%d acked_counters=%d\n", s.StatsEpoch, s.AckedStatCounters)
	fmt.Printf("xray_config:          %s exists=%t valid_json=%t\n", s.XrayConfigPath, s.XrayConfigExists, s.XrayConfigValid)
	fmt.Printf("reality:              %s %s\n", s.RealityPath, boolMark(s.RealityOK))
	fmt.Printf("queue:                batches=%d bytes=%d quarantined=%d\n", s.Queue.Batches, s.Queue.Bytes, s.Queue.QuarantinedBatches)
	printCert(s.Cert)
}

func printQueue(q agent.CtlQueue) {
	fmt.Printf("dir:          %s\n", q.Dir)
	fmt.Printf("batches:      %d\n", q.Batches)
	fmt.Printf("bytes:        %d\n", q.Bytes)
	fmt.Printf("quarantined:  %d\n", q.QuarantinedBatches)
	if q.OldestBatch != "" {
		fmt.Printf("oldest:       %s\n", q.OldestBatch)
	}
	for _, f := range q.Files {
		fmt.Printf("  %s\n", f)
	}
}

func printCert(c agent.CtlCert) {
	if !c.Present {
		fmt.Printf("cert:                 MISSING (%s): %s\n", c.Path, c.Error)
		return
	}
	fmt.Printf("cert:                 cn=%s not_after=%s days_left=%d renew_due=%t (window %dd)\n",
		c.CommonName, c.NotAfter, c.DaysLeft, c.RenewDue, c.RenewBefore)
}

func printEnrollInfo(e agent.CtlEnrollInfo) {
	fmt.Printf("node_id:          %d\n", e.NodeID)
	fmt.Printf("panel_url:        %s\n", e.PanelURL)
	fmt.Printf("panel_mtls_url:   %s\n", e.PanelMTLSURL)
	fmt.Printf("ca_cert:          %s %s\n", e.CACertPath, boolMark(e.CACertPresent))
	fmt.Printf("client_cert:      %s %s\n", e.ClientCertPath, boolMark(e.Cert.Present))
	fmt.Printf("client_key:       %s %s\n", e.ClientKeyPath, boolMark(e.ClientKeyOK))
	fmt.Printf("enroll_code_set:  %t\n", e.EnrollCodeSet)
	printCert(e.Cert)
}

func short(v string) string {
	if len(v) > 12 {
		return v[:12] + "…"
	}
	if v == "" {
		return "-"
	}
	return v
}
