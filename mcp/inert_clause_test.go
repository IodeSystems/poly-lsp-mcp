package mcp

import (
	"strings"
	"testing"
)

// hintFor runs a selector and returns (totalMatches, hint).
func hintFor(t *testing.T, s *mcpSession, sel string) (int, string) {
	t.Helper()
	q := query(t, s, map[string]any{"selector": sel, "limit": 500})
	return q.TotalMatches, q.Hint
}

// `path=a.go path=b.go` reads like "things in b under things in a" and is
// neither: a bare attribute after a space FILTERS the element before it, so
// both paths land on ONE compound and the selector asks for a node living in
// two files. It answers 0 and the old hint blamed the last filter, which is
// true but does not explain why no filter could have worked.
func TestInertNote_TwoPathsOnOneElement(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	n, hint := hintFor(t, s, "path=main.go path=notes.md")
	if n != 0 {
		t.Fatalf("one node cannot live in two files; got %d matches", n)
	}
	for _, want := range []string{"SAME element", "two files", "::out.call"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint should explain the shape and offer the graph form (%q missing): %q", want, hint)
		}
	}
}

// The dangerous one. `{0,…}` lets a clause VANISH, so a selector whose tail
// can never match still answers — with the prefix alone, at full confidence.
// This is the shape that returned 129 rows on the real workspace while the
// correct answer to the question being asked was zero.
func TestInertNote_VanishingClauseStillReported(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	bare, _ := hintFor(t, s, "path=main.go")
	n, hint := hintFor(t, s, "path=main.go *{0,3} path=notes.md")
	if n != bare {
		t.Fatalf("the {0,…} clause vanishes, so this is just the prefix: got %d, prefix alone is %d", n, bare)
	}
	if n == 0 {
		t.Fatal("this test is only meaningful when the result is NON-empty")
	}
	if !strings.Contains(hint, "can never match") {
		t.Errorf("a clause that cannot contribute must say so even at %d matches: %q", n, hint)
	}
	if !strings.Contains(hint, "SKIP path") {
		t.Errorf("and must say the answer is only the part before it: %q", hint)
	}
	if !strings.Contains(hint, "::out.call") {
		t.Errorf("and hand back the reference-graph form: %q", hint)
	}
}

// The check must stay quiet on selectors that are fine, or it becomes noise
// that trains callers to ignore hints. A directory above a file DOES contain
// it, and a tag after a path is the ordinary descendant reading.
func TestInertNote_QuietOnValidSelectors(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	for _, sel := range []string{
		"path=main.go func",         // the normal descendant form
		"path=main.go",              // one constraint
		"path=web *",                // a directory
		"path=main.go path=main.go", // same path twice: a symbol shares its file's path
	} {
		n, hint := hintFor(t, s, sel)
		if strings.Contains(hint, "can never match") || strings.Contains(hint, "SAME element") {
			t.Errorf("%q (%d matches) is satisfiable and must not be flagged: %q", sel, n, hint)
		}
	}
}

// A result reached by CROSSING an edge keeps the edge's facts. `::out.call`
// reports the call site and a conf of lsp|lexical|unsettled; `::out.call > *`
// used to report neither, so the walk's own results could not be told apart
// from guesses — while the payload's edges note still promised "each edge's
// conf says which".
func TestCrossedEdgeKeepsSiteAndConfidence(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	q := query(t, s, map[string]any{
		"selector": "path=main.go ::out.call > *", "limit": 10,
	})
	if len(q.Matches) == 0 {
		t.Skip("no resolvable call edges in the fixture")
	}
	for _, m := range q.Matches {
		if m.Conf == "" {
			t.Errorf("a crossed row must carry the edge's confidence; got %+v", m)
		}
		if m.Via == "" {
			t.Errorf("a crossed row must name the site it was reached through; got %+v", m)
		}
		if !strings.Contains(m.Via, "@") {
			t.Errorf("via should be a file@line site address; got %q", m.Via)
		}
	}
}

// A multi-hop walk reports DISTANCE. Without it "reachable" cannot separate a
// direct call from a four-hop chain, which is most of what a {1,n} walk is
// asked for. The walk is a level-synchronous BFS, so first-reached is
// fewest-hops and the number needs no extra search.
func TestMultiHopReportsDistance(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	q := query(t, s, map[string]any{
		"selector": "path=main.go ::out.call{1,3} > *", "limit": 30,
	})
	if len(q.Matches) == 0 {
		t.Skip("no resolvable call edges in the fixture")
	}
	seen := map[int]bool{}
	for _, m := range q.Matches {
		if m.Hop < 1 {
			t.Errorf("every row of a repeated walk should carry its hop; got %+v", m)
			continue
		}
		if m.Hop > 3 {
			t.Errorf("hop %d exceeds the {1,3} bound: %+v", m.Hop, m)
		}
		seen[m.Hop] = true
	}
	if !seen[1] {
		t.Error("a walk starting at hop 1 must report some hop-1 results")
	}

	// A single hop is trivially 1; saying so would be noise.
	q1 := query(t, s, map[string]any{"selector": "path=main.go ::out.call > *", "limit": 5})
	for _, m := range q1.Matches {
		if m.Hop != 0 {
			t.Errorf("a single-hop query should not label distance; got hop=%d", m.Hop)
		}
	}
}

// The subject line names which end of a chain comes back. baseClause drops
// attributes, so two filtered wildcards both rendered `*` and the sentence
// became "returns the `*` nodes — NOT the `*` nodes" — naming nothing, on
// exactly the cross-file selectors where it matters most.
func TestSubjectLineDistinguishesIdenticalBases(t *testing.T) {
	list, err := parseModernSelector("*[path=a.go] ::out.call > *[path=b.go]")
	if err != nil {
		t.Fatal(err)
	}
	got := subjectLine(list)
	if got == "" {
		t.Fatal("a multi-element chain should state its subject")
	}
	if strings.Contains(got, "`*` nodes — NOT the `*` nodes") {
		t.Fatalf("the two ends must be distinguishable: %q", got)
	}
	if !strings.Contains(got, "b.go") || !strings.Contains(got, "a.go") {
		t.Errorf("both ends should be named by what distinguishes them: %q", got)
	}
}
