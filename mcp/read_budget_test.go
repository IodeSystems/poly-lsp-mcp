package mcp

import "testing"

// TestReadCharBudgetOverride pins the tunable. The default must stay 2048 so
// existing callers are unaffected, and the env override must take effect —
// this is the lever for trading round-trips (generated tokens, expensive)
// against payload size (processed tokens, cheap).
func TestReadCharBudgetOverride(t *testing.T) {
	if defaultReadCharBudgetFallback != 2048 {
		t.Fatalf("default fallback = %d, want 2048 (changing it silently changes every caller)",
			defaultReadCharBudgetFallback)
	}
	t.Setenv("POLY_LSP_READ_CHAR_BUDGET", "16384")
	if got := readCharBudgetFromEnv(); got != 16384 {
		t.Fatalf("override = %d, want 16384", got)
	}
	for _, bad := range []string{"", "0", "-5", "notanumber"} {
		t.Setenv("POLY_LSP_READ_CHAR_BUDGET", bad)
		if got := readCharBudgetFromEnv(); got != defaultReadCharBudgetFallback {
			t.Fatalf("%q gave %d, want the fallback %d", bad, got, defaultReadCharBudgetFallback)
		}
	}
}

// TestSetReadCharBudget covers the --read-char-budget flag path: it wins over
// the environment, and the flag's unset value (0) must NOT clobber whatever
// was already resolved.
func TestSetReadCharBudget(t *testing.T) {
	orig := defaultReadCharBudget
	defer func() { defaultReadCharBudget = orig }()

	defaultReadCharBudget = defaultReadCharBudgetFallback
	SetReadCharBudget(16384)
	if defaultReadCharBudget != 16384 {
		t.Fatalf("after flag: %d, want 16384", defaultReadCharBudget)
	}
	for _, noop := range []int{0, -1} {
		SetReadCharBudget(noop)
		if defaultReadCharBudget != 16384 {
			t.Fatalf("SetReadCharBudget(%d) clobbered the budget to %d", noop, defaultReadCharBudget)
		}
	}
}
