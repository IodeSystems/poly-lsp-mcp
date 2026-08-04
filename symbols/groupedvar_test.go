package symbols

import (
	"strings"
	"testing"
)

// Grouped declarations. Go has four and tree-sitter parses only ONE of them
// through a list node — `var (…)` becomes var_declaration > var_spec_list >
// var_spec, while grouped const/type go straight to their spec. classifyGo
// listed import_spec_list but not var_spec_list, so the gather() walk never
// descended and every name in a grouped var block was missing from the
// index entirely: not mis-classed, not mis-ranged, absent. No selector could
// reach it, and two dogfood sessions burned ~11 calls each hunting one.
//
// All four forms are pinned together here because the asymmetry is the whole
// bug — testing var alone would not have caught it, and would not catch the
// grammar growing a const_spec_list later.
func TestGroupedDeclarationsIndexEveryName(t *testing.T) {
	src := []byte(`package p

import (
	"fmt"
	"os"
)

var (
	alpha = 1
	beta  = 2
)

const (
	gamma = 3
	delta = 4
)

type (
	Eps   int
	Zeta  string
)

var solo = 5
`)
	syms, err := FileSymbols("go", src)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"fmt": "import", "os": "import",
		"alpha": "var", "beta": "var", "solo": "var",
		"gamma": "const", "delta": "const",
		"Eps": "type", "Zeta": "type",
	}
	for sym, class := range want {
		got := symByPath(syms, sym)
		if got == nil {
			t.Errorf("missing %q; have %s", sym, symNames(syms))
			continue
		}
		if got.Class != class {
			t.Errorf("%q class = %q, want %q", sym, got.Class, class)
		}
		if got.DeclStartLine < 1 || got.DeclEndLine < got.DeclStartLine || got.NameStartLine < 1 {
			t.Errorf("%q ranges malformed: %+v", sym, got)
		}
	}
}

// Each name in a multi-spec group owns its OWN line, not the whole block —
// otherwise every sibling would report the same span and node_edit on one
// would rewrite all of them.
func TestGroupedVarSpecsKeepPerSpecRanges(t *testing.T) {
	src := []byte("package p\n\nvar (\n\talpha = 1\n\tbeta  = 2\n)\n")
	syms, err := FileSymbols("go", src)
	if err != nil {
		t.Fatal(err)
	}
	a, b := symByPath(syms, "alpha"), symByPath(syms, "beta")
	if a == nil || b == nil {
		t.Fatalf("both names should be indexed; have %s", symNames(syms))
	}
	if a.DeclStartLine != 4 || a.DeclEndLine != 4 {
		t.Errorf("alpha should span line 4 only; got %d-%d", a.DeclStartLine, a.DeclEndLine)
	}
	if b.DeclStartLine != 5 || b.DeclEndLine != 5 {
		t.Errorf("beta should span line 5 only; got %d-%d", b.DeclStartLine, b.DeclEndLine)
	}
}

// A SINGLE-spec group takes the whole declaration, keyword and parens
// included — the rule const/type already followed. Deleting just the spec
// would strand an empty `var ()`, and reading it back without the keyword
// is not what a caller means by the declaration.
func TestSingleSpecGroupedVarTakesTheWholeDeclaration(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"var", "package p\n\nvar (\n\tonly = 1\n)\n"},
		{"const", "package p\n\nconst (\n\tonly = 1\n)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			syms, err := FileSymbols("go", []byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			got := symByPath(syms, "only")
			if got == nil {
				t.Fatalf("missing %q; have %s", "only", symNames(syms))
			}
			// Lines 3-5: the keyword line, the spec, the closing paren.
			if got.DeclStartLine != 3 || got.DeclEndLine != 5 {
				t.Errorf("a single-spec group spans its whole declaration (3-5); got %d-%d",
					got.DeclStartLine, got.DeclEndLine)
			}
		})
	}
}

// symNames renders a symbol list for failure messages — the useful part of
// a miss is what WAS found next to what wasn't.
func symNames(syms []Symbol) string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.Sym+":"+s.Class)
	}
	return "[" + strings.Join(out, " ") + "]"
}
