package agent

import (
	"context"
	"testing"
)

// fakeConfigApplier records calls; stands in for a future non-xray core.
type fakeConfigApplier struct {
	applied    []XrayApplyRequest
	rolledBack []XrayRollbackRequest
}

func (f *fakeConfigApplier) ApplyConfig(_ context.Context, req XrayApplyRequest) (map[string]any, error) {
	f.applied = append(f.applied, req)
	return map[string]any{"ok": true, "runtime_reloaded": true}, nil
}

func (f *fakeConfigApplier) RollbackConfig(_ context.Context, req XrayRollbackRequest) (map[string]any, error) {
	f.rolledBack = append(f.rolledBack, req)
	return map[string]any{"ok": true, "backup_name": "config.json.bak.x"}, nil
}

// Job dispatch must go through the ConfigApplier seam: with a fake injected,
// xray_apply/xray_rollback never touch the xray file pipeline.
func TestExecJobUsesConfigApplierSeam(t *testing.T) {
	fake := &fakeConfigApplier{}
	a := &Agent{config: fake} // no xrayConfigPath: file pipeline would fail loudly

	res := a.execJob(context.Background(), &NodeJob{
		ID: 1, Kind: "xray_apply", PayloadJSON: `{"template":"{}"}`, DesiredVersion: "v1",
	})
	if res.Status != "succeeded" {
		t.Fatalf("apply via seam failed: %+v", res)
	}
	if len(fake.applied) != 1 || fake.applied[0].Template != "{}" {
		t.Fatalf("apply did not reach the injected ConfigApplier: %+v", fake.applied)
	}
	if res.AppliedVersion != "v1" {
		t.Fatalf("applied version lost: %+v", res)
	}

	res = a.execJob(context.Background(), &NodeJob{
		ID: 2, Kind: "xray_rollback", PayloadJSON: `{"backup_name":""}`,
	})
	if res.Status != "succeeded" {
		t.Fatalf("rollback via seam failed: %+v", res)
	}
	if len(fake.rolledBack) != 1 {
		t.Fatalf("rollback did not reach the injected ConfigApplier")
	}
	if res.AppliedVersion != "rollback:config.json.bak.x" {
		t.Fatalf("rollback applied version wrong: %q", res.AppliedVersion)
	}
}

// Without explicit wiring the seam defaults to the xray file pipeline —
// the behavior-preserving default for production and old tests.
func TestConfigApplierDefaultsToXrayPipeline(t *testing.T) {
	a := &Agent{}
	if _, ok := a.configApplier().(xrayConfigApplier); !ok {
		t.Fatalf("default ConfigApplier is not the xray pipeline: %T", a.configApplier())
	}
	// And the default pipeline still enforces its own preconditions.
	_, err := a.configApplier().ApplyConfig(context.Background(), XrayApplyRequest{Template: "{}"})
	if err == nil {
		t.Fatalf("expected XRAY_CONFIG_PATH error from the real pipeline")
	}
}
