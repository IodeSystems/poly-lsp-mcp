package symbols

import "testing"

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
