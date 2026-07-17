package mcp

import (
	"strings"
	"testing"
)

func TestSplitExplainPrefix(t *testing.T) {
	cases := []struct {
		in      string
		rest    string
		explain bool
	}{
		{":explain func#main", "func#main", true},
		{"  :explain  func ", "func", true},
		{":explain", "", true},
		{"func#main", "func#main", false},
		{":explained", ":explained", false}, // not the mode — no boundary
		{":root > *", ":root > *", false},   // a real leading pseudo, untouched
	}
	for _, c := range cases {
		rest, ex := splitExplain(c.in)
		if ex != c.explain || (ex && strings.TrimSpace(rest) != c.rest) {
			t.Errorf("splitExplain(%q) = (%q,%v), want (%q,%v)", c.in, rest, ex, c.rest, c.explain)
		}
	}
}

// :explain returns a cost tree: an a-priori est beside the measured work,
// with >x floors on the element that blew the budget. It RAN the query
// (that is the measured column) — it just renders the trace not matches.
func TestExplainReportsEstAndMeasured(t *testing.T) {
	s := newQueryServer(t, writeEstimatorFixture(t))

	// A completing query: est from the tallies, exact measured, no floors.
	e, _ := s.buildTree()
	list, _ := parseModernSelector(`func`)
	e.evaluate(list)
	rows := e.explainRows(list)
	if len(rows) != 1 {
		t.Fatalf("one element; got %d rows", len(rows))
	}
	if rows[0].Est != "3" { // 3 funcs in the fixture, from classCounts
		t.Errorf("bare-class est is classCounts[func]=3; got %q", rows[0].Est)
	}
	if strings.HasPrefix(rows[0].Measured, ">") {
		t.Errorf("a completing element measures EXACTLY, no floor; got %q", rows[0].Measured)
	}

	// An exact name filter reads NameFreq (free), not classCounts.
	e2, _ := s.buildTree()
	l2, _ := parseModernSelector(`#'Alpha'`)
	e2.evaluate(l2)
	if r := e2.explainRows(l2); r[0].Est == "?" {
		t.Error("an exact #name est must come from NameFreq, not be unknown")
	}
}

// A blown :explain marks the culprit with a >x floor and shows unreached
// elements as "—".
func TestExplainFloorsOnBlow(t *testing.T) {
	s := startGraph(t)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	s.srv.SetQueryWorkBudget(30)

	list, err := parseModernSelector(`func::in.call{1,3} > func`)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := s.srv.buildTree()
	e.evaluate(list)
	if !e.workExceeded {
		t.Skip("budget did not trip on this fixture")
	}
	rows := e.explainRows(list)
	var floors, unreached, blown int
	for _, r := range rows {
		if strings.HasPrefix(r.Measured, ">") {
			floors++
		}
		if r.Measured == "—" {
			unreached++
		}
		if r.Blown {
			blown++
		}
	}
	if blown != 1 {
		t.Errorf("exactly one element blew the budget; got %d marked", blown)
	}
	if floors == 0 {
		t.Error("the blown element must carry a >x floor")
	}
	if unreached == 0 {
		t.Error("elements after the blow never ran — must show —")
	}
}

// node_query :explain returns the explain payload, NOT matches — the
// documented result-shape fork.
func TestNodeQueryExplainReturnsTrace(t *testing.T) {
	s := startGraph(t)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_query", map[string]any{"selector": ":explain func"})
	if r.IsError {
		t.Fatalf("node_query :explain errored: %s", r.Content[0].Text)
	}
	txt := r.Content[0].Text
	if !strings.Contains(txt, `"explain"`) {
		t.Errorf("node_query :explain must return an explain trace, got: %s", txt)
	}
	if strings.Contains(txt, `"matches"`) {
		t.Errorf(":explain returns a trace, not matches; got: %s", txt)
	}
}
