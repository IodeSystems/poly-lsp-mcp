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

// A comment TRAILING a declaration documents that declaration, not the one
// below it. Both span rules used to ask only whether a comment ended on the
// line above, which a trailing comment does — so every field's comment was
// credited to its neighbour. In go and java that only mis-aimed ::comment; in
// typescript and kotlin the neighbour's comment was pulled into the decl span,
// and deleting the field destroyed it while stranding the field's own.
func TestTrailingCommentBelongsToTheLineItSitsOn(t *testing.T) {
	cases := []struct {
		lang string
		src  string
		// want[sym] = the text the decl span must cover, "" for "no comment".
		want map[string]string
	}{
		{"go", "package p\n\ntype C struct {\n\tA string // doc for A\n\tB bool   // doc for B\n\tD int\n}\n",
			map[string]string{
				"C.A": "A string // doc for A",
				"C.B": "B bool   // doc for B",
				"C.D": "D int",
			}},
		{"typescript", "export class C {\n  a: string = \"\"; // doc for a\n  b = false; // doc for b\n  d = 0;\n}\n",
			map[string]string{
				"C.a": "a: string = \"\"; // doc for a",
				"C.b": "b = false; // doc for b",
				"C.d": "d = 0",
			}},
		{"kotlin", "class C {\n  val a: String = \"\" // doc for a\n  val b = false // doc for b\n  val d = 0\n}\n",
			map[string]string{
				"C.a": "val a: String = \"\" // doc for a",
				"C.b": "val b = false // doc for b",
				"C.d": "val d = 0",
			}},
		{"python", "class C:\n    a = \"\"  # doc for a\n    b = False  # doc for b\n    d = 0\n",
			map[string]string{
				"C.a": "a = \"\"  # doc for a",
				"C.b": "b = False  # doc for b",
				"C.d": "d = 0",
			}},
		{"c", "struct C {\n  char *a; // doc for a\n  int b; // doc for b\n  int d;\n};\n",
			map[string]string{
				"C.a": "char *a; // doc for a",
				"C.b": "int b; // doc for b",
				"C.d": "int d;",
			}},
	}

	for _, c := range cases {
		syms, err := FileSymbols(c.lang, []byte(c.src))
		if err != nil {
			t.Errorf("%s: %v", c.lang, err)
			continue
		}
		lines := strings.Split(c.src, "\n")
		for name, want := range c.want {
			var s *Symbol
			for i := range syms {
				if syms[i].Sym == name {
					s = &syms[i]
				}
			}
			if s == nil {
				t.Errorf("%s: no symbol %q", c.lang, name)
				continue
			}
			if s.DeclStartLine != s.DeclEndLine {
				t.Errorf("%s %s: decl spans lines %d..%d, want one line — it has "+
					"reached across into a neighbour", c.lang, name, s.DeclStartLine, s.DeclEndLine)
				continue
			}
			line := lines[s.DeclStartLine-1]
			end := min(s.DeclEndCol-1, len(line))
			if got := line[s.DeclStartCol-1 : end]; got != want {
				t.Errorf("%s %s: decl text = %q, want %q", c.lang, name, got, want)
			}
		}
	}
}

// ::comment reads from the same rule, so it must name the declaration's OWN
// trailing comment — never the one belonging to the line above.
func TestTrailingCommentIsTheDeclsOwnDoc(t *testing.T) {
	src := "package p\n\ntype C struct {\n\tA string // doc for A\n\tB bool   // doc for B\n\tD int\n}\n"
	syms, err := FileSymbols("go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"C.A": 4, "C.B": 5, "C.D": 0} // 1-based line of its doc
	for _, s := range syms {
		w, ok := want[s.Sym]
		if !ok {
			continue
		}
		if s.CommentStartLine != w {
			t.Errorf("%s doc comment on line %d, want %d", s.Sym, s.CommentStartLine, w)
		}
	}
}

// The upward rule must survive: a comment on its OWN line above a declaration
// is still that declaration's doc. Go's grammar puts an anonymous newline
// terminator between declarations that ends at column 0 of the comment's row,
// so a naive "previous sibling ends on this row" test marks every doc comment
// in the language as trailing.
func TestOwnLineCommentIsStillADocComment(t *testing.T) {
	src := "package p\n\ntype C struct {\n\tA string // doc for A\n\t// doc for B, on its own line\n\tB bool\n}\n"
	syms, err := FileSymbols("go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range syms {
		switch s.Sym {
		case "C.B":
			if s.DeclStartLine != 5 {
				t.Errorf("C.B decl starts at line %d, want 5 (its own-line doc comment)", s.DeclStartLine)
			}
			if s.CommentStartLine != 5 {
				t.Errorf("C.B doc comment on line %d, want 5", s.CommentStartLine)
			}
		case "C.A":
			if s.DeclEndLine != 4 {
				t.Errorf("C.A decl ends on line %d, want 4 — it must not reach down "+
					"into B's doc comment", s.DeclEndLine)
			}
		}
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
