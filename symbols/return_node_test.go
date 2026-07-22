package symbols

import (
	"strings"
	"testing"
)

// wantReturn asserts a return sym path exists with class "return", the
// given Alias (full type, "" when unqualified), and a sane range.
func wantReturn(t *testing.T, syms []Symbol, path, alias string) {
	t.Helper()
	got := symByPath(syms, path)
	if got == nil {
		t.Errorf("missing return %q; have %s", path, symPaths(syms))
		return
	}
	if got.Class != "return" {
		t.Errorf("%q class = %q, want return", path, got.Class)
	}
	if got.Alias != alias {
		t.Errorf("%q alias = %q, want %q", path, got.Alias, alias)
	}
	if got.DeclStartLine < 1 || got.DeclEndLine < got.DeclStartLine {
		t.Errorf("%q range malformed: %+v", path, got)
	}
}

func TestFileSymbolsGoReturns(t *testing.T) {
	src := []byte(`package main

import "io"

func Single() error { return nil }

func Tuple() (int, error) { return 0, nil }

func Qualified() io.Writer { return nil }

func Pointer() *Server { return nil }

func Void() {}

type Server struct{}
`)
	syms, err := FileSymbols("go", src)
	if err != nil {
		t.Fatal(err)
	}
	wantReturn(t, syms, "Single.error", "")
	// A tuple splits into one node per type — the point of the feature
	// for Go's (T, error) idiom.
	wantReturn(t, syms, "Tuple.int", "")
	wantReturn(t, syms, "Tuple.error", "")
	// A qualified type: leaf is the last segment, full form is the alias.
	wantReturn(t, syms, "Qualified.Writer", "io.Writer")
	wantReturn(t, syms, "Pointer.*Server", "")

	// A void function has no return child.
	for _, s := range syms {
		if s.Class == "return" && strings.HasPrefix(s.Sym, "Void.") {
			t.Errorf("Void() should have no return node, got %q", s.Sym)
		}
	}
}

func TestFileSymbolsTypeScriptReturns(t *testing.T) {
	src := []byte(`export function f(): number { return 1; }
export function g(): string | null { return null; }
class C { m(x: number): boolean { return true; } }
function noAnn() { return 1; }
`)
	syms, err := FileSymbols("typescript", src)
	if err != nil {
		t.Fatal(err)
	}
	wantReturn(t, syms, "f.number", "")
	// A union type is one expression, kept whole.
	wantReturn(t, syms, "g.string | null", "")
	wantReturn(t, syms, "C.m.boolean", "")
	for _, s := range syms {
		if s.Class == "return" && strings.HasPrefix(s.Sym, "noAnn.") {
			t.Errorf("an un-annotated function has no return node, got %q", s.Sym)
		}
	}
}

func TestFileSymbolsPythonReturns(t *testing.T) {
	src := []byte(`def f() -> int:
    return 1

class C:
    def m(self) -> bool:
        return True

def noAnn():
    return 1
`)
	syms, err := FileSymbols("python", src)
	if err != nil {
		t.Fatal(err)
	}
	wantReturn(t, syms, "f.int", "")
	wantReturn(t, syms, "C.m.bool", "")
	for _, s := range syms {
		if s.Class == "return" && strings.HasPrefix(s.Sym, "noAnn.") {
			t.Errorf("an un-annotated def has no return node, got %q", s.Sym)
		}
	}
}
