package symbols

import (
	"path/filepath"
	"testing"

	"github.com/iodesystems/tslsmcp/internal/config"
)

func TestTreeSitterGoIgnoresStringsAndComments(t *testing.T) {
	ex := DefaultExtractor("go")
	if ex == nil {
		t.Fatal("no Go extractor registered")
	}
	src := []byte(`package main

// UserID is mentioned in this comment but should not be indexed.
type UserID int64

func GreetUser(id UserID) string {
	bogus := "UserID is just a string here"
	_ = bogus
	return ""
}
`)
	got := map[string]int{}
	for _, h := range ex.Extract(src) {
		got[h.Name]++
	}
	// Real identifier sites for UserID: declaration (line 4) + parameter
	// type in signature (line 6). The comment mention and the string
	// content do NOT count.
	if got["UserID"] != 2 {
		t.Errorf("UserID count = %d, want 2 (1 declaration + 1 use)", got["UserID"])
	}
	if got["bogus"] != 2 {
		t.Errorf("bogus count = %d, want 2 (declaration + use)", got["bogus"])
	}
}

func TestTreeSitterGoCapturesAllIdentifierKinds(t *testing.T) {
	ex := DefaultExtractor("go")
	src := []byte(`package pkgname

type MyStruct struct {
	FieldName int
}

func (m *MyStruct) MethodName() {}
`)
	got := map[string]bool{}
	for _, h := range ex.Extract(src) {
		got[h.Name] = true
	}
	for _, want := range []string{"pkgname", "MyStruct", "FieldName", "MethodName"} {
		if !got[want] {
			t.Errorf("missing %q from extracted identifiers: %+v", want, got)
		}
	}
}

func TestTreeSitterGoKeywordsAndBuiltinsFiltered(t *testing.T) {
	ex := DefaultExtractor("go")
	src := []byte(`package main

import "fmt"

func main() {
	var s string = "hi"
	fmt.Println(s)
}
`)
	for _, h := range ex.Extract(src) {
		switch h.Name {
		case "string", "int", "true", "false", "nil":
			t.Errorf("builtin type %q leaked: %+v", h.Name, h)
		case "package", "import", "func", "var", "return":
			// Grammar should never emit these as identifier-kind nodes.
			t.Errorf("keyword %q leaked from grammar: %+v", h.Name, h)
		}
	}
}

func TestTreeSitterGoLineColumnTracking(t *testing.T) {
	ex := DefaultExtractor("go")
	src := []byte("package main\n\nfunc Foo() {}\n")
	hits := ex.Extract(src)
	want := map[string]Hit{
		"main": {Name: "main", Line: 1, Col: 9},
		"Foo":  {Name: "Foo", Line: 3, Col: 6},
	}
	got := map[string]Hit{}
	for _, h := range hits {
		got[h.Name] = h
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("missing %q", name)
			continue
		}
		if g != w {
			t.Errorf("%q: got %+v, want %+v", name, g, w)
		}
	}
}

func TestPolyglotGoUserIDCountDroppedAfterTreeSitter(t *testing.T) {
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Build(fixturePath(t, "testdata", "fixtures", "polyglot"), reg)
	if err != nil {
		t.Fatal(err)
	}
	mainGoCount := 0
	for _, s := range idx.Lookup("UserID") {
		if filepath.Base(s.File) == "main.go" {
			mainGoCount++
		}
	}
	// main.go has UserID in a comment (line 5), the type declaration
	// (line 6), and the parameter type (line 8). Tree-sitter excludes
	// the comment, so the count is 2.
	if mainGoCount != 2 {
		t.Errorf("UserID sites in main.go = %d, want 2 (declaration + use; comment excluded)", mainGoCount)
	}
}
