package symbols

import (
	"strings"
	"testing"
)

func symByPath(syms []Symbol, sym string) *Symbol {
	for i := range syms {
		if syms[i].Sym == sym {
			return &syms[i]
		}
	}
	return nil
}

func TestFileSymbolsGoNestingAndClasses(t *testing.T) {
	src := []byte(`package main

const Pi = 3.14

type Server struct {
	Name string
}

func (s *Server) Start() error { return nil }

func Free() {}
`)
	syms, err := FileSymbols("go", src)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"Pi":           "const",
		"Server":       "struct",
		"Server.Name":  "field",
		"Server.Start": "method",
		"Free":         "func",
	}
	for sym, class := range cases {
		got := symByPath(syms, sym)
		if got == nil {
			t.Errorf("missing %q; have %+v", sym, syms)
			continue
		}
		if got.Class != class {
			t.Errorf("%q class = %q, want %q", sym, got.Class, class)
		}
		if got.DeclStartLine < 1 || got.DeclEndLine < got.DeclStartLine {
			t.Errorf("%q decl range malformed: %+v", sym, got)
		}
		if got.NameStartLine < 1 {
			t.Errorf("%q name range malformed: %+v", sym, got)
		}
	}
}

func TestFileSymbolsDisambiguatesSameNameSiblings(t *testing.T) {
	src := []byte("package main\n\nfunc init() {}\n\nfunc init() {}\n")
	syms, err := FileSymbols("go", src)
	if err != nil {
		t.Fatal(err)
	}
	if symByPath(syms, "init[1]") == nil || symByPath(syms, "init[2]") == nil {
		t.Errorf("expected init[1] and init[2]; have %+v", syms)
	}
	if symByPath(syms, "init") != nil {
		t.Errorf("bare init should not be emitted when there are duplicates")
	}
}

func TestFileSymbolsTypeScriptClassMembers(t *testing.T) {
	src := []byte(`export class UserService {
  name: string;
  constructor() {}
  getUser() { return ""; }
}
export enum Color { Red, Green }
`)
	syms, err := FileSymbols("typescript", src)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"UserService":             "class",
		"UserService.name":        "field",
		"UserService.constructor": "ctor",
		"UserService.getUser":     "method",
		"Color":                   "enum",
		"Color.Red":               "field",
	}
	for sym, class := range cases {
		got := symByPath(syms, sym)
		if got == nil {
			t.Errorf("missing %q; have %+v", sym, syms)
			continue
		}
		if got.Class != class {
			t.Errorf("%q class = %q, want %q", sym, got.Class, class)
		}
	}
}

func TestFileSymbolsUnsupportedLanguageErrors(t *testing.T) {
	if _, err := FileSymbols("markdown", []byte("# hi")); err == nil {
		t.Error("expected error for language without a grammar")
	}
}

// ------------------------------------------------------- .argument nodes

// wantArgs asserts each sym path exists with class "argument" and a
// sane, non-degenerate range.
func wantArgs(t *testing.T, syms []Symbol, paths ...string) {
	t.Helper()
	for _, p := range paths {
		got := symByPath(syms, p)
		if got == nil {
			t.Errorf("missing argument %q; have %s", p, symPaths(syms))
			continue
		}
		if got.Class != "argument" {
			t.Errorf("%q class = %q, want argument", p, got.Class)
		}
		if got.DeclStartLine < 1 || got.DeclEndLine < got.DeclStartLine {
			t.Errorf("%q decl range malformed: %+v", p, got)
		}
		if got.NameStartLine < 1 {
			t.Errorf("%q name range malformed: %+v", p, got)
		}
	}
}

func symPaths(syms []Symbol) string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.Sym+":"+s.Class)
	}
	return strings.Join(out, " ")
}

func TestFileSymbolsGoArguments(t *testing.T) {
	src := []byte(`package main

func Add(a, b int, name string, opts ...Opt) (int, error) { return 0, nil }

type Server struct{}

func (s *Server) Start(ctx context.Context, retries int) error { return nil }

func NoParams() {}
`)
	syms, err := FileSymbols("go", src)
	if err != nil {
		t.Fatal(err)
	}
	// Multi-name ("a, b int"), plain, and variadic params all land.
	wantArgs(t, syms, "Add.a", "Add.b", "Add.name", "Add.opts",
		"Server.Start.ctx", "Server.Start.retries")

	// The method RECEIVER is not a parameter — it lives on Go's
	// separate `receiver` field and must not be indexed as an argument.
	if got := symByPath(syms, "Server.Start.s"); got != nil {
		t.Errorf("receiver leaked in as an argument: %+v", got)
	}
	// A param list with no params yields no argument children.
	for _, s := range syms {
		if s.Class == "argument" && strings.HasPrefix(s.Sym, "NoParams.") {
			t.Errorf("NoParams should have no arguments, got %q", s.Sym)
		}
	}
	// Multi-name params share one parameter_declaration; their spans
	// must not overlap (each is its own identifier).
	a, b := symByPath(syms, "Add.a"), symByPath(syms, "Add.b")
	if a.DeclStartCol == b.DeclStartCol && a.DeclStartLine == b.DeclStartLine {
		t.Errorf("Add.a and Add.b have identical spans: %+v / %+v", a, b)
	}
}

func TestFileSymbolsGoArgumentsAnonymousAndDuplicate(t *testing.T) {
	// An unnamed param is anonymous ("[n]"); reuse of renderSegment
	// means duplicate names disambiguate with [n] too.
	syms, err := FileSymbols("go", []byte("package main\n\nfunc H(http.ResponseWriter) {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, syms, "H.[1]")
}

func TestFileSymbolsTypeScriptArguments(t *testing.T) {
	src := []byte(`function greet(name: string, age?: number, ...rest: any[]) {}
class C {
  method(a: string, b = 3) {}
  constructor(x: number) {}
}
`)
	syms, err := FileSymbols("typescript", src)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, syms, "greet.name", "greet.age", "greet.rest",
		"C.method.a", "C.method.b", "C.constructor.x")
}

func TestFileSymbolsTSXArguments(t *testing.T) {
	// .tsx files map to the "typescript" language name (backed by the
	// tsx grammar), so JSX-bearing content runs the same codepath.
	src := []byte(`function Comp({title, id}: Props, ref: Ref) {
  return <div>{title}</div>;
}
`)
	syms, err := FileSymbols("typescript", src)
	if err != nil {
		t.Fatal(err)
	}
	// A destructured param binds no single name → anonymous "[1]".
	wantArgs(t, syms, "Comp.[1]", "Comp.ref")
}

func TestFileSymbolsPythonArguments(t *testing.T) {
	src := []byte(`def add(a, b: int = 3, *args, **kwargs):
    pass

class C:
    def __init__(self, x: int):
        pass

    def meth(self, y):
        pass
`)
	syms, err := FileSymbols("python", src)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, syms, "add.a", "add.b", "add.args", "add.kwargs",
		"C.__init__.self", "C.__init__.x", "C.meth.self", "C.meth.y")
}

func TestFileSymbolsArgumentsNestUnderOwner(t *testing.T) {
	// An argument's sym path is always its owner's path + one segment,
	// which is what makes `.func:has(.argument#x)` / :has_parent work.
	syms, err := FileSymbols("go", []byte("package main\n\nfunc F(x int) {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	owner := symByPath(syms, "F")
	arg := symByPath(syms, "F.x")
	if owner == nil || arg == nil {
		t.Fatalf("missing F or F.x: %s", symPaths(syms))
	}
	// The argument's decl range sits inside its owner's.
	if arg.DeclStartLine < owner.DeclStartLine || arg.DeclEndLine > owner.DeclEndLine {
		t.Errorf("arg range %+v not contained in owner range %+v", arg, owner)
	}
}
