package mcp

import (
	"strings"
	"testing"
)

// ::grep used to demand exactly ONE quoted string, and real sessions kept
// writing shell instead. Every one of these means something unambiguous, and
// rejecting them bought a round-trip and nothing else.
func TestGrepArgAcceptsShellQuoting(t *testing.T) {
	for _, tc := range []struct{ in, want, why string }{
		{`'-E foo')`, "-E foo", "the plain form still works"},
		{`'-E ''foo''')`, "-E foo", "doubled quotes: adjacent segments concatenate"},
		{`'-E 'foo')`, "-E foo", "unterminated quote runs to the close"},
		{`'-E' 'foo')`, "-E foo", "separate argv words rejoin with one space"},
		{`'a'|'b')`, "a|b", "quoted | quoted concatenates around the pipe"},
		{`'-E (Get|Post)\(')`, `-E (Get|Post)\(`, "parens inside quotes survive"},
		{`'-i -A2 derp')`, "-i -A2 derp", "several flags"},
	} {
		p := &modSelParser{s: []rune(tc.in)}
		got, err := p.parseGrepArg()
		if err != nil {
			t.Errorf("%s: %s -> error %v", tc.why, tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: %s -> %q, want %q", tc.why, tc.in, got, tc.want)
		}
	}
}

// Empty and unclosed stay errors: forgiving about QUOTING is not the same as
// guessing at a pattern that was never written.
func TestGrepArgStillRejectsTheAmbiguous(t *testing.T) {
	for _, in := range []string{`)`, `''`, `'unclosed`} {
		p := &modSelParser{s: []rune(in)}
		if got, err := p.parseGrepArg(); err == nil {
			t.Errorf("%q should be refused; got %q", in, got)
		}
	}
}

// The literal-vs-regex note has to see what was actually SEARCHED. grepArg
// used to re-extract the first quoted segment from the raw selector, which
// was correct while ::grep took one — and silently wrong the moment segments
// concatenated: `::grep('a'|'b')` searches "a|b" and reported "a", so the
// note stopped firing on the shape that needs it most.
func TestLiteralNoteSeesTheConcatenatedPattern(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	q := query(t, s, map[string]any{
		"selector": `path=main.go ::grep('Server'|'Free')`, "limit": 5,
	})
	if q.TotalMatches != 0 {
		t.Fatalf("a literal search for \"Server|Free\" should find nothing; got %d", q.TotalMatches)
	}
	if !strings.Contains(q.Note, "searched LITERALLY") {
		t.Fatalf("the note must fire on the concatenated pattern; got %q", q.Note)
	}
	if !strings.Contains(q.Note, "Server|Free") {
		t.Errorf("and must quote what was actually searched, not the first segment; got %q", q.Note)
	}
}

// The shell forms reach the same rows as the canonical one — the point is
// that they are the SAME query, not merely accepted.
func TestShellQuotedGrepFindsTheSameRows(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	want := query(t, s, map[string]any{"selector": `path=main.go ::grep('-E Server|Free')`, "limit": 50})
	if want.TotalMatches == 0 {
		t.Fatal("fixture drift: the canonical form should match something")
	}
	for _, sel := range []string{
		`path=main.go ::grep('-E ''Server|Free''')`,
		`path=main.go ::grep('-E 'Server|Free')`,
		`path=main.go ::grep('-E' 'Server|Free')`,
	} {
		got := query(t, s, map[string]any{"selector": sel, "limit": 50})
		if got.TotalMatches != want.TotalMatches {
			t.Errorf("%s matched %d, canonical matched %d", sel, got.TotalMatches, want.TotalMatches)
		}
	}
}
