package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// A transitive edge walk is where the tri-state conf earns its keep: the
// per-query LSP cap is spent shallowest-first, so any unsettled (ambiguous,
// unresolved) edges cluster at the DEEP hops — and only a hop-aware note can
// say "these distant nodes are the least certain."
//
// The fixture is a pure-lexical call chain A→B→C→D, with a SECOND top-level
// D in another package so the C→D edge (hop 3) is ambiguous — no LSP here to
// settle it, so it is unsettled, while A→B and B→C stay name-unique (lexical).
func writeChainFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module chain\ngo 1.21\n")
	write("main.go", `package chain

func A() string { return B() }
func B() string { return C() }
func C() string { return D() }
func D() string { return "d" }
`)
	// A same-named D makes the C→D edge ambiguous; nothing but an LSP could
	// pick between them, and this test runs without one.
	write("other/other.go", `package other

func D() string { return "other-d" }
`)
	return dir
}

func newChainEngine(t *testing.T, dir string) *engine {
	t.Helper()
	cfg, _, err := config.LoadOrDefault("nonexistent.yaml") // defaults, no manager
	if err != nil {
		t.Fatal(err)
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil) // no SetManager — the CLI's shape
	if err := srv.BuildIndex(); err != nil {
		t.Fatal(err)
	}
	e, err := srv.buildTree()
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func evalSel(t *testing.T, e *engine, sel string) []*treeNode {
	t.Helper()
	list, err := parseModernSelector(sel) //nolint
	if err != nil {
		t.Fatal(err)
	}
	return e.evaluate(list)
}

// Single-hop edges carry the tri-state directly: A→B is name-unique
// (lexical), C→D is ambiguous with no LSP (unsettled) — not the same value.
func TestTriStateConfSplitsCertainFromGuess(t *testing.T) {
	dir := writeChainFixture(t)

	unique := evalSel(t, newChainEngine(t, dir), `#'main.go#A'::out.call`)
	if len(unique) == 0 {
		t.Fatal("A calls B — expected an outgoing edge")
	}
	for _, r := range unique {
		if r.refConf != refLexical {
			t.Errorf("A→B is name-unique — want conf %q, got %q", refLexical, r.refConf)
		}
	}

	ambiguous := evalSel(t, newChainEngine(t, dir), `#'main.go#C'::out.call`)
	if len(ambiguous) == 0 {
		t.Fatal("C calls D — expected an outgoing edge")
	}
	for _, r := range ambiguous {
		if r.refConf != refUnsettled {
			t.Errorf("C→D is ambiguous with no LSP — want conf %q (a guess), got %q", refUnsettled, r.refConf)
		}
	}
}

// The transitive walk reaches hop 3 (A→B→C→D); its one unsettled edge sits
// there, and precisionNote must name the hop so the distant guess never
// reads as a resolved reference.
func TestTransitiveNoteReportsUnsettledHop(t *testing.T) {
	e := newChainEngine(t, writeChainFixture(t))
	evalSel(t, e, `#'main.go#A'::out.call{1,}`)

	if e.maxHopReached != 3 {
		t.Errorf("A→B→C→D is a 3-hop walk; maxHopReached=%d", e.maxHopReached)
	}
	if e.unsettledFromHop != 3 {
		t.Errorf("the only ambiguous edge (C→D) is hop 3; unsettledFromHop=%d", e.unsettledFromHop)
	}
	if e.transUnsettled != 1 {
		t.Errorf("exactly one unsettled edge in the walk; transUnsettled=%d", e.transUnsettled)
	}
	note := e.precisionNote()
	if !strings.Contains(note, "hop 3") || !strings.Contains(note, "unsettled") {
		t.Errorf("note must name the unsettled hop; got %q", note)
	}
}

// A transitive walk with no ambiguity says so positively — "all resolved or
// name-unique" — rather than staying silent, so a clean deep walk is
// distinguishable from one that was never checked.
func TestTransitiveNoteCleanWalkSaysSo(t *testing.T) {
	dir := writeChainFixture(t)
	// Bound to 2 hops (A→B, B→C) so the walk never reaches the ambiguous
	// C→D at hop 3 — every edge crossed is name-unique.
	e := newChainEngine(t, dir)
	evalSel(t, e, `#'main.go#A'::out.call{1,2}`)

	if e.maxHopReached != 2 {
		t.Fatalf("bounded to 2 hops; maxHopReached=%d", e.maxHopReached)
	}
	if e.transUnsettled != 0 {
		t.Fatalf("A→B and B→C are both name-unique; transUnsettled=%d", e.transUnsettled)
	}
	note := e.precisionNote()
	if !strings.Contains(note, "2 hops") || !strings.Contains(note, "name-unique") {
		t.Errorf("a clean walk must say so positively; got %q", note)
	}
}
