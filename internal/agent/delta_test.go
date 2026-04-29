package agent

import "testing"

func TestDeltaValueGrowth(t *testing.T) {
	delta, rebase := deltaValue(100, 150)
	if delta != 50 {
		t.Fatalf("delta=%d, want 50", delta)
	}
	if rebase != 150 {
		t.Fatalf("rebase=%d, want 150", rebase)
	}
}

func TestDeltaValueStable(t *testing.T) {
	delta, rebase := deltaValue(100, 100)
	if delta != 0 {
		t.Fatalf("delta=%d, want 0", delta)
	}
	if rebase != 100 {
		t.Fatalf("rebase=%d, want 100", rebase)
	}
}

func TestDeltaValueReset(t *testing.T) {
	delta, rebase := deltaValue(500, 120)
	if delta != 120 {
		t.Fatalf("on reset, delta=%d, want 120", delta)
	}
	if rebase != 120 {
		t.Fatalf("on reset, rebase=%d, want 120 (current)", rebase)
	}
}

func TestDeltaValueNegative(t *testing.T) {
	delta, rebase := deltaValue(100, -3)
	if delta != 0 || rebase != 0 {
		t.Fatalf("negative current: delta=%d rebase=%d, want 0/0", delta, rebase)
	}
}
