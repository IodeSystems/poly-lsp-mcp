package symbols

import "testing"

func TestStructureNodesGoTopLevel(t *testing.T) {
	src := []byte(`package main

import "fmt"

type UserID int64

func GreetUser(id UserID) string {
	return fmt.Sprintf("hi %d", id)
}

func main() {
	GreetUser(1)
}
`)
	got, err := StructureNodes("go", src)
	if err != nil {
		t.Fatal(err)
	}
	// Expect: package_clause, import_declaration, type_declaration,
	// function_declaration, function_declaration.
	wantTypes := []string{
		"package_clause",
		"import_declaration",
		"type_declaration",
		"function_declaration",
		"function_declaration",
	}
	if len(got) != len(wantTypes) {
		t.Fatalf("got %d nodes, want %d: %+v", len(got), len(wantTypes), got)
	}
	for i, w := range wantTypes {
		if got[i].Type != w {
			t.Errorf("[%d].Type = %q, want %q", i, got[i].Type, w)
		}
	}
	// type_declaration should have name "UserID"
	if got[2].Name != "UserID" {
		t.Errorf("[2].Name = %q, want UserID", got[2].Name)
	}
	if got[3].Name != "GreetUser" {
		t.Errorf("[3].Name = %q, want GreetUser", got[3].Name)
	}
	if got[4].Name != "main" {
		t.Errorf("[4].Name = %q, want main", got[4].Name)
	}
	// Ranges should be sensible (1-based, end-exclusive).
	for _, n := range got {
		if n.StartLine < 1 || n.StartCol < 1 || n.EndLine < n.StartLine {
			t.Errorf("bad range on %+v", n)
		}
	}
}

func TestStructureNodesTypeScript(t *testing.T) {
	src := []byte(`export type UserID = number;

export function fetchUser(id: UserID): Promise<string> {
  return Promise.resolve("ok");
}

export class UserService {
  async getUser(id: UserID) {
    return await fetchUser(id);
  }
}
`)
	got, err := StructureNodes("typescript", src)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, n := range got {
		names[n.Name] = n.Type
	}
	for _, want := range []string{"UserID", "fetchUser", "UserService"} {
		if _, ok := names[want]; !ok {
			t.Errorf("missing %q from extracted structure: %+v", want, got)
		}
	}
}

func TestStructureNodesPython(t *testing.T) {
	src := []byte(`from typing import Optional

UserID = int

def process(user_id: UserID) -> Optional[str]:
    return None

class UserService:
    def fetch(self, user_id: UserID):
        return None
`)
	got, err := StructureNodes("python", src)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, n := range got {
		names[n.Name] = true
	}
	for _, want := range []string{"process", "UserService"} {
		if !names[want] {
			t.Errorf("missing %q from python structure: %+v", want, got)
		}
	}
}

func TestStructureNodesUnsupportedLanguage(t *testing.T) {
	_, err := StructureNodes("markdown", []byte("# hello"))
	if err == nil {
		t.Error("expected error for unsupported language")
	}
}

func TestEnclosingStructureNodeReturnsContainingDecl(t *testing.T) {
	src := []byte("package main\n\nfunc Greet() {\n\tfoo()\n}\n\nfunc Other() {}\n")
	// Position inside Greet's body (line 4, "foo()").
	got, err := EnclosingStructureNode("go", src, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("nil node")
	}
	if got.Name != "Greet" {
		t.Errorf("Name = %q, want Greet", got.Name)
	}
	if got.Type != "function_declaration" {
		t.Errorf("Type = %q, want function_declaration", got.Type)
	}
	if got.StartLine != 3 || got.EndLine < 5 {
		t.Errorf("range = (%d:%d-%d:%d), want lines 3..5", got.StartLine, got.StartCol, got.EndLine, got.EndCol)
	}
}

func TestEnclosingStructureNodePositionAtIdentifier(t *testing.T) {
	src := []byte("package main\n\nfunc Hello() {}\n")
	// Position on the H of Hello (line 3, col 6).
	got, err := EnclosingStructureNode("go", src, 3, 6)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Name != "Hello" {
		t.Errorf("got %+v, want enclosing Hello", got)
	}
}

func TestEnclosingStructureNodeUnsupported(t *testing.T) {
	_, err := EnclosingStructureNode("markdown", []byte("hi"), 1, 1)
	if err == nil {
		t.Error("expected error for unsupported language")
	}
}
