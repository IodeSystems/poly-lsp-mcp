package mcp

import (
	"reflect"
	"testing"
)

// The probe reuses the LIVE engine — a fresh one would re-walk the workspace
// and re-parse every file it touched, which is the opposite of cheap. That
// makes invisibility a property to PROVE, not to assume: every field the
// evaluator writes and the payload later reads has to come back unchanged, or
// a query that finished fine reports itself truncated because the hint that
// explained it ran out of budget.
//
// Comparing the whole probeState (not a hand-picked field or two) is the
// point: the assertion keeps holding when the struct grows.
func TestProbeLeavesEngineUntouched(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	list, err := parseModernSelector("*[name=Start]")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a probe that finds something", func(t *testing.T) {
		e, err := s.srv.buildTree()
		if err != nil {
			t.Fatal(err)
		}
		e.openHintBudget()
		before := e.probeSnapshot()
		if hit, ok := e.probe(list); hit == nil || !ok {
			t.Fatal("expected the probe to find Server.Start")
		}
		if after := e.probeSnapshot(); !reflect.DeepEqual(before, after) {
			t.Errorf("probe changed engine state:\n before %+v\n after  %+v", before, after)
		}
	})

	// The case the fixture would otherwise never reach: a probe that BLOWS
	// its allowance. Left unrestored, workExceeded is what makes the caller's
	// own complete result claim to be truncated.
	t.Run("a probe that runs out", func(t *testing.T) {
		e, err := s.srv.buildTree()
		if err != nil {
			t.Fatal(err)
		}
		e.workLeft = 3 // caps the probe at 3 ops — nothing completes in that
		e.openHintBudget()
		before := e.probeSnapshot()
		if hit, ok := e.probe(list); hit != nil || ok {
			t.Fatalf("a probe out of budget must be INCONCLUSIVE, got hit=%v ok=%v", hit, ok)
		}
		if after := e.probeSnapshot(); !reflect.DeepEqual(before, after) {
			t.Errorf("an exhausted probe leaked:\n before %+v\n after  %+v", before, after)
		}
	})

	// A caller who asked for a cheap query does not get an expensive
	// explanation: with the budget already spent there is no probe at all.
	t.Run("no budget left, no probe", func(t *testing.T) {
		e, err := s.srv.buildTree()
		if err != nil {
			t.Fatal(err)
		}
		e.workLeft = 0
		e.openHintBudget()
		if hit, ok := e.probe(list); hit != nil || ok {
			t.Errorf("a spent budget must buy no probe, got hit=%v ok=%v", hit, ok)
		}
	})
}

// One allowance for the whole hint, and every probe that touches the tree
// draws on it.
//
// hintNearName is the reason this is a test and not a comment: it used to
// verify its candidate with declsNamed, which parses every file the name
// occurs in and charges nothing — 67 ms on this repo against a 10 ms
// allowance, on a query that had answered in microseconds. A probe that
// spends outside the budget makes the budget decorative.
func TestHintBudgetIsSharedAndSpent(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// The name must be a NEAR MISS (Stat -> Start), or hintNearName bails
	// before its verification step and the exhausted-budget check below
	// passes for the wrong reason.
	list, err := parseModernSelector("interface method name=Stat")
	if err != nil {
		t.Fatal(err)
	}
	cx := list[0]

	e, err := s.srv.buildTree()
	if err != nil {
		t.Fatal(err)
	}
	e.openHintBudget()
	start := e.hintOpsLeft
	if start <= 0 {
		t.Fatalf("openHintBudget gave nothing to spend: %d", start)
	}
	e.hintDropLastAttr(cx)
	afterOne := e.hintOpsLeft
	if afterOne >= start || afterOne < 0 {
		t.Errorf("a probe should draw down the shared allowance: %d -> %d", start, afterOne)
	}
	e.hintDropTag(cx)
	if e.hintOpsLeft > afterOne {
		t.Errorf("the allowance must not refill between probes: %d -> %d", afterOne, e.hintOpsLeft)
	}

	// Exhausted: every probe that reads the tree goes quiet. hintDeadPath is
	// deliberately absent — it answers from the dir/file nodes the walk
	// already built and parses nothing, so it costs nothing to run.
	e.hintOpsLeft = 0
	for name, probe := range map[string]func(selComplex) string{
		"hintDeadPrefix":   e.hintDeadPrefix,
		"hintDropTag":      e.hintDropTag,
		"hintNearName":     e.hintNearName,
		"hintDropLastAttr": e.hintDropLastAttr,
	} {
		if got := probe(cx); got != "" {
			t.Errorf("%s spent outside the allowance: %q", name, got)
		}
	}
}

// cloneComplex has one job: let a probe rewrite a tag or an attribute without
// touching the selector the caller wrote. A shared compound would corrupt the
// cost trace and, worse, the :explain rendering of the caller's own query.
func TestCloneComplexDoesNotAliasTheCallersSelector(t *testing.T) {
	list, err := parseModernSelector("func path=main.go name^=S")
	if err != nil {
		t.Fatal(err)
	}
	orig := list[0]
	relaxed := cloneComplex(orig)

	c := relaxed.elems[len(relaxed.elems)-1].comp
	c.class, c.anyType = "", true
	c.attrs = c.attrs[:len(c.attrs)-1]

	sub := orig.elems[len(orig.elems)-1].comp
	if sub.class != "func" || sub.anyType {
		t.Errorf("the caller's tag was rewritten: class=%q anyType=%v", sub.class, sub.anyType)
	}
	if len(sub.attrs) != 2 {
		t.Errorf("the caller's attrs were truncated: %d, want 2", len(sub.attrs))
	}
}

// probeSafe is now a purely STRUCTURAL gate. Edges and generated elements
// used to be excluded here as a proxy for "do not spend child-LSP
// round-trips"; that is enforced at the source instead (probeBlocked), which
// lets an edge chain whose refs are already materialized probe for free.
func TestProbeSafeExcludesOnlyGroups(t *testing.T) {
	cases := []struct {
		selector string
		safe     bool
	}{
		{"func name=Start", true},
		{"file path=main.go func", true},
		{":root > *", true},
		{"func:contains('return')", true},
		{"#Save::in.call", true},          // an edge chain: gated dynamically, not here
		{"func::grep('x')", true},         // ditto
		{"func::comment", true},           // ditto
		{"func::signature", true},         // ditto
		{"func:recursive", true},          // ditto — degrades to inconclusive, not to a guess
		{"(func > argument){1,2}", false}, // a group has no single subject compound
	}
	for _, c := range cases {
		list, err := parseModernSelector(c.selector)
		if err != nil {
			t.Fatalf("%s: %v", c.selector, err)
		}
		if got := probeSafe(list[0]); got != c.safe {
			t.Errorf("probeSafe(%s) = %v, want %v", c.selector, got, c.safe)
		}
	}
}

// The invariant that replaced the exclusion list: a probe never performs a
// child-LSP round-trip and never builds an edge set the caller's own query
// did not already build. When it would have to, the probe is INCONCLUSIVE —
// its result is discarded, not reported at lower confidence, because a hint
// naming a node the caller's retry won't return is worse than silence.
func TestProbeNeverDoesNewEdgeWork(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	list, err := parseModernSelector("#'main.go#CallsStart'::out > interface")
	if err != nil {
		t.Fatal(err)
	}

	// Cold engine: nothing has materialized CallsStart's out-edges, so the
	// probe must refuse and say so.
	cold, err := s.srv.buildTree()
	if err != nil {
		t.Fatal(err)
	}
	cold.openHintBudget()
	if hit, ok := cold.probe(list); ok || hit != nil {
		t.Errorf("a cold edge probe must be inconclusive, got hit=%v ok=%v", hit, ok)
	}
	if cold.lspAsked != 0 {
		t.Errorf("a probe asked a child LSP %d time(s)", cold.lspAsked)
	}

	// Warm engine: the caller's own query built those edges, so re-running a
	// relaxed form of it is free and as resolved as the answer it explains.
	warm, err := s.srv.buildTree()
	if err != nil {
		t.Fatal(err)
	}
	warm.evaluate(list) // the caller's query — empty, but it materialized the edges
	relaxed, err := parseModernSelector("#'main.go#CallsStart'::out > *")
	if err != nil {
		t.Fatal(err)
	}
	warm.openHintBudget()
	hit, ok := warm.probe(relaxed)
	if !ok {
		t.Fatal("a warm edge probe should be conclusive: the edges are already built")
	}
	// CallsStart references Server (a type edge) then s.Start (a call edge);
	// need=1 stops at whichever comes first in document order.
	if hit == nil || hit.file != "main.go" {
		t.Errorf("warm probe should reach a far end in main.go, got %v", hit)
	}
	if warm.lspAsked != 0 {
		t.Errorf("a probe asked a child LSP %d time(s)", warm.lspAsked)
	}
}
