package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"neutrino/internal/repo"
	"neutrino/internal/templates"
)

type nodeExtra struct {
	Xray *nodeExtraXray `json:"xray,omitempty"`
}

type nodeExtraXray struct {
	// Template is an optional full Xray config template (JSON text).
	// If empty, the server uses the built-in template.
	//
	// IMPORTANT: Do not embed secrets (e.g. REALITY private key) here. Keep secrets node-local via env/secret mount
	// and reference them as ${XRAY_REALITY_PRIVATE_KEY} for node-agent substitution.
	Template       string            `json:"template"`
	Vars           map[string]string `json:"vars"`
	RollbackOnFail bool              `json:"rollback_on_fail"`
}

type xrayApplyPayload struct {
	Template       string            `json:"template"`
	Vars           map[string]string `json:"vars"`
	RollbackOnFail bool              `json:"rollback_on_fail,omitempty"`
}

type xrayRollbackPayload struct {
	BackupName string `json:"backup_name,omitempty"`
}

func buildManagedXrayApplyPayload(node repo.Node) (string, error) {
	ex, err := parseNodeExtra(node.ExtraJSON)
	if err != nil {
		return "", err
	}
	cfg := ex.Xray
	if cfg == nil {
		// Defaults: rely on agent-local env (from node .env) for vars; rollback enabled for safety.
		cfg = &nodeExtraXray{RollbackOnFail: true}
	}
	tpl := strings.TrimSpace(cfg.Template)
	if tpl == "" {
		tpl = templates.XrayConfigTemplate
	}
	p := xrayApplyPayload{
		Template:       tpl,
		Vars:           cfg.Vars,
		RollbackOnFail: cfg.RollbackOnFail,
	}
	b, _ := json.Marshal(p)
	return string(b), nil
}

func buildManagedXrayRollbackPayload(node repo.Node, backupName string) (string, error) {
	if _, err := parseNodeExtra(node.ExtraJSON); err != nil {
		return "", err
	}
	p := xrayRollbackPayload{
		BackupName: strings.TrimSpace(backupName),
	}
	b, _ := json.Marshal(p)
	return string(b), nil
}

func ensureManagedXrayDesiredAndMaybeEnqueue(ctx context.Context, store *repo.Store, node repo.Node) (desiredVersion string, enqueued bool, jobID int64, err error) {
	if node.ID <= 0 {
		return "", false, 0, fmt.Errorf("invalid node id")
	}
	if !node.Managed || node.CoreType != "xray" {
		return "", false, 0, nil
	}
	payload, err := buildManagedXrayApplyPayload(node)
	if err != nil {
		return "", false, 0, err
	}
	desiredVersion = sha256Hex(payload)
	if err := store.UpsertNodeDesiredState(ctx, node.ID, "xray_apply", desiredVersion, payload); err != nil {
		return "", false, 0, err
	}
	if err := store.SetNodeDesiredXrayVersion(ctx, node.ID, desiredVersion); err != nil {
		return "", false, 0, err
	}
	if !node.Enabled {
		return desiredVersion, false, 0, nil
	}
	jobID, enqueued, err = store.EnqueueNodeJob(ctx, node.ID, "xray_apply", desiredVersion, payload, 120, "xray_apply")
	if err != nil {
		return desiredVersion, false, 0, err
	}
	return desiredVersion, enqueued, jobID, nil
}

func (a *App) enqueueManagedXrayReloadForEnabledNodes(ctx context.Context) {
	nodes, err := a.store.ListEnabledNodes(ctx)
	if err != nil {
		log.Printf("list enabled nodes for xray reload: %v", err)
		return
	}
	for _, node := range nodes {
		if !node.Managed || node.CoreType != "xray" {
			continue
		}
		payload, err := buildManagedXrayApplyPayload(node)
		if err != nil {
			log.Printf("build managed xray payload for node=%d: %v", node.ID, err)
			continue
		}
		desiredVersion := sha256Hex(payload)
		if err := a.store.UpsertNodeDesiredState(ctx, node.ID, "xray_apply", desiredVersion, payload); err != nil {
			log.Printf("upsert desired xray state for node=%d: %v", node.ID, err)
			continue
		}
		if err := a.store.SetNodeDesiredXrayVersion(ctx, node.ID, desiredVersion); err != nil {
			log.Printf("set desired xray version for node=%d: %v", node.ID, err)
			continue
		}
		if _, _, err := a.store.EnqueueNodeJob(ctx, node.ID, "xray_apply", desiredVersion, payload, 120, "user_disconnect"); err != nil {
			log.Printf("enqueue xray reload for node=%d: %v", node.ID, err)
		}
	}
}

func parseNodeExtra(raw string) (nodeExtra, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nodeExtra{}, nil
	}
	var ex nodeExtra
	if err := json.Unmarshal([]byte(raw), &ex); err != nil {
		return nodeExtra{}, err
	}
	return ex, nil
}
