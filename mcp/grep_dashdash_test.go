package mcp

import (
	"strings"
	"testing"
)

// Searching for a CLI flag name was impossible. `::grep('-E --docker')` reads
// --docker as flags, dies on "unsupported flag -", and there was no escape —
// observed in a dun session 2026-08-05 that was looking for its own --docker
// flag. Every grep spells this `--`, so this does too.
func TestGrepDoubleDashEndsFlags(t *testing.T) {
	t.Run("a dash-leading pattern is searchable after --", func(t *testing.T) {
		g, err := parseFragmentSpec("-E -- --docker")
		if err != nil {
			t.Fatalf("`--` should end the flags: %v", err)
		}
		if g.pattern != "--docker" {
			t.Errorf("pattern = %q, want %q", g.pattern, "--docker")
		}
		if !g.regex {
			t.Error("flags BEFORE the terminator must still apply (-E was dropped)")
		}
	})

	t.Run(":contains too", func(t *testing.T) {
		g, err := parseContainsSpec("-i -- -v")
		if err != nil {
			t.Fatalf("`--` should end the flags: %v", err)
		}
		if g.pattern != "-v" {
			t.Errorf("pattern = %q, want %q — `-v` after the terminator is text, not the invert flag", g.pattern, "-v")
		}
		if g.invert {
			t.Error("-v after `--` is the PATTERN; treating it as invert is the bug")
		}
		if !g.ignoreCase {
			t.Error("-i before the terminator must still apply")
		}
	})

	t.Run("without it, the error names the escape", func(t *testing.T) {
		_, err := parseFragmentSpec("-E --docker")
		if err == nil {
			t.Fatal("expected an error for a dash-leading pattern with no terminator")
		}
		if !strings.Contains(err.Error(), "--") {
			t.Errorf("the error must name the `--` escape, got %q", err)
		}
		// A leading dash is far more often a pattern than a typo, so the
		// message must answer that reading rather than lecture about flags.
		if !strings.Contains(err.Error(), "pattern beginning with") {
			t.Errorf("the error should lead with the pattern reading, got %q", err)
		}
	})

	t.Run("a real bad flag still reports as a bad flag", func(t *testing.T) {
		_, err := parseFragmentSpec("-Z pattern")
		if err == nil || !strings.Contains(err.Error(), "unsupported flag") {
			t.Errorf("a genuine unknown flag must still say so, got %v", err)
		}
	})

	t.Run("a bare -- with nothing after it is an empty pattern, not a crash", func(t *testing.T) {
		if _, err := parseFragmentSpec("--"); err == nil {
			t.Log("empty pattern accepted")
		}
	})
}
