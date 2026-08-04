package mcp

import (
	"strings"
	"testing"
)

// A space is a NODE BOUNDARY, with no exceptions. It used to mean "descend"
// before a tag and "filter" before an attribute, and nothing in the response
// said which you got: `method name=X` was ONE node, not two. That rule was
// documented only in the `?` grammar help — asked for by 2 of 426 measured
// calls — while the always-present tool description said "space=descendant".
func TestSpaceIsAlwaysANodeBoundary(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// The filter is the bracketed form, and it is ONE element.
	filtered := query(t, s, map[string]any{"selector": "file[path=main.go]", "limit": 50})
	if got := nodes(filtered); len(got) != 1 || got[0] != "main.go" {
		t.Errorf("file[path=main.go] should be the file itself; got %v", got)
	}
	// The spaced form descends, so it returns what is INSIDE, never the file.
	descended := query(t, s, map[string]any{"selector": "file path=main.go", "limit": 50})
	for _, n := range nodes(descended) {
		if n == "main.go" {
			t.Errorf("`file path=main.go` descends; the file itself must not be in %v", nodes(descended))
		}
	}
	if len(nodes(descended)) == 0 {
		t.Error("`file path=main.go` should return the file's contents")
	}
}

// A bare attribute is exactly `*[…]` — sugar, not a separate rule.
func TestBareAttrIsSugarForUniversalWithAttrs(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	for _, pair := range [][2]string{
		{"path=main.go", "*[path=main.go]"},
		{"file > path=main.go", "file > *[path=main.go]"},
		{"name=Free", "*[name=Free]"},
	} {
		a := nodes(query(t, s, map[string]any{"selector": pair[0], "limit": 50}))
		b := nodes(query(t, s, map[string]any{"selector": pair[1], "limit": 50}))
		if len(a) != len(b) {
			t.Errorf("%q must equal %q: %v vs %v", pair[0], pair[1], a, b)
		}
	}
}

// `|` is OR, `&` is AND, `()` groups — over whole TESTS. Before this, OR had
// to be smuggled through a regex value, which worked for one axis and one
// operator: there was no way to say "named X or living in Y".
func TestAttrBooleans(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	count := func(sel string) int {
		return query(t, s, map[string]any{"selector": sel, "limit": 200}).TotalMatches
	}

	free, callsStart := count("func[name=Free]"), count("func[name=CallsStart]")
	if free == 0 || callsStart == 0 {
		t.Fatal("fixture drift: both funcs should exist")
	}
	if got := count("func[name=Free|name=CallsStart]"); got != free+callsStart {
		t.Errorf("OR must be the union: %d + %d != %d", free, callsStart, got)
	}
	// AND narrows, and is what several brackets already meant.
	if a, b := count("func[name=Free&path=main.go]"), count("func[name=Free][path=main.go]"); a != b || a != free {
		t.Errorf("AND should equal chained brackets and not widen: %d vs %d (free=%d)", a, b, free)
	}
	// Grouping binds tighter than the surrounding OR.
	grouped := count("func[name=CallsStart|(name=Free&path=main.go)]")
	if grouped != free+callsStart {
		t.Errorf("a group should not change the union here; got %d want %d", grouped, free+callsStart)
	}
	if narrowed := count("func[name=CallsStart|(name=Free&path=nowhere.go)]"); narrowed != callsStart {
		t.Errorf("the failing branch of the group should drop out; got %d want %d", narrowed, callsStart)
	}
	// OR across DIFFERENT axes — the thing that had no spelling at all before.
	if got := count("*[name=Free|path=notes.md]"); got <= free {
		t.Errorf("an OR across axes should be wider than either side; got %d", got)
	}
}

// Disambiguation is by QUOTING, which the language already used for exactly
// this. An unquoted `|` is the operator; a quoted value keeps its pipes.
func TestQuotedValueKeepsItsPipes(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	boolean := query(t, s, map[string]any{"selector": "func[name~=Free|name~=CallsStart]", "limit": 50})
	quoted := query(t, s, map[string]any{"selector": "func[name~='Free|CallsStart']", "limit": 50})
	if boolean.TotalMatches != quoted.TotalMatches || boolean.TotalMatches == 0 {
		t.Errorf("a quoted alternation must equal the boolean union: %d vs %d",
			boolean.TotalMatches, quoted.TotalMatches)
	}
}

// The migration case, and the likeliest way to reach a bad attribute name:
// `|` used to be regex alternation INSIDE a value. The error has to name that
// rather than recite the attribute list at someone who knows it.
func TestUnquotedAlternationNamesTheBooleanRule(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	msg := queryErr(t, s, map[string]any{"selector": "func[path~=app|util]"})
	for _, want := range []string{"BOOLEAN operators", "quote the value"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error should teach the migration (%q missing): %s", want, msg)
		}
	}
}

// An OR guarantees no single leaf, so the planner's index shortcuts — which
// assume an attribute MUST hold — have to see only the top-level conjuncts.
// Getting this wrong is a wrong answer, not a slow one.
func TestRequiredAttrsIgnoresLeavesUnderAnOr(t *testing.T) {
	list, err := parseModernSelector("func[name=a|name=b]")
	if err != nil {
		t.Fatal(err)
	}
	if got := list[0].elems[0].comp.attrs; len(got) != 0 {
		t.Errorf("no leaf under an OR is required; got %v", got)
	}
	list, err = parseModernSelector("func[name=a&path=b.go]")
	if err != nil {
		t.Fatal(err)
	}
	if got := list[0].elems[0].comp.attrs; len(got) != 2 {
		t.Errorf("both conjuncts are required; got %v", got)
	}
	list, err = parseModernSelector("func[name=a|(name=b&path=c.go)]")
	if err != nil {
		t.Fatal(err)
	}
	if got := list[0].elems[0].comp.attrs; len(got) != 0 {
		t.Errorf("a conjunct UNDER an or is still not required; got %v", got)
	}
}
