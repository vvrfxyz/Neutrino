package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reloadCounter returns reload args that append a marker line to a counter
// file each time xray is asked to reload, plus a function to read the counter.
func reloadCounter(t *testing.T, dir string) ([]string, func() int) {
	t.Helper()
	counterPath := filepath.Join(dir, "reload.count")
	args := []string{"sh", "-c", "echo x >> " + counterPath}
	read := func() int {
		b, err := os.ReadFile(counterPath)
		if err != nil {
			if os.IsNotExist(err) {
				return 0
			}
			t.Fatalf("read counter: %v", err)
		}
		return strings.Count(string(b), "x")
	}
	return args, read
}

func TestExecXrayApplySkipsWhenCanonicalConfigUnchanged(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	args, count := reloadCounter(t, dir)

	a := &Agent{xrayConfigPath: configPath, xrayReloadArgs: args}
	template := `{"hello":"world","port":443}`

	res, err := a.execXrayApply(context.Background(), XrayApplyRequest{Template: template})
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if skipped, _ := res["skipped"].(bool); skipped {
		t.Fatalf("first apply should not skip; res=%+v", res)
	}
	if c := count(); c != 1 {
		t.Fatalf("counter after first apply = %d, want 1", c)
	}

	res, err = a.execXrayApply(context.Background(), XrayApplyRequest{Template: template})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if skipped, _ := res["skipped"].(bool); !skipped {
		t.Fatalf("second apply should skip; res=%+v", res)
	}
	if c := count(); c != 1 {
		t.Fatalf("counter must not advance on skip; got %d", c)
	}
}

func TestExecXrayApplySkipsWhitespaceOnlyDiff(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	args, count := reloadCounter(t, dir)

	// Seed an existing config with non-canonical whitespace + key order.
	pretty := "{\n    \"b\": 2,\n    \"a\": 1\n}\n"
	if err := os.WriteFile(configPath, []byte(pretty), 0600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	a := &Agent{xrayConfigPath: configPath, xrayReloadArgs: args}
	res, err := a.execXrayApply(context.Background(), XrayApplyRequest{Template: `{"a":1,"b":2}`})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if skipped, _ := res["skipped"].(bool); !skipped {
		t.Fatalf("apply should skip on whitespace-only diff; res=%+v", res)
	}
	if c := count(); c != 0 {
		t.Fatalf("counter must stay at 0; got %d", c)
	}

	got, _ := os.ReadFile(configPath)
	if string(got) != pretty {
		t.Fatalf("config file should be untouched on skip; got %q", string(got))
	}
}

func TestExecXrayApplyAppliesWhenContentDiffers(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	args, count := reloadCounter(t, dir)

	if err := os.WriteFile(configPath, []byte(`{"port":443}`), 0600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	a := &Agent{xrayConfigPath: configPath, xrayReloadArgs: args}
	res, err := a.execXrayApply(context.Background(), XrayApplyRequest{Template: `{"port":8443}`})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if skipped, _ := res["skipped"].(bool); skipped {
		t.Fatalf("apply should not skip when content differs")
	}
	if c := count(); c != 1 {
		t.Fatalf("counter should be 1 after apply; got %d", c)
	}
}

func TestExecXrayApplyAppliesWhenExistingConfigCorrupt(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	args, count := reloadCounter(t, dir)

	// Corrupt JSON on disk: smart-skip's parse fails → must apply.
	if err := os.WriteFile(configPath, []byte(`{not json`), 0600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	a := &Agent{xrayConfigPath: configPath, xrayReloadArgs: args}
	res, err := a.execXrayApply(context.Background(), XrayApplyRequest{Template: `{"port":443}`})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if skipped, _ := res["skipped"].(bool); skipped {
		t.Fatalf("apply should not skip when existing config fails to parse")
	}
	if c := count(); c != 1 {
		t.Fatalf("counter should be 1; got %d", c)
	}
}
