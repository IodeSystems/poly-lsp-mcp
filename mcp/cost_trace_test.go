package mcp

import (
	"strings"
	"testing"
)

// A budget blow renders the selector as a per-element cost trace pointing
// at the element that ate the budget — always-on, no :explain prefix
// needed. The generic "narrow it" warning becomes legible: WHICH element.
func TestBudgetBlowTracePointsAtCulprit(t *testing.T) {
	s := startGraph(t)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	s.srv.SetQueryWorkBudget(30) // trip mid-walk on the edge element

	// func (cheap) then a transitive edge (expensive). The edge should be
	// the culprit, not the leading func.
	list, err := parseModernSelector(`func::in.call{1,3}`)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.srv.buildTree()
	if err != nil {
		t.Fatal(err)
	}
	e.evaluate(list)
	if !e.workExceeded {
		t.Skip("budget did not trip on this fixture; nothing to trace")
	}

	trace := e.costTrace(list)
	if len(trace) == 0 {
		t.Fatal("a blown query must produce a cost trace")
	}
	joined := strings.Join(trace, "\n")
	// The culprit line is the edge element and carries the marker.
	var culprit string
	for _, l := range trace {
		if strings.Contains(l, "budget ran out here") {
			culprit = l
		}
	}
	if culprit == "" {
		t.Fatalf("the trace must mark the element that blew the budget:\n%s", joined)
	}
	if !strings.Contains(culprit, "::in") {
		t.Errorf("the edge element ate the budget, not the leading func; got %q", culprit)
	}
	// The leading func is billed too (it collected the hosts) but is not
	// the culprit.
	if !strings.Contains(joined, "func") {
		t.Errorf("every element appears in the trace; got:\n%s", joined)
	}
}

// node_query surfaces the same trace as a `cost` array on a blow, so the
// model narrows the right element instead of guessing.
func TestNodeQueryEmitsCostOnBlow(t *testing.T) {
	s := startGraph(t)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	s.srv.SetQueryWorkBudget(30)

	q := query(t, s, map[string]any{"selector": `func::in.call{1,3}`, "limit": 50})
	if !q.Truncated {
		t.Skip("budget did not trip; nothing to trace")
	}
	if len(q.Cost) == 0 {
		t.Error("a truncated node_query result must carry a cost trace")
	}
}

// A walk that stopped EARLY knows a floor, not a total — whether it stopped
// at the row limit or ran out of budget. The budget case used to report
// `totalMatches` flat: `::in.call > *` blows the clock and answered
// "totalMatches: 0", a positive claim of NONE for a walk that never finished,
// one field away from a note saying results may be incomplete.
func TestBudgetBlowReportsAFloorNotATotal(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	blown := query(t, s, map[string]any{
		"selector": "::in.call > *", "limit": 3, "budget": "1ops",
	})
	if blown.TotalMatches != 0 {
		t.Fatalf("fixture drift: expected the budget to blow; got %d", blown.TotalMatches)
	}
	if blown.TotalAtLeast == "" {
		t.Error("a budget-blown count must be reported as a FLOOR, not an exact total")
	}
	if !blown.Truncated {
		t.Error("and must be marked truncated")
	}
	if !strings.Contains(blown.Note, "INCOMPLETE") {
		t.Errorf("the note should still say results may be incomplete; got %q", blown.Note)
	}

	// A query that finishes still reports an exact total — the floor key is
	// for uncertainty, not for every answer.
	done := query(t, s, map[string]any{"selector": "path=main.go func", "limit": 50})
	if done.TotalMatches == 0 || done.TotalAtLeast != "" {
		t.Errorf("a completed walk reports an exact total; got %d / %q",
			done.TotalMatches, done.TotalAtLeast)
	}
}
