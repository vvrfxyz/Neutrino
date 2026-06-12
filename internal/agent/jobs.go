package agent

import (
	"context"
	"encoding/json"
	"log"
	"neutrino/internal/probe"
	"strings"
	"time"
)

type jobResult struct {
	Status         string
	Retryable      bool
	HTTPStatus     *int
	ResultJSON     any
	Error          string
	AppliedVersion string
}

func (a *Agent) startJobRunner(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		pc := a.panelClient()
		if pc == nil {
			time.Sleep(2 * time.Second)
			continue
		}
		jobCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
		job, ok, err := pc.ClaimJob(jobCtx, 25)
		cancel()
		if err != nil {
			log.Printf("claim job failed: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		if !ok || job == nil {
			continue
		}

		res := a.execJob(ctx, job)
		// Re-fetch panel client: cert renewal can hot-swap the client between claim and finish.
		pc = a.panelClient()
		if pc == nil {
			time.Sleep(2 * time.Second)
			continue
		}
		finishCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = pc.FinishJob(finishCtx, job.ID, FinishJobRequest{
			Status:         res.Status,
			Retryable:      res.Retryable,
			HTTPStatus:     res.HTTPStatus,
			ResultJSON:     res.ResultJSON,
			Error:          res.Error,
			AppliedVersion: res.AppliedVersion,
			Attempt:        job.Attempts,
		})
		cancel()
		if err != nil {
			log.Printf("finish job id=%d kind=%s err=%v", job.ID, job.Kind, err)
		}
	}
}

func (a *Agent) execJob(ctx context.Context, job *NodeJob) jobResult {
	if job == nil {
		return jobResult{Status: "failed", Retryable: true, Error: "nil job"}
	}
	timeout := 0
	if job.TimeoutSec != nil && *job.TimeoutSec > 0 {
		timeout = *job.TimeoutSec
	}
	runCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	}
	defer cancel()

	switch strings.TrimSpace(job.Kind) {
	case "users_sync":
		outcome, err := a.execUsersSync(runCtx, job.PayloadJSON)
		if err != nil {
			return jobErr(err)
		}
		return jobResult{
			Status:         "succeeded",
			Retryable:      false,
			ResultJSON:     outcome.Result,
			AppliedVersion: outcome.AppliedVersion,
		}
	case "xray_apply":
		var req XrayApplyRequest
		if err := json.Unmarshal([]byte(job.PayloadJSON), &req); err != nil {
			return jobResult{Status: "failed", Retryable: false, Error: "invalid payload_json"}
		}
		out, err := a.configApplier().ApplyConfig(runCtx, req)
		if err != nil {
			return jobErr(err)
		}
		return jobResult{Status: "succeeded", ResultJSON: out, AppliedVersion: strings.TrimSpace(job.DesiredVersion)}
	case "xray_rollback":
		var req XrayRollbackRequest
		if err := json.Unmarshal([]byte(job.PayloadJSON), &req); err != nil {
			return jobResult{Status: "failed", Retryable: false, Error: "invalid payload_json"}
		}
		out, err := a.configApplier().RollbackConfig(runCtx, req)
		if err != nil {
			return jobErr(err)
		}
		appliedVersion := "rollback"
		if name, ok := out["backup_name"].(string); ok && strings.TrimSpace(name) != "" {
			appliedVersion = "rollback:" + strings.TrimSpace(name)
		}
		return jobResult{Status: "succeeded", ResultJSON: out, AppliedVersion: appliedVersion}
	case probe.KindDNS, probe.KindPing, probe.KindTCP, probe.KindHTTP:
		req, err := probe.ParsePayload(job.Kind, job.PayloadJSON)
		if err != nil {
			return jobResult{Status: "failed", Retryable: false, Error: err.Error()}
		}
		out := probe.Run(runCtx, job.Kind, req)
		if !out.Success {
			return jobResult{Status: "failed", Retryable: false, ResultJSON: out, Error: out.Error}
		}
		return jobResult{Status: "succeeded", Retryable: false, ResultJSON: out}
	default:
		return jobResult{Status: "failed", Retryable: false, Error: "unknown job kind"}
	}
}

type JobError struct {
	Retryable  bool
	Msg        string
	ResultJSON any
	HTTPStatus *int
}

func (e JobError) Error() string { return e.Msg }

func jobErr(err error) jobResult {
	if err == nil {
		return jobResult{Status: "succeeded"}
	}
	if je, ok := err.(JobError); ok {
		return jobResult{Status: "failed", Retryable: je.Retryable, HTTPStatus: je.HTTPStatus, ResultJSON: je.ResultJSON, Error: je.Msg}
	}
	return jobResult{Status: "failed", Retryable: true, Error: err.Error()}
}
