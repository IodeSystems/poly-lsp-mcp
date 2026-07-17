package mcp

import (
	"strings"
	"testing"
)

// The CLI resolves no edges (no child LSP), so any ::in/::out answer is
// lexical — and the tree renders far ends without a conf column. A
// caveat must fire on edge selectors so a name-keyed guess isn't read as
// a resolved fact, and must NOT fire on plain containment queries (where
// there's nothing lexical to warn about).
func TestQueryTextLexicalEdgeCaveat(t *testing.T) {
	s := newQueryServer(t, writeRefPosFixture(t))

	run := func(sel string) string {
		var b strings.Builder
		if err := s.QueryText(sel, 0, 0, "", &b); err != nil {
			t.Fatalf("%s: %v", sel, err)
		}
		return b.String()
	}

	// An edge selector — with matches — carries the caveat.
	if out := run(`#'Server'::in.type`); !strings.Contains(out, "name-keyed (lexical)") {
		t.Errorf("edge query must carry the lexical caveat; got:\n%s", out)
	}
	// The traversal case (`> *` lands on symbols, no ref row in output)
	// still crossed a lexical edge — the caveat must still fire.
	if out := run(`#'Server'::in.return.type > *`); !strings.Contains(out, "name-keyed (lexical)") {
		t.Errorf("edge traversal must carry the caveat even when rows are symbols; got:\n%s", out)
	}
	// A plain containment query crossed no edge — no caveat.
	if out := run(`file.go func`); strings.Contains(out, "name-keyed (lexical)") {
		t.Errorf("containment query must NOT carry the edge caveat; got:\n%s", out)
	}
	// An empty edge answer is a lexical answer too — caveat still fires.
	if out := run(`#'Nonexistent'::in.call`); !strings.Contains(out, "name-keyed (lexical)") {
		t.Errorf("empty edge answer must still carry the caveat; got:\n%s", out)
	}
}
