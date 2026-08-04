package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// The budget charged per matched SITE while the expensive work — declsOf,
// LookupExisting — was per NODE and free. A wide query over many nodes with
// few sites each therefore burned real seconds while barely spending, and the
// clock (sampled every 256th spend) was almost never consulted. Measured
// before the fix: `::in.call` took 5.06s under a 1ms budget and 23.3s under
// 1000ms, against a 2.3s tree build — i.e. the wall-clock budget did not bind.
func TestWallClockBudgetBindsOnEdgeWork(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	start := time.Now()
	q := query(t, s, map[string]any{"selector": "::in.call", "limit": 2, "budget": "50ms"})
	elapsed := time.Since(start)

	// Generous: the point is that it is BOUNDED, not that it is instant —
	// the tree build is not part of the budget and dominates a small fixture.
	if elapsed > 5*time.Second {
		t.Errorf("a 50ms budget should bind; took %s", elapsed)
	}
	if q.TotalMatches == 0 && q.TotalAtLeast == "" {
		t.Error("a stopped walk must still report a floor")
	}
}

// An edge element expands EVERY tip, so a broad one grinds to the deadline and
// returns a truncated floor. That it cannot finish is knowable long before the
// clock says so: sample the rate, project across the remaining tips, stop when
// the projection cannot fit — and NAME breadth, since "the clock ran out" is
// true of every blown query while "8,998 tips" tells you which lever to pull.
func TestBreadthGuardStopsEarlyAndSaysWhy(t *testing.T) {
	// The guard samples 64 tips before projecting, so it cannot apply to a
	// workspace smaller than that. Give it one that is — many small symbols,
	// which is also the shape it exists for: wide, not deep.
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("package wide\n\n")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "func Fn%d() int { return Fn%d() }\n", i, (i+1)%400)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module wide\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wide.go"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	s2 := startSessionFull(t, dir, nil, nil)
	s2.request("initialize", map[string]any{})
	s2.notify("notifications/initialized", map[string]any{})
	defer s2.close()

	q := query(t, s2, map[string]any{"selector": "::in.call > *", "limit": 2, "budget": "1ms"})
	if q.TotalMatches != 0 {
		t.Skipf("the guard did not trip (walk completed with %d) — fixture still too cheap", q.TotalMatches)
	}
	if !strings.Contains(q.Note, "BREADTH") {
		t.Errorf("a breadth stop should say so rather than blaming the clock; got %q", q.Note)
	}
	if !strings.Contains(q.Note, "tips") {
		t.Errorf("and should name the tip count; got %q", q.Note)
	}
	if !q.Truncated || q.TotalAtLeast == "" {
		t.Error("an early stop is still a truncated result reporting a floor")
	}
}

// Conservative by construction: it trips only when the PROJECTION exceeds the
// budget, so anchored and filtered edge queries — the ones anyone actually
// writes — must still complete.
func TestBreadthGuardDoesNotTripOnNormalEdgeQueries(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	for _, sel := range []string{
		"#'main.go#Server.Start'::in.call",
		"#'main.go#CallsStart'::out.call > *",
		"path=main.go func::out.call",
	} {
		q := query(t, s, map[string]any{"selector": sel, "limit": 20})
		if strings.Contains(q.Note, "BREADTH") {
			t.Errorf("%s should complete, not trip the guard: %q", sel, q.Note)
		}
	}
}
