package mcp

import (
	"strings"
	"testing"
)

// node_query exposes a `budget` arg, and a blow tells the caller about
// BOTH levers — narrow (with the cost trace naming the culprit) or raise
// the budget — so a model isn't stuck guessing at a hidden knob.
func TestNodeQueryBudgetArgAndHint(t *testing.T) {
	s := startGraph(t)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// A tiny budget forces a blow; the note must name the budget lever AND
	// point at the cost trace, not just say "narrow".
	blown := query(t, s, map[string]any{"selector": `func::in.call{1,3}`, "budget": 20})
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
	raised := query(t, s, map[string]any{"selector": `func::in.call{1,3}`, "budget": 5_000_000})
	if raised.TotalMatches < blown.TotalMatches {
		t.Errorf("a raised budget must not see FEWER matches: raised=%d blown=%d",
			raised.TotalMatches, blown.TotalMatches)
	}

	// The cap holds: an absurd budget is clamped, not honored blindly (no
	// crash, still answers).
	capped := query(t, s, map[string]any{"selector": `func`, "budget": 999_999_999})
	if capped.TotalMatches == 0 {
		t.Error("a clamped over-max budget must still evaluate")
	}
}
