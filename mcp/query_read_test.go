package mcp

import (
	"strings"
	"testing"
)

// readingOf parses a selector and renders its prose reading as one blob —
// every assertion here is about what a CALLER is told, so the tests read the
// same surface the caller does.
func readingOf(t *testing.T, sel string) string {
	t.Helper()
	list, err := parseModernSelector(sel)
	if err != nil {
		t.Fatalf("parse %q: %v", sel, err)
	}
	var b strings.Builder
	for _, r := range describeSelector(list) {
		b.WriteString(r.Clause + " => " + r.Means + "\n")
	}
	if s := subjectLine(list); s != "" {
		b.WriteString("SUBJECT: " + s + "\n")
	}
	return b.String()
}

// The whole reason the reading exists: `func > argument` returns ARGUMENTS.
// Read left-to-right as English it says the opposite, and a caller who
// believes the English gets a wrong result set with no error to warn them.
func TestReading_NamesTheSubjectAsTheLastElement(t *testing.T) {
	got := readingOf(t, "func > argument")
	if !strings.Contains(got, "SUBJECT:") {
		t.Fatalf("a multi-element chain must say what it returns:\n%s", got)
	}
	subj := got[strings.Index(got, "SUBJECT:"):]
	if !strings.Contains(subj, "`argument`") || !strings.Contains(subj, "NOT the `func`") {
		t.Errorf("the subject is the LAST element, and the line must say so:\n%s", subj)
	}
	// A single-element selector returns the only thing it names — saying so
	// is noise on every query that never had a chance to be wrong.
	if s := readingOf(t, "func"); strings.Contains(s, "SUBJECT:") {
		t.Errorf("a one-element selector needs no subject line:\n%s", s)
	}
}

// = is LITERAL and ~= is regex. The parser rejects a bare `|` because that one
// silently no-ops; every other metacharacter is a legal literal, so the
// reading is the ONLY place a reached-for pattern can be flagged without
// refusing a valid selector.
func TestReading_FlagsRegexPunctuationUnderALiteralOp(t *testing.T) {
	got := readingOf(t, "func[name=parse*]")
	if !strings.Contains(got, "LITERAL") || !strings.Contains(got, "~=") {
		t.Errorf("a literal op carrying `*` must point at ~=:\n%s", got)
	}
	// A dot is in almost every path, so cautioning on it would fire on
	// [path=main.go] — routine, correct, and not a mistake. Noise that
	// frequent trains the reader to skip the line that matters.
	if s := readingOf(t, "func[path=cmd/main.go]"); strings.Contains(s, "~=") {
		t.Errorf("a literal path must not be flagged as a botched pattern:\n%s", s)
	}
	// And ~= itself is never suspicious — it IS the pattern operator.
	if s := readingOf(t, "func[name~='parse|scan']"); strings.Contains(s, "LITERAL here") {
		t.Errorf("a regex op must not be warned about:\n%s", s)
	}
}

// selOpSpelling had no case for ~=, so it fell to the "=" default and every
// rendering — cost trace, zero-result hint, reading — echoed [name~=parse]
// back as [name=parse]: a REGEX quoted as a LITERAL. That is precisely the
// confusion the alternation error exists to prevent, reproduced by the tool's
// own output, on the selector the caller was already debugging.
func TestReading_EchoesTheOperatorTheCallerWrote(t *testing.T) {
	got := readingOf(t, "func[name~=parse]")
	if !strings.Contains(got, "[name~=parse]") {
		t.Errorf("~= must survive rendering, not degrade to =:\n%s", got)
	}
	if !strings.Contains(got, "REGEX") {
		t.Errorf("a ~= attr must be described as a regex:\n%s", got)
	}
	// The engine's default is unanchored, but printing "unanchored" beside a
	// pattern the caller anchored reads as a contradiction of the text next
	// to it.
	if s := readingOf(t, "func[name~=^New]"); strings.Contains(s, "no anchoring") {
		t.Errorf("an anchored pattern must not be called unanchored:\n%s", s)
	}
}

// :parents MOVES the tip upstream instead of filtering in place, so pseudos
// after it test a different set than the ones before it. Nothing in the
// spelling suggests a move, which is what makes it worth narrating.
func TestReading_SaysParentsMoves(t *testing.T) {
	got := readingOf(t, "method:parents(class)")
	if !strings.Contains(got, "MOVES UP") {
		t.Errorf(":parents must be described as a move, not a filter:\n%s", got)
	}
	if !strings.Contains(got, "`class`") {
		t.Errorf("the inner selector must be described too:\n%s", got)
	}
}

// A rendering that adds syntax teaches syntax nobody used — the rule
// renderElem already follows for the implied universal. Two clauses are
// synthesized by the parser and must not appear as if typed: the `*` in
// `#id`, and the child combinator carrying a pseudo-element.
func TestReading_DoesNotInventClausesTheCallerNeverWrote(t *testing.T) {
	got := readingOf(t, "#newInputStream")
	if strings.Contains(got, "* =>") {
		t.Errorf("`#id` is not `*#id` — the implied universal must stay unrendered:\n%s", got)
	}
	if !strings.Contains(got, "newInputStream") {
		t.Errorf("the id itself must still be described:\n%s", got)
	}
	// But a universal the caller DID write still earns its line.
	if s := readingOf(t, "* > func"); !strings.Contains(s, "* =>") {
		t.Errorf("an explicit `*` must be described:\n%s", s)
	}
	if s := readingOf(t, "func::signature"); strings.Contains(s, "> =>") {
		t.Errorf("a pseudo-element attaches to its host; no '>' was written:\n%s", s)
	}
}

// The combinators are the other half of the subject confusion: a space and a
// '>' look nearly identical and differ by every level of the tree.
func TestReading_DistinguishesTheCombinators(t *testing.T) {
	child := readingOf(t, "class > method")
	if !strings.Contains(child, "DIRECT children") {
		t.Errorf("'>' must be called out as direct-only:\n%s", child)
	}
	desc := readingOf(t, "class method")
	if !strings.Contains(desc, "any depth") {
		t.Errorf("a descendant combinator must say it spans depths:\n%s", desc)
	}
}

// The reading is derived from the AST, never the source text. A reading built
// by re-reading the caller's string could agree with their misconception and
// confirm the bug it exists to expose — so a selector the parser NORMALIZED
// must read as what will actually run.
func TestReading_ReportsWhatWillRunNotWhatWasTyped(t *testing.T) {
	// `.func` is repaired to the `func` type by normalizeSelector; the
	// reading must show the type it became.
	got := readingOf(t, ".func")
	if !strings.Contains(got, "type `func`") {
		t.Errorf("a normalized selector must read as what runs:\n%s", got)
	}
}

// A reading nobody can finish is a reading nobody reads. Deep nesting is
// bounded rather than rendered to the leaves.
func TestReading_BoundsNesting(t *testing.T) {
	got := readingOf(t, "func:where(class:where(method:where(argument)))")
	if !strings.Contains(got, "run it on its own") {
		t.Errorf("nesting past the cap must say so rather than recurse:\n%s", got)
	}
	if n := strings.Count(got, "\n"); n > 12 {
		t.Errorf("a bounded reading should stay short, got %d lines:\n%s", n, got)
	}
}
