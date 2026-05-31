package symbols

import (
	"path/filepath"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
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

func TestTreeSitterTSXCapturesAllIdentifierKinds(t *testing.T) {
	ex := DefaultExtractor("typescript")
	src := []byte(`import { foo } from "./bar";

export type UserID = number;

export async function fetchUser(id: UserID): Promise<string> {
  const obj = {userId: id, shorthand};
  const c = <UserCard userId={id} />;
  return obj.shorthand;
}
`)
	got := map[string]int{}
	for _, h := range ex.Extract(src) {
		got[h.Name]++
	}
	for _, want := range []string{
		"foo",       // imported binding
		"UserID",    // type_identifier (declared + used)
		"fetchUser", // function name
		"id",        // parameter
		"Promise",   // type_identifier in return type
		"obj",       // const binding
		"userId",    // property_identifier
		"UserCard",  // JSX component (identifier)
		"shorthand", // shorthand_property_identifier
	} {
		if got[want] == 0 {
			t.Errorf("missing %q from tsx extract: %+v", want, got)
		}
	}
	// "string" and "number" are builtins — filtered.
	for _, drop := range []string{"string", "number"} {
		if got[drop] != 0 {
			t.Errorf("builtin %q leaked: %d", drop, got[drop])
		}
	}
}

func TestTreeSitterTSXSkipsStringContents(t *testing.T) {
	ex := DefaultExtractor("typescript")
	// "UserID" inside a string literal and inside a template literal must
	// NOT be indexed. The interpolated `id` IS an identifier, so it stays.
	src := []byte("const a: string = \"UserID\";\nconst b = `/api/${id}/UserID`;\n")
	for _, h := range ex.Extract(src) {
		if h.Name == "UserID" {
			t.Errorf("UserID leaked from string at line %d col %d", h.Line, h.Col)
		}
	}
}

func TestTreeSitterSQLCapturesIdentifiers(t *testing.T) {
	ex := DefaultExtractor("sql")
	src := []byte(`CREATE TABLE users (
  UserID BIGINT PRIMARY KEY,
  email TEXT NOT NULL
);

CREATE INDEX users_idx ON users (email);
`)
	got := map[string]int{}
	for _, h := range ex.Extract(src) {
		got[h.Name]++
	}
	for _, want := range []string{"users", "UserID", "email", "users_idx"} {
		if got[want] == 0 {
			t.Errorf("missing %q from sql extract: %+v", want, got)
		}
	}
	for _, drop := range []string{"CREATE", "TABLE", "BIGINT", "PRIMARY", "KEY", "TEXT", "NOT", "NULL", "INDEX", "ON"} {
		if got[drop] != 0 {
			t.Errorf("DDL keyword %q surfaced as identifier: %d", drop, got[drop])
		}
	}
}

func TestPolyglotSQLContributesUserID(t *testing.T) {
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Build(fixturePath(t, "testdata", "fixtures", "polyglot"), reg)
	if err != nil {
		t.Fatal(err)
	}
	langs := map[string]bool{}
	for _, s := range idx.Lookup("UserID") {
		langs[s.Language] = true
	}
	if !langs["sql"] {
		t.Errorf("UserID not surfaced from sql files; languages seen: %+v", langs)
	}
}

func TestTreeSitterPythonCapturesIdentifiers(t *testing.T) {
	ex := DefaultExtractor("python")
	src := []byte(`"""Module docstring with UserID inside."""

from typing import Optional

UserID = int

class UserService:
    """Class docstring with UserID."""

    def fetch(self, user_id: UserID) -> Optional[str]:
        msg = f"loaded {user_id}"
        # comment mentioning UserID should not be indexed
        return msg
`)
	got := map[string]int{}
	for _, h := range ex.Extract(src) {
		got[h.Name]++
	}
	for _, want := range []string{
		"typing", "Optional",
		"UserID",      // variable + annotation + class generic
		"UserService", // class name
		"fetch",       // method name
		"self",        // implicit param
		"user_id",     // parameter, used in f-string
		"msg",         // local, returned
	} {
		if got[want] == 0 {
			t.Errorf("missing %q from python extract: %+v", want, got)
		}
	}
	// Builtins surfaced as identifier nodes — filtered.
	for _, drop := range []string{"int", "str", "print"} {
		if got[drop] != 0 {
			t.Errorf("builtin %q leaked: %d", drop, got[drop])
		}
	}
}

func TestTreeSitterPythonSkipsStringsAndComments(t *testing.T) {
	ex := DefaultExtractor("python")
	// UserID appears inside docstring, regular string, and comment —
	// none must reach the index. The f-string's `x` IS an identifier.
	src := []byte(`"""UserID mentioned here."""
x = 1
y = "UserID is just text"
# UserID inside a comment
z = f"value={x}"
`)
	for _, h := range ex.Extract(src) {
		if h.Name == "UserID" {
			t.Errorf("UserID leaked from string/comment at %d:%d", h.Line, h.Col)
		}
	}
	got := map[string]bool{}
	for _, h := range ex.Extract(src) {
		got[h.Name] = true
	}
	if !got["x"] || !got["y"] || !got["z"] {
		t.Errorf("expected x/y/z identifiers, got %+v", got)
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
