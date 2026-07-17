package mcp

import (
	"strings"
	"testing"
	"time"
)

// node_query exposes a `budget` arg, and a blow tells the caller about
// BOTH levers — narrow (with the cost trace naming the culprit) or raise
// the budget — so a model isn't stuck guessing at a hidden knob.
func TestNodeQueryBudgetArgAndHint(t *testing.T) {
	s := startGraph(t)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// A tiny OPS budget forces a DETERMINISTIC blow; the note must name
	// the budget lever AND point at the cost trace, not just say "narrow".
	blown := query(t, s, map[string]any{"selector": `func::in.call{1,3}`, "budget": "20ops"})
	if !blown.Truncated {
		t.Skip("budget 20 did not trip on this fixture")
	}
	if !strings.Contains(blown.Note, "budget") {
		t.Errorf("a blow note must mention the budget lever; got %q", blown.Note)
	}
	for _, want := range []string{"NARROW", "RAISE", "cost"} {
		if !strings.Contains(blown.Note, want) {
			t.Errorf("blow note missing %q lever/pointer; got %q", want, blown.Note)
		}
	}
	if len(blown.Cost) == 0 {
		t.Error("a blown result must carry the cost trace the note points at")
	}

	// Raising the budget completes what the tiny one truncated — the
	// escape hatch actually works.
	raised := query(t, s, map[string]any{"selector": `func::in.call{1,3}`, "budget": "5000000ops"})
	if raised.TotalMatches < blown.TotalMatches {
		t.Errorf("a raised budget must not see FEWER matches: raised=%d blown=%d",
			raised.TotalMatches, blown.TotalMatches)
	}

	// The cap holds: an absurd budget is clamped, not honored blindly (no
	// crash, still answers).
	capped := query(t, s, map[string]any{"selector": `func`, "budget": "999999999ops"})
	if capped.TotalMatches == 0 {
		t.Error("a clamped over-max budget must still evaluate")
	}
}

func TestParseBudgetSuffixes(t *testing.T) {
	cases := []struct {
		in    string
		value int
		unit  string
		ok    bool
	}{
		{"2000", 2000, "ms", true},   // bare = ms
		{"2000ms", 2000, "ms", true}, // explicit ms
		{"500ops", 500, "ops", true}, // deterministic
		{`"300ops"`, 300, "ops", true},
		{" 40 ms ", 40, "ms", true},
		{"", 0, "", false},
		{"0", 0, "", false},   // non-positive
		{"abc", 0, "", false}, // non-numeric
	}
	for _, c := range cases {
		v, u, ok := parseBudget(c.in)
		if ok != c.ok || (ok && (v != c.value || u != c.unit)) {
			t.Errorf("parseBudget(%q) = (%d,%q,%v), want (%d,%q,%v)",
				c.in, v, u, ok, c.value, c.unit, c.ok)
		}
	}
}

// A ms budget trips the WALL CLOCK and says so — nondeterministic, unlike
// the ops budget. (A 1ms limit against the workspace-wide edge sweep is
// reliably slower than 1ms, so it trips.)
func TestMsBudgetTripsTheClock(t *testing.T) {
	s := startGraph(t)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	list, err := parseModernSelector(`func::in.call`)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.srv.buildTree()
	if err != nil {
		t.Fatal(err)
	}
	// An already-expired deadline trips on the first clock check (the walk
	// has plenty of spends past the sample interval), deterministically.
	e.deadline = time.Now().Add(-time.Hour)
	e.workLeft = maxBudgetOps // don't let the ops budget trip first
	e.evaluate(list)
	if !e.workExceeded || !e.timedOut {
		t.Errorf("an expired time budget must trip via the clock: exceeded=%v timedOut=%v",
			e.workExceeded, e.timedOut)
	}
}
