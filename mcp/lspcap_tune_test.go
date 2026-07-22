package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// The cap formula: a floor for a clean workspace, linear in the collision-
// prone tail, ceilinged so cost stays predictable.
func TestTunedLSPCapFormula(t *testing.T) {
	if got := tunedLSPCap(0); got != lspCapFloor {
		t.Errorf("no collisions → floor %d; got %d", lspCapFloor, got)
	}
	if got := tunedLSPCap(10); got != lspCapFloor+lspCapPerName*10 {
		t.Errorf("10 collisions → floor + 10*perName; got %d", got)
	}
	if got := tunedLSPCap(1_000_000); got != lspCapCeil {
		t.Errorf("a huge workspace must be ceilinged at %d; got %d", lspCapCeil, got)
	}
	// Monotonic non-decreasing.
	prev := 0
	for a := 0; a < 2000; a += 37 {
		c := tunedLSPCap(a)
		if c < prev {
			t.Fatalf("cap must not decrease: tunedLSPCap(%d)=%d < %d", a, c, prev)
		}
		prev = c
	}
}

func newTunedEngine(t *testing.T, files map[string]string) *engine {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, _, err := config.LoadOrDefault("nonexistent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	if err := srv.BuildIndex(); err != nil {
		t.Fatal(err)
	}
	e, err := srv.buildTree()
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// The cap is TUNED from the workspace collision rate: two packages each
// declaring Foo and Bar make exactly two collision-prone names, so the cap
// is tunedLSPCap(2) — and it's derived lazily, only once asked.
func TestLSPCapTunedFromCollisions(t *testing.T) {
	e := newTunedEngine(t, map[string]string{
		"go.mod":  "module x\ngo 1.21\n",
		"a/a.go":  "package a\n\nfunc Foo() {}\nfunc Bar() {}\nfunc UniqueA() {}\n",
		"b/b.go":  "package b\n\nfunc Foo() {}\nfunc Bar() {}\nfunc UniqueB() {}\n",
		"main.go": "package main\n\nfunc Solo() {}\n",
	})

	// Not computed until the first round-trip is considered.
	if e.lspCapReady {
		t.Fatal("the cap must be lazy — unset before any edge resolution")
	}
	e.ensureLSPCap()

	if e.collAmbiguous != 2 {
		t.Errorf("Foo and Bar each have 2 decls → 2 collision-prone names; got %d (of %d)",
			e.collAmbiguous, e.collTotal)
	}
	if e.lspLeft != tunedLSPCap(2) || e.lspCapChosen != tunedLSPCap(2) {
		t.Errorf("cap must be tunedLSPCap(2)=%d; got lspLeft=%d chosen=%d",
			tunedLSPCap(2), e.lspLeft, e.lspCapChosen)
	}
	if !e.capTuned {
		t.Error("the cap should be marked workspace-tuned for the legibility note")
	}
}

// An explicit SetLSPResolveCap overrides the tuning entirely.
func TestLSPCapExplicitOverride(t *testing.T) {
	e := newTunedEngine(t, map[string]string{
		"go.mod": "module x\ngo 1.21\n",
		"a/a.go": "package a\n\nfunc Foo() {}\n",
		"b/b.go": "package b\n\nfunc Foo() {}\n",
	})
	e.s.SetLSPResolveCap(7)
	// buildTree already ran; re-derive on this engine.
	e.ensureLSPCap()
	if e.lspLeft != 7 || e.capTuned {
		t.Errorf("explicit cap must win and NOT be marked tuned; got lspLeft=%d tuned=%v", e.lspLeft, e.capTuned)
	}
}
