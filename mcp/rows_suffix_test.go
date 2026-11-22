package mcp

import (
	"strings"
	"testing"
)

// ::rows on a ref element is the opt-out from the default far-node
// landing: the selector's answer is the EDGE rows themselves (site,
// from:, to:), and the text output says so in one line, naming the
// default far-node form so the reader learns the idiom from the output.

func TestRowsSuffixParses(t *testing.T) {
	list, err := parseModernSelector(`#'Server'::in.call::rows`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(list) != 1 || len(list[0].elems) != 2 {
		t.Fatalf("elems = %d, want 2 (id, ref)", len(list[0].elems))
	}
	ref := list[0].elems[1].comp
	if ref == nil || !ref.isRef || !ref.rows {
		t.Fatalf("ref element = %+v, want isRef && rows", ref)
	}
	if lastIsBareEdge(list) {
		t.Fatal("a ::rows element must NOT land on far nodes by default")
	}
}

// The bare form still lands on the far nodes — the regression this
// suffix must not touch.
func TestRowsSuffixKeepsBareFormLanding(t *testing.T) {
	s := newQueryServer(t, writeRefPosFixture(t))
	s.SetQueryWorkBudget(50_000_000)

	list, err := parseModernSelector(`#'Server'::in.type`)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.buildTree()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(e.evaluate(list)); n != 2 {
		t.Errorf("bare ::in.type lands on 2 far nodes; got %d", n)
	}
}

// ::rows on the LAST element returns the edge rows: for
// #'Server'::in.type there are 3 type uses (param, return, field), so
// the answer is 3 edge rows, not the 2 far nodes the bare form gives.
func TestRowsSuffixReturnsEdgeRows(t *testing.T) {
	s := newQueryServer(t, writeRefPosFixture(t))
	s.SetQueryWorkBudget(50_000_000)

	list, err := parseModernSelector(`#'Server'::in.type::rows`)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.buildTree()
	if err != nil {
		t.Fatal(err)
	}
	rows := e.evaluate(list)
	if n := len(rows); n != 3 {
		t.Fatalf("::rows returns the 3 edge rows; got %d", n)
	}
	for _, r := range rows {
		if r.class != "ref" {
			t.Errorf("row %q is not a ref (edge) node; class=%q", r.addr(), r.class)
		}
	}
}

// A kind/attr/id after ::rows is refused — the marker closes the element.
func TestRowsSuffixRefusesTrailingSelectors(t *testing.T) {
	for _, sel := range []string{
		`#'Server'::in.call::rows.type`,
		`#'Server'::in.call::rows#Server`,
		`#'Server'::in.call::rows[name=x]`,
		`#'Server'::in.call::rows::out.call`,
		`func::rows`,
		`#'Server'::in.call > *::rows`,
	} {
		if _, err := parseModernSelector(sel); err == nil {
			t.Errorf("%q: want a parse error", sel)
		}
	}
}

// The text output carries the one-line hint when (and only when) the
// last step is a ref element marked ::rows: it names the far-node form,
// so a model reading the edge rows learns the idiom from the output.
func TestQueryTextEdgeRowsHint(t *testing.T) {
	s := newQueryServer(t, writeRefPosFixture(t))

	run := func(sel string) string {
		var b strings.Builder
		if err := s.QueryText(sel, 0, 0, "", &b); err != nil {
			t.Fatalf("%s: %v", sel, err)
		}
		return b.String()
	}

	out := run(`#'Server'::in.type::rows`)
	if !strings.Contains(out, "drop ::rows (or use > *) to land on the far nodes") {
		t.Errorf("::rows text output must name the far-node form; got:\n%s", out)
	}
	// The bare form already lands on far nodes — the hint would be
	// redundant and must not fire.
	if out := run(`#'Server'::in.type`); strings.Contains(out, "drop ::rows") {
		t.Errorf("bare form must NOT carry the ::rows hint; got:\n%s", out)
	}
	// A non-edge answer carries no hint either.
	if out := run(`file.go func`); strings.Contains(out, "drop ::rows") {
		t.Errorf("containment query must NOT carry the hint; got:\n%s", out)
	}
}
