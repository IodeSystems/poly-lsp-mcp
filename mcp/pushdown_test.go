package mcp

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// newQueryServer builds an indexed, manager-less server over a workspace
// — the `query` CLI's read-only shape, enough to evaluate selectors.
func newQueryServer(t *testing.T, root string) *Server {
	t.Helper()
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	s := New(reg, root, nil, nil)
	if err := s.BuildIndex(); err != nil {
		t.Fatal(err)
	}
	return s
}

// A workspace with a cross-package name collision (two `Target`s) and
// filler funcs, so a leading-ref filter has FEW candidate hosts among
// MANY symbols — the shape where the pushdown must both match the full
// scan and cost less.
func writePushdownFixture(t *testing.T) string {
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
	write("go.mod", "module pd\ngo 1.21\n")
	write("a/a.go", `package a

func Target() {}

func CallsA() { Target() }
`)
	write("b/b.go", `package b

func Target() {}

func CallsB() { Target() }
`)
	// Filler: many hosts the universal scan must visit but the pushdown
	// need not, none of which reference Target.
	filler := "package main\n\nfunc main() {}\n"
	for i := 0; i < 12; i++ {
		filler += "\nfunc Filler" + string(rune('A'+i)) + "() {}\n"
	}
	write("main.go", filler)
	return dir
}

func evalNodes(t *testing.T, s *Server, sel string, noPush bool) ([]string, int) {
	t.Helper()
	list, err := parseModernSelector(sel)
	if err != nil {
		t.Fatalf("%s: %v", sel, err)
	}
	e, err := s.buildTree()
	if err != nil {
		t.Fatal(err)
	}
	e.noPushdown = noPush
	before := e.workLeft
	rows := e.evaluate(list)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		key := r.addr() + "|" + r.refDir + "|" + r.refKind
		for _, f := range r.refFar {
			key += "|" + f.addr()
		}
		out = append(out, key)
	}
	sort.Strings(out)
	return out, before - e.workLeft
}

// The leading-ref cardinality pushdown must return EXACTLY the full
// scan's nodes — the fast path only skips hosts that provably cannot
// match — and must cost less on the shape it targets.
func TestLeadingRefPushdownEquivalentAndCheaper(t *testing.T) {
	s := newQueryServer(t, writePushdownFixture(t))

	sels := []string{
		`::in.call#'Target'`,
		`::out.call#'Target'`,
		`::in#'Target'`,
		`::out#'Target'`,
		`::in.call[name=Target]`,
		`::in.call#'Target' > func`,
		`::in.call#'Nonexistent'`, // empty, and must stay empty
	}
	for _, sel := range sels {
		full, fullCost := evalNodes(t, s, sel, true)
		push, pushCost := evalNodes(t, s, sel, false)
		if len(full) != len(push) {
			t.Errorf("%s: count differs full=%d push=%d", sel, len(full), len(push))
			continue
		}
		for i := range full {
			if full[i] != push[i] {
				t.Errorf("%s: row %d differs\n full=%s\n push=%s", sel, i, full[i], push[i])
				break
			}
		}
		if pushCost > fullCost {
			t.Errorf("%s: pushdown must not cost MORE: full=%d push=%d", sel, fullCost, pushCost)
		}
	}

	// The pathological form specifically: a filter-form leading ref over
	// a workspace of many unrelated hosts must not pay for all of them.
	_, fullCost := evalNodes(t, s, `::in.call#'Target'`, true)
	_, pushCost := evalNodes(t, s, `::in.call#'Target'`, false)
	if pushCost >= fullCost {
		t.Errorf("::in.call#'Target' pushdown gave no saving: full=%d push=%d", fullCost, pushCost)
	}
}

// The anchored and non-universal forms must be UNAFFECTED — the pushdown
// only fires for a global implied-universal leading ref.
func TestPushdownLeavesAnchoredFormsUntouched(t *testing.T) {
	s := newQueryServer(t, writePushdownFixture(t))
	for _, sel := range []string{`#'Target'::in.call`, `func::in.call#'Target'`} {
		a, _ := evalNodes(t, s, sel, true)
		b, _ := evalNodes(t, s, sel, false)
		if len(a) != len(b) {
			t.Errorf("%s: pushdown changed a non-universal form: %d vs %d", sel, len(a), len(b))
		}
	}
}

// module main and func main share the sym path "main". An edge inside
// func main must be attributed to it ALONE — a name-only match hands the
// call site to both and double-counts the edge.
func TestSameNamedSymbolsDoNotDoubleCountEdges(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module mc\ngo 1.21\n")
	// package main => a `module main` node; func main => a `func main`
	// node; both answer to sym "main". The call sits in the func.
	write("main.go", `package main

func helper() {}

func main() {
	helper()
}
`)
	s := newQueryServer(t, dir)
	// Full scan (noPushdown) exercises buildOutRefs, which attributes a
	// call site to a host. Without containment, both `module main` and
	// `func main` (both sym "main") claim the line-6 call to helper and
	// the edge is emitted twice. The pushdown path resolves via
	// nodeByAddr (containment-correct) and would hide the bug, so this
	// must test the full scan.
	got, _ := evalNodes(t, s, `::out.call#'helper'`, true)
	if len(got) != 1 {
		t.Fatalf("helper is called once, from func main; got %d edges: %v", len(got), got)
	}
}
