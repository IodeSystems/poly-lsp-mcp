package mcp

import (
	"strings"
	"testing"
)

// The graph half of the selector language: :parents moves (re-rooting
// at reference edges), hop ranges, and the :where/:any/:all/:empty
// set operators. The containment half stays pure CSS and is covered by
// modern_test.go.
//
// The call graph under test:
//
//	A ──▶ B ──▶ C ◀── h (a var, not a func)
//	      Y ◀──▶ X ──▶ C        (X/Y are a real cycle)
//
const graphSrc = `package lib

func C() {}

func B(bArg int) { C() }

func A() { B(1) }

func X(xArg string) { Y(); C() }

func Y() { X() }

var h = C
`

func startGraph(t *testing.T) *mcpSession {
	t.Helper()
	s := startSessionFull(t, goWorkspace(t, graphSrc), nil, nil)
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	return s
}

func wantNodes(t *testing.T, q queryResult, want ...string) {
	t.Helper()
	got := map[string]bool{}
	for _, n := range nodes(q) {
		got[n] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %q; got %v", w, nodes(q))
		}
	}
	if len(got) != len(want) {
		t.Errorf("want exactly %v; got %v", want, nodes(q))
	}
}

func TestParentsSingleHop(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// Default {1,1}: the direct referrers. The inner selector names WHO
	// may stand there — * admits the var, func does not.
	q := query(t, s, map[string]any{"selector": `#'main.go#C':parents(*)`})
	wantNodes(t, q, "main.go#B", "main.go#X", "main.go#h")

	q = query(t, s, map[string]any{"selector": `#'main.go#C':parents(func)`})
	wantNodes(t, q, "main.go#B", "main.go#X")
}

func TestParentsTransitiveClosure(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// {1,} is the fixpoint: every transitive caller, THROUGH funcs
	// only. The X↔Y cycle must terminate, not diverge.
	q := query(t, s, map[string]any{"selector": `#'main.go#C':parents(func){1,}`})
	wantNodes(t, q, "main.go#B", "main.go#X", "main.go#A", "main.go#Y")

	// A node can reach ITSELF through a cycle: X calls Y calls X.
	q = query(t, s, map[string]any{"selector": `#'main.go#X':parents(func){1,}`})
	wantNodes(t, q, "main.go#Y", "main.go#X")
}

func TestParentsExactHopWindow(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// {2,2}: callers-of-callers only.
	q := query(t, s, map[string]any{"selector": `#'main.go#C':parents(func){2,2}`})
	wantNodes(t, q, "main.go#A", "main.go#Y")
}

func TestParentsReRootsMidChain(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// Re-rooting is legal at any point: move to the callers, then the
	// chain continues DOWNWARD from the new tips through containment.
	q := query(t, s, map[string]any{"selector": `#'main.go#C':parents(func) argument`})
	wantNodes(t, q, "main.go#B.bArg", "main.go#X.xArg")
}

func TestAnyParentsIsTheFilterForm(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// "What does A call?" — funcs whose referrers include A. The claim
	// form keeps the tip; the move form would have replaced it.
	q := query(t, s, map[string]any{"selector": `func:any(:parents(#'main.go#A'))`})
	wantNodes(t, q, "main.go#B")

	// :where is the filter spelling of the same connection test.
	q = query(t, s, map[string]any{"selector": `func:where(:parents(#'main.go#A'))`})
	wantNodes(t, q, "main.go#B")
}

func TestEmptyParentsFindsDeadCode(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// ∄ referrers = nothing points here. Only A is uncalled.
	q := query(t, s, map[string]any{"selector": `func:empty(:parents(*))`})
	wantNodes(t, q, "main.go#A")
}

func TestAllQuantifiesOverEveryConnection(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// ∀: everything the structure reaches must match as written. C's
	// referrers include the var h, so "all referrers are funcs" fails
	// for C but holds for B (whose only referrer is A).
	q := query(t, s, map[string]any{"selector": `func:all(:parents(func))`, "limit": 50})
	if hasNode(q, "main.go#C") {
		t.Errorf("C is also referenced by var h — ∀ must fail; got %v", nodes(q))
	}
	if !hasNode(q, "main.go#B") {
		t.Errorf("B's every referrer is a func — ∀ must hold; got %v", nodes(q))
	}

	// Containment ∀: every descendant is an argument. True for X
	// (its only child is xArg), false for main.go (mixed children) —
	// and VACUOUSLY true for go.mod, which has no symbol children at
	// all (∀ over an empty set holds; use :any for existence).
	q = query(t, s, map[string]any{"selector": `func#X:all(argument)`})
	wantNodes(t, q, "main.go#X")
	q = query(t, s, map[string]any{"selector": `file:all(argument)`})
	wantNodes(t, q, "go.mod")
}

func TestRemovedPseudosNameTheirReplacement(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// The three retired pseudos answer with the modern spelling —
	// terse, no grammar dump (same budget as unknownTypeErr).
	for sel, want := range map[string]string{
		`file:has(func)`:           ":any",
		`func:has_parent(#'a.go')`: `#'a.ts' func`,
		`*:references(#'C')`:       ":parents",
	} {
		got := queryErr(t, s, map[string]any{"selector": sel})
		if !strings.Contains(got, want) {
			t.Errorf("%s: error should name %q, got: %s", sel, want, got)
		}
		if strings.Contains(got, "Selector grammar") || len(got) > 500 {
			t.Errorf("%s: error must stay terse (%d chars): %s", sel, len(got), got)
		}
	}
}

func TestParentsHopRangeParseErrors(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	if got := queryErr(t, s, map[string]any{"selector": `#'C':parents(*){0,}`}); !strings.Contains(got, "hops start at 1") {
		t.Errorf("{0,} should be rejected with guidance, got: %s", got)
	}
	if got := queryErr(t, s, map[string]any{"selector": `#'C':parents(*){3,1}`}); !strings.Contains(got, "max must be >= min") {
		t.Errorf("{3,1} should be rejected, got: %s", got)
	}
}
