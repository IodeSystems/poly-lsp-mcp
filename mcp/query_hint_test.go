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
		before := e.probeSnapshot()
		if hit := e.probe(list); hit == nil {
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
		before := e.probeSnapshot()
		if hit := e.probe(list); hit != nil {
			t.Fatalf("a probe out of budget must report nothing, got %s", hit.addr())
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
		if hit := e.probe(list); hit != nil {
			t.Errorf("a spent budget must buy no probe, got %s", hit.addr())
		}
	})
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

// probeSafe is what keeps a hint from costing more than the query it explains.
func TestProbeSafeExcludesWhatAProbeMustNotRerun(t *testing.T) {
	cases := []struct {
		selector string
		safe     bool
	}{
		{"func name=Start", true},
		{"file path=main.go func", true},
		{":root > *", true},
		{"func:contains('return')", true},
		{"#Save::in.call", false},         // an edge probe spends child-LSP round-trips
		{"func::grep('x')", false},        // a generated element filters nothing
		{"func::comment", false},          // ditto
		{"func::signature", false},        // ditto
		{"func:recursive", false},         // asks a child LSP
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
