package symbols

import (
	"strings"
	"testing"
)

// A declaration OWNS its doc comment. tree-sitter models comments as SIBLINGS,
// so the raw node span stops at `func` — which meant node_read returned a
// function without its documentation, and node_edit (which replaces the span)
// rewrote the body while leaving the old comment stranded above it, silently
// describing code that no longer existed.
func TestDeclSpanIncludesDocComment(t *testing.T) {
	src := `package p

// Save persists.
// Second doc line.
func Save(id string) error { return nil }

// Detached: blank line below, belongs to nobody.

func Other() {}
`
	syms, err := FileSymbols("go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	get := func(name string) *Symbol {
		for i := range syms {
			if syms[i].Sym == name {
				return &syms[i]
			}
		}
		t.Fatalf("no symbol %q in %+v", name, syms)
		return nil
	}

	// Save's span starts at the FIRST doc line (3), not at `func` (5).
	if s := get("Save"); s.DeclStartLine != 3 {
		t.Errorf("Save decl starts at line %d, want 3 (the whole doc block)", s.DeclStartLine)
	}
	// A blank line ends the block: Other's comment is not Other's.
	if s := get("Other"); s.DeclStartLine != 9 {
		t.Errorf("Other decl starts at line %d, want 9 — a comment separated by a "+
			"blank line is not a doc comment", s.DeclStartLine)
	}
}

// The whole point: read the span, get the docs.
func TestDeclSpanTextCarriesTheDocs(t *testing.T) {
	src := "package p\n\n// Doc line.\nfunc F() {}\n"
	syms, err := FileSymbols("go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var s Symbol
	for _, c := range syms {
		if c.Sym == "F" {
			s = c
		}
	}
	if s.Sym == "" {
		t.Fatalf("no symbol F in %+v", syms)
	}
	lines := strings.Split(src, "\n")
	span := strings.Join(lines[s.DeclStartLine-1:s.DeclEndLine], "\n")
	if !strings.Contains(span, "// Doc line.") {
		t.Errorf("decl span must carry the doc comment, got: %q", span)
	}
}
