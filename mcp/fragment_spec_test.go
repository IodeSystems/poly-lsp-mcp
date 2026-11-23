package mcp

import (
	"strings"
	"testing"
)

// These tests are the review anchor for the F27 ::grep fragment spec rows.
// The rows were written before the surface existed; each test here names the
// row it proves and asserts the row's Then/And clauses against a live query,
// so a future change that breaks a promise is caught at the row, not buried in
// the corpus of incidental fragment coverage.

// F27 row 1 — "grep produces one node per matching line".
// Given a workspace with at least one file containing a line matching a
// pattern, when a selector ends in ::grep('pattern'), the result carries one
// generated node per matching source line, and each row shows the matched
// text and the file address.
func TestFragmentOneNodePerMatchingLine(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// Host is the FILE (the id form names the file node, not the symbols in
	// it), so a row's address is the file the line was found in. "func" sits
	// on several distinct source lines of main.go.
	q := query(t, s, map[string]any{"selector": `#'main.go'::grep('func')`, "limit": 50})
	if q.TotalMatches == 0 {
		t.Fatal("fixture drift: main.go should have lines matching 'func'")
	}
	if len(q.Matches) != q.TotalMatches {
		t.Fatalf("one node per matching line: %d rows for %d matches", len(q.Matches), q.TotalMatches)
	}
	seenLine := map[int]bool{}
	for _, m := range q.Matches {
		if m.Class != "::grep" {
			t.Errorf("a fragment row's type is its selector spelling; got %q", m.Class)
		}
		// Each row shows the matched text.
		if !strings.Contains(m.Text, "func") {
			t.Errorf("a row must carry its matched line; got %q", m.Text)
		}
		// Each row shows the file address (the host the line was found in).
		if m.In != "main.go" {
			t.Errorf("a row must carry the file address; got %q", m.In)
		}
		// The line is where the matched line sits, not the host's span.
		if m.At[0] == 0 {
			t.Errorf("a row must carry its line number; got %v", m.At)
		}
		if seenLine[m.At[0]] {
			t.Errorf("two rows for one source line %d — a line is one node", m.At[0])
		}
		seenLine[m.At[0]] = true
	}
	if len(seenLine) != q.TotalMatches {
		t.Errorf("expected %d distinct matching lines, saw %d", q.TotalMatches, len(seenLine))
	}
}

// F27 row 2 — "context flags add surrounding lines".
// Given a ::grep selector with a context flag -A1, each row carries the
// matched line plus the following line, and the rollup shows a per-file
// count of matches.
func TestFragmentA1ContextAndPerFileRollup(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// Host is the FILE, so the context is clipped to the whole file and the
	// following line is present. "func" matches several distinct lines of
	// main.go, each with a distinct following line.
	q := query(t, s, map[string]any{"selector": `#'main.go'::grep('-A1 func')`, "limit": 50})
	if q.TotalMatches == 0 {
		t.Fatal("fixture drift: main.go should have lines matching 'func'")
	}
	for _, m := range q.Matches {
		if !strings.Contains(m.Text, "func") {
			t.Errorf("a row must carry its matched line; got %q", m.Text)
		}
		// -A1: the following line rides along.
		if len(m.After) != 1 {
			t.Errorf("-A1 must carry exactly one following line; got %v", m.After)
		}
	}
	// The rollup is a per-file count of matches: every match is in main.go.
	if got := q.Rollup["main.go"]; got != q.TotalMatches {
		t.Errorf("the rollup must count matches per file; got main.go=%d, want %d", got, q.TotalMatches)
	}
	if len(q.Rollup) != 1 {
		t.Errorf("a single-file query rolls up to one file; got %v", q.Rollup)
	}
	// Context is OFF by default — the same pattern without -A1 carries no
	// following line, so -A1 is what added it.
	plain := query(t, s, map[string]any{"selector": `#'main.go'::grep('func')`, "limit": 50})
	for _, m := range plain.Matches {
		if len(m.After) != 0 {
			t.Errorf("context is opt-in; without -A a row carries no following line, got %v", m.After)
		}
	}
}

// F27 row 3 — "contains shares the matcher".
// Given a :contains selector with a pattern, it uses the same matcher as
// ::grep, the same flag vocabulary as ::grep, and the context flags and -c
// are not accepted.
func TestContainsSharesMatcherAndFlagVocabulary(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// Same matcher: a REGEX pattern under -E — literal mode would match
	// nothing, so a hit proves :contains compiles with ::grep's matcher.
	a := query(t, s, map[string]any{"selector": `method:contains('-E fmt.P.intln')`, "limit": 50})
	b := query(t, s, map[string]any{"selector": `method:where(::grep('-E fmt.P.intln'))`, "limit": 50})
	if len(nodes(a)) == 0 || len(nodes(a)) != len(nodes(b)) {
		t.Fatalf(":contains must use ::grep's matcher (regex -E); got %v vs %v", nodes(a), nodes(b))
	}
	// Same flag vocabulary: the boolean flags ::grep accepts are accepted
	// here too — none of them refuse.
	for _, sel := range []string{
		`method:contains('-i PRINTLN')`,     // -i
		`method:contains('-w Start')`,       // -w
		`method:contains('-F fmt.Println')`, // -F
		`method:contains('-v init')`,        // -v
		`method:contains('-n fmt')`,         // -n
		`method:contains('-r fmt')`,         // -r
	} {
		if r := s.callTool("node_query", map[string]any{"selector": sel, "limit": 5}); r.IsError {
			t.Errorf(":contains must accept the grep boolean flags; %s refused: %s", sel, r.Content[0].Text)
		}
	}
	// The context flags are not accepted.
	for _, f := range []string{"-A1", "-B1", "-C1"} {
		msg := queryErr(t, s, map[string]any{"selector": `method:contains('` + f + ` x')`})
		if !strings.Contains(msg, strings.TrimLeft(f, "-")) {
			t.Errorf("a context flag must be refused naming it; %s -> %q", f, msg)
		}
	}
	// -c is not accepted.
	msg := queryErr(t, s, map[string]any{"selector": `method:contains('-c x')`})
	if !strings.Contains(msg, "-c") {
		t.Errorf("-c must be refused naming it; got %q", msg)
	}
}

// F27 row 4 — "unknown flag is refused".
// Given a ::grep selector with a flag not in -i -w -E -F -v -n -r -A -B -C,
// the query is refused with an error naming the unknown flag.
func TestGrepUnknownFlagRefusedNamingTheFlag(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	for _, f := range []string{"-Z", "-x", "-Q"} {
		msg := queryErr(t, s, map[string]any{"selector": `*::grep('` + f + ` derp')`})
		if !strings.Contains(msg, f) {
			t.Errorf("an unknown flag must be refused naming it; %s -> %q", f, msg)
		}
	}
	// A genuine unknown flag is refused even among valid ones.
	msg := queryErr(t, s, map[string]any{"selector": `*::grep('-i -Z derp')`})
	if !strings.Contains(msg, "-Z") {
		t.Errorf("an unknown flag must be named even next to valid flags; got %q", msg)
	}
}
