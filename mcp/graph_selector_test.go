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

func TestReferencesIsTheOutgoingMove(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// "What does A call?" — the outgoing edge, one move, no inversion.
	q := query(t, s, map[string]any{"selector": `#'main.go#A':references(func)`})
	wantNodes(t, q, "main.go#B")

	// Unfiltered outgoing from X: the funcs it calls (Y, C). xArg is
	// declared but never used, so it is NOT referenced.
	q = query(t, s, map[string]any{"selector": `#'main.go#X':references(*)`})
	wantNodes(t, q, "main.go#Y", "main.go#C")

	// A var's initializer references too.
	q = query(t, s, map[string]any{"selector": `#'main.go#h':references(*)`})
	wantNodes(t, q, "main.go#C")

	// Transitive: everything A reaches.
	q = query(t, s, map[string]any{"selector": `#'main.go#A':references(func){1,}`})
	wantNodes(t, q, "main.go#B", "main.go#C")
}

func TestBareClaimsCloseTheMove(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// Dead code, the postfix spelling: nothing points at A.
	q := query(t, s, map[string]any{"selector": `func:parents:empty`})
	wantNodes(t, q, "main.go#A")

	// Leaf funcs: C calls no other function.
	q = query(t, s, map[string]any{"selector": `func:references(func):empty`})
	wantNodes(t, q, "main.go#C")

	// :any is the complement: funcs WITH callers.
	q = query(t, s, map[string]any{"selector": `func:parents:any`})
	wantNodes(t, q, "main.go#C", "main.go#B", "main.go#X", "main.go#Y")

	// The bare form is sugar for the canonical :where(&…): identical
	// result sets, with or without the explicit &.
	for _, sel := range []string{
		`func:where(&:references(func):empty)`,
		`func:where(:references(func):empty)`,
	} {
		q = query(t, s, map[string]any{"selector": sel})
		wantNodes(t, q, "main.go#C")
	}
}

func TestBareAllComparesAgainstTheUnfilteredWalk(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// ∀: the written move reaches everything the unfiltered move would.
	// C fails (var h also points at it); A holds vacuously (no
	// referrers at all); B/X/Y hold (their referrers are all funcs).
	q := query(t, s, map[string]any{"selector": `func:parents(func):all`, "limit": 50})
	wantNodes(t, q, "main.go#A", "main.go#B", "main.go#X", "main.go#Y")

	// Identical to the parenthesized spelling.
	q = query(t, s, map[string]any{"selector": `func:all(:parents(func))`, "limit": 50})
	wantNodes(t, q, "main.go#A", "main.go#B", "main.go#X", "main.go#Y")
}

func TestAnyParentsIsTheFilterForm(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// The relative inner starts at & implicitly (CSS nesting rule), so
	// a leading pseudo attaches to the node under test.
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

	// The retired pseudos answer with the modern spelling — terse, no
	// grammar dump (same budget as unknownTypeErr).
	for sel, want := range map[string]string{
		`file:has(func)`:           ":any",
		`func:has_parent(#'a.go')`: `#'a.ts' func`,
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

func TestSelfRefAndClaimParseRules(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// A bare claim needs an open move — nothing to test otherwise.
	if got := queryErr(t, s, map[string]any{"selector": `func:empty`}); !strings.Contains(got, "func:parents:empty") {
		t.Errorf("bare :empty without a move should guide, got: %s", got)
	}
	// A claim CLOSES the move: a second bare claim needs a new one.
	if got := queryErr(t, s, map[string]any{"selector": `func:parents:empty:any`}); !strings.Contains(got, "needs a set") {
		t.Errorf("claim after a closed move should guide, got: %s", got)
	}
	// & names the node under test — meaningless outside a relative list.
	if got := queryErr(t, s, map[string]any{"selector": `&:parents:empty`}); !strings.Contains(got, ":where") {
		t.Errorf("top-level & should point at :where, got: %s", got)
	}
	if got := queryErr(t, s, map[string]any{"selector": `func:where(> &)`}); !strings.Contains(got, "contradicts") {
		t.Errorf("'> &' should be rejected, got: %s", got)
	}
}

func TestCompoundBraceRangeIsCanonicalDepth(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// {m,n} on a compound ≡ :depth(m,n): distance from the previous
	// target. {0} = that target itself — the self-reference trick.
	q := query(t, s, map[string]any{"selector": `#'main.go' *{0}`})
	wantNodes(t, q, "main.go")

	// Equivalent spellings, byte for byte the same result.
	a := query(t, s, map[string]any{"selector": `*{0,1}`, "limit": 50})
	b := query(t, s, map[string]any{"selector": `*:depth(0,1)`, "limit": 50})
	if len(nodes(a)) == 0 || strings.Join(nodes(a), "|") != strings.Join(nodes(b), "|") {
		t.Errorf("{0,1} and :depth(0,1) must agree; got %v vs %v", nodes(a), nodes(b))
	}

	// {1} ≡ '>' : direct children only.
	q = query(t, s, map[string]any{"selector": `#'main.go' argument{2}`, "limit": 50})
	wantNodes(t, q, "main.go#B.bArg", "main.go#X.xArg")
	q = query(t, s, map[string]any{"selector": `#'main.go' argument{1}`})
	wantNodes(t, q)

	// One range per compound, either spelling.
	if got := queryErr(t, s, map[string]any{"selector": `func{1}:depth(1,1)`}); !strings.Contains(got, "one depth range") && !strings.Contains(got, "one :depth") {
		t.Errorf("duplicate range should error, got: %s", got)
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
