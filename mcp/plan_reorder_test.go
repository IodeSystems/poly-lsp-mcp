package mcp

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The descendant-chain reorder seeds a `broad-leading … rare-tip` chain
// from the RARE end: an exact-name tip is resolved from the INDEX (only
// files containing the name are loaded), then filtered by the ancestor
// chain — the containment form of the leading-ref pushdown. It must
// return EXACTLY the forward evaluation's nodes and cost less on the
// shape it targets.
func writeReorderFixture(t *testing.T) string {
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
	write("go.mod", "module rc\ngo 1.21\n")
	// One file holds the rare tip name; many others are filler the seed
	// must NOT need to load.
	write("hot.go", `package rc

func Handler(needle int) {}
func Worker(needle int) {}
`)
	// Many filler files with DISTINCT names — raises the index's distinct-
	// name count (the broad-proxy the gate compares the rare tip against)
	// above the reorder threshold, as a real workspace's would be.
	for i := 0; i < 30; i++ {
		c := string(rune('a' + i%26))
		write("filler"+c+string(rune('0'+i/26))+".go",
			"package rc\n\nfunc Zz"+c+string(rune('0'+i/26))+"() { _ = 1 }\n")
	}
	return dir
}

func reorderEval(t *testing.T, s *Server, sel string, noPlan bool) ([]string, int) {
	t.Helper()
	list, err := parseModernSelector(sel)
	if err != nil {
		t.Fatalf("%s: %v", sel, err)
	}
	e, err := s.buildTree()
	if err != nil {
		t.Fatal(err)
	}
	e.noPlan = noPlan
	before := e.workLeft
	rows := e.evaluate(list)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.addr())
	}
	sort.Strings(out)
	return out, before - e.workLeft
}

func TestReorderEquivalentAndCheaper(t *testing.T) {
	s := newQueryServer(t, writeReorderFixture(t))
	s.SetQueryWorkBudget(50_000_000)

	// Chains the reorder targets (broad class ⊃ rare name) and control
	// chains it must leave alone.
	for _, sel := range []string{
		`func #needle`,       // fires: broad func ⊃ rare needle
		`func #Handler`,      // fires: the func itself, name-anchored
		`func #Nonexistent`, // fires (empty), must stay empty
		`func argument`,     // control: tip is a bare class, not a name
		`#'hot.go' #needle`, // control: leading elem already rare
	} {
		fwd, fc := reorderEval(t, s, sel, true)   // forward
		plan, pc := reorderEval(t, s, sel, false) // reordered
		if len(fwd) != len(plan) {
			t.Errorf("%s: count differs forward=%d reorder=%d", sel, len(fwd), len(plan))
			continue
		}
		for i := range fwd {
			if fwd[i] != plan[i] {
				t.Errorf("%s: row %d differs\n forward=%s\n reorder=%s", sel, i, fwd[i], plan[i])
				break
			}
		}
		if pc > fc {
			t.Errorf("%s: reorder must not cost MORE: forward=%d reorder=%d", sel, fc, pc)
		}
	}

	// The targeted shape must actually be CHEAPER — else the reorder is a
	// no-op and the whole thing is dead code (the trap the first cut fell
	// into: collectMatches walked the tree regardless).
	_, fc := reorderEval(t, s, `func #needle`, true)
	_, pc := reorderEval(t, s, `func #needle`, false)
	if pc >= fc {
		t.Errorf("func #needle reorder gave no saving: forward=%d reorder=%d", fc, pc)
	}

	// Correctness on the value: needle is a param of Handler and Worker,
	// nothing else.
	got, _ := reorderEval(t, s, `func #needle`, false)
	want := []string{"hot.go#Handler.needle", "hot.go#Worker.needle"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("func #needle = %v, want %v", got, want)
	}
}

// A 3-element chain exercises the ancestor SUBSEQUENCE (not just a single
// parent): file ⊃ func ⊃ #needle must keep only needles under a func
// under a file.
func TestReorderAncestorSubsequence(t *testing.T) {
	s := newQueryServer(t, writeReorderFixture(t))
	fwd, _ := reorderEval(t, s, `file func #needle`, true)
	plan, _ := reorderEval(t, s, `file func #needle`, false)
	if len(fwd) != len(plan) {
		t.Fatalf("subsequence chain differs: forward=%d reorder=%d", len(fwd), len(plan))
	}
	for i := range fwd {
		if fwd[i] != plan[i] {
			t.Errorf("row %d: forward=%s reorder=%s", i, fwd[i], plan[i])
		}
	}
	if len(plan) != 2 {
		t.Errorf("both needles are under a func under a file; got %d", len(plan))
	}
}
