package mcp

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The query-cost estimator's a-priori sources (:explain commit 2): a
// bare-class tally (from the symbol tree, memoized per index generation)
// and O(1) name frequency (from the index). Commit 3 renders these as the
// est column; the planner reorders a chain with them.
func writeEstimatorFixture(t *testing.T) string {
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
	write("go.mod", "module est\ngo 1.21\n")
	write("a.go", `package est

type Alpha struct{}
type Beta struct{}

func one() {}
func two() {}
func three() {}
`)
	return dir
}

func TestClassCountsAccurateAndMemoized(t *testing.T) {
	s := newQueryServer(t, writeEstimatorFixture(t))

	counts := s.classCounts()
	if counts["func"] != 3 {
		t.Errorf("3 funcs in the fixture; classCounts got %d", counts["func"])
	}
	if counts["struct"] != 2 {
		t.Errorf("2 structs; classCounts got %d", counts["struct"])
	}

	// Cross-check against an actual query — the tally must match what the
	// selector would return.
	if n := countSel(t, s, "func"); n != counts["func"] {
		t.Errorf("classCounts[func]=%d disagrees with `func` query=%d", counts["func"], n)
	}

	// Memoized within a generation: the same map INSTANCE comes back, so
	// the expensive full-symbol walk ran once, not twice.
	if reflect.ValueOf(s.classCounts()).Pointer() != reflect.ValueOf(counts).Pointer() {
		t.Error("classCounts must be memoized within a generation (same instance)")
	}
	// A mutation bumps the generation and forces a fresh tally — a
	// different instance, still correct.
	genBefore := s.getIndex().Generation()
	s.getIndex().RemoveFiles([]string{"/does/not/exist"}) // mutates → gen++
	if s.getIndex().Generation() == genBefore {
		t.Fatal("a mutation must bump the index generation")
	}
	fresh := s.classCounts()
	if reflect.ValueOf(fresh).Pointer() == reflect.ValueOf(counts).Pointer() {
		t.Error("a gen bump must invalidate the memo (recompute)")
	}
	if fresh["func"] != 3 {
		t.Errorf("recomputed classCounts must still be correct; got %d", fresh["func"])
	}
}

func TestNameFreqIsCheapSelectivity(t *testing.T) {
	s := newQueryServer(t, writeEstimatorFixture(t))
	idx := s.getIndex()
	// A declared name occurs at least once (its declaration site).
	if idx.NameFreq("Alpha") == 0 {
		t.Error("Alpha is declared; NameFreq must be > 0")
	}
	// A name nobody uses is 0 — the maximal selectivity signal.
	if idx.NameFreq("NoSuchNameZZZ") != 0 {
		t.Error("an absent name has frequency 0")
	}
	// NameFreq is an over-or-equal estimate of the deduped Lookup count
	// (raw tally, no per-position dedup) — never an under-count.
	if idx.NameFreq("one") < len(idx.Lookup("one")) {
		t.Error("NameFreq is a raw tally; it must not undercount Lookup")
	}
}

func countSel(t *testing.T, s *Server, sel string) int {
	t.Helper()
	list, err := parseModernSelector(sel)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.buildTree()
	if err != nil {
		t.Fatal(err)
	}
	return len(e.evaluate(list))
}
