// Package segment cuts source text into addressable spans: the symbols of a
// code file and the heading sections of a Markdown file.
//
// It owns the tree-sitter symbol walk. Package symbols builds its
// FileSymbols on Walk, so the two cannot drift apart.
//
// Import rule: segment imports only the tree-sitter adapter
// (internal/ts, which wraps the grammars) and the standard library. It must
// not import config, internal/git, yaml.v3 or any LSP or MCP package, so a
// consumer that only wants spans does not link the rest of the module.
// segment_test.go enforces this.
package segment

import "fmt"

// Span is one cut of a source file. Lines are 1-based and EndLine is
// exclusive. Spans come in source order and are flat; nesting shows only in
// the dotted Sym ("Server.Start").
type Span struct {
	Sym   string
	Class string

	StartLine int
	EndLine   int

	// CommentStartLine is the first line of the doc comment above the
	// declaration, 0 when there is none.
	CommentStartLine int
	// BodyStartLine is the line where a callable's body begins, 0 when it
	// has none.
	BodyStartLine int
}

// Symbols returns the declarations of content, parsed as lang. It returns an
// error for a language without a grammar (see Languages).
//
// Parameter, return-type and annotation entries of the full symbol walk are
// left out: they are slices of a declaration, not declarations.
func Symbols(lang string, content []byte) ([]Span, error) {
	syms, err := Walk(lang, content)
	if err != nil {
		return nil, err
	}
	return spans(syms), nil
}

// Sections returns one span per heading section of a Markdown document. Sym
// is the heading path, and a section's span covers its heading, its body and
// its nested sections. Text above the first heading is not a section.
func Sections(content []byte) ([]Span, error) {
	syms, err := Walk("markdown", content)
	if err != nil {
		return nil, fmt.Errorf("sections: %w", err)
	}
	return spans(syms), nil
}

func spans(syms []Symbol) []Span {
	out := make([]Span, 0, len(syms))
	for _, s := range syms {
		switch s.Class {
		case "argument", "return", "annotation":
			continue
		}
		// A declaration ending at column 1 already ends at the start of a
		// line (a Markdown section); any other end is inside its last line.
		end := s.DeclEndLine
		if s.DeclEndCol != 1 {
			end++
		}
		out = append(out, Span{
			Sym:              s.Sym,
			Class:            s.Class,
			StartLine:        s.DeclStartLine,
			EndLine:          end,
			CommentStartLine: s.CommentStartLine,
			BodyStartLine:    s.BodyStartLine,
		})
	}
	return out
}
