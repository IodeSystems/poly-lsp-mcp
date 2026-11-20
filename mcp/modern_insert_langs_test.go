package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Spec 4 for the node_edit insert op: the splice point is each language's
// declaration boundary, and after an after/before insert the file must parse
// with the new symbol present and resolvable. The parse proof is the index:
// node_query finds the new symbol and node_read resolves it, the same proof
// the Go reference test uses (TestModernNodeEditInsertAfter in
// modern_test.go). No compiler is invoked.
//
// This file owns ONLY the per-language boundary coverage (go, ts, py, java,
// kotlin). Refusal and additivity live in modern_test.go.
//
// go and ts use the files startModern already writes (main.go,
// web/some_file.ts); py, java and kotlin files are added here. ts and java
// use a class-member anchor for their before-insert: a whole-class anchor is
// not a short splice point.

const insertPyMain = `def alpha(x: int) -> int:
    return x

class Beta:
    def value(self) -> int:
        return 1
`

const insertJavaWidget = `package lib;

public class Widget {
  public int size() { return 1; }
}
`

const insertKotlinNote = `package note

class Note {
  fun open(): String = "open"
}
`

// insertLangsWorkspace boots the default modern surface on the go + ts files
// from startModern plus a python, java and kotlin file.
func insertLangsWorkspace(t *testing.T) (*mcpSession, string) {
	t.Helper()
	s, dir := startModern(t)
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("py/main.py", insertPyMain)
	write("lib/Widget.java", insertJavaWidget)
	write("note/Note.kt", insertKotlinNote)
	return s, dir
}

// insertCase is one language's boundary. anchor is the address the insert op
// targets; newAddr is the new symbol's full address; boundary is a substring
// of the anchor's declaration that must survive the edit unchanged.
type insertCase struct {
	name, anchor, insert, newDecl, newAddr, file, boundary string
}

// insertAndAssert drives one insert op and checks the three spec-4
// obligations: the splice landed (the new declaration is in the file), the
// anchor's declaration is unchanged, and the file now parses with the new
// symbol present and resolvable in the index.
func insertAndAssert(t *testing.T, s *mcpSession, dir string, tc insertCase) {
	t.Helper()
	if tc.file == "" {
		tc.file = strings.SplitN(tc.anchor, "#", 2)[0]
	}

	r := s.callTool("node_edit", map[string]any{
		"node": tc.anchor, "insert": tc.insert, "newText": tc.newDecl,
	})
	if r.IsError {
		t.Fatalf("node_edit insert:%q %s errored: %s", tc.insert, tc.anchor, r.Content[0].Text)
	}

	src, err := os.ReadFile(filepath.Join(dir, tc.file))
	if err != nil {
		t.Fatal(err)
	}
	// The new declaration must be in the file.
	if !strings.Contains(string(src), tc.newDecl) {
		t.Errorf("%s: new declaration not in file after insert.\nWant:\n  %q\nGot:\n%s",
			tc.name, tc.newDecl, src)
	}
	// The anchor's declaration must be unchanged.
	if !strings.Contains(string(src), tc.boundary) {
		t.Errorf("%s: anchor declaration was modified.\nWant substring:\n  %q\nGot:\n%s",
			tc.name, tc.boundary, src)
	}

	// The parse proof: the new symbol is in the index and resolvable.
	q := s.callTool("node_query", map[string]any{"selector": "#" + strings.SplitN(tc.newAddr, "#", 2)[1]})
	if q.IsError {
		t.Fatalf("%s: query for the new symbol errored: %s", tc.name, q.Content[0].Text)
	}
	if !strings.Contains(q.Content[0].Text, tc.newAddr) {
		t.Errorf("%s: index did not find the new symbol %s:\n%s", tc.name, tc.newAddr, q.Content[0].Text)
	}
	nr := s.callTool("node_read", map[string]any{"node": tc.newAddr})
	if nr.IsError {
		t.Fatalf("%s: node_read of the new symbol %s errored: %s", tc.name, tc.newAddr, nr.Content[0].Text)
	}
}

func TestInsertOpAtLanguageBoundaries(t *testing.T) {
	s, dir := insertLangsWorkspace(t)
	defer s.close()

	cases := []insertCase{
		// GO: top-level function boundary (the reference language).
		{
			name:     "go/after",
			anchor:   "main.go#Free",
			insert:   "after",
			newDecl:  "func NewFunc(x int) int { return x }",
			newAddr:  "main.go#NewFunc",
			boundary: "func Free(only int) {}",
		},
		{
			name:     "go/before",
			anchor:   "main.go#CallsStart",
			insert:   "before",
			newDecl:  "func NewFn() {}",
			newAddr:  "main.go#NewFn",
			boundary: "func CallsStart() {",
		},
		// TYPESCRIPT: top-level function boundary, then a class-member boundary.
		{
			name:     "ts/after",
			anchor:   "web/some_file.ts#topLevel",
			insert:   "after",
			newDecl:  "export function beta(x: number) { return x; }",
			newAddr:  "web/some_file.ts#beta",
			boundary: "export function topLevel(alpha: number) {",
		},
		{
			name:     "ts/before",
			anchor:   "web/some_file.ts#UserService.getUser",
			insert:   "before",
			newDecl:  "  describe(): string { return 'x'; }",
			newAddr:  "web/some_file.ts#UserService.describe",
			boundary: "getUser(id: string, verbose: boolean) { return id; }",
		},
		// PYTHON: top-level function boundary, then a class-member boundary.
		{
			name:     "py/after",
			anchor:   "py/main.py#alpha",
			insert:   "after",
			newDecl:  "def omega(x: int) -> int:\n    return x + 1",
			newAddr:  "py/main.py#omega",
			boundary: "def alpha(x: int) -> int:",
		},
		{
			name:     "py/before",
			anchor:   "py/main.py#Beta.value",
			insert:   "before",
			newDecl:  "    def name(self) -> str:\n        return 'beta'",
			newAddr:  "py/main.py#Beta.name",
			boundary: "def value(self) -> int:",
		},
		// JAVA: a class-member boundary for both directions.
		{
			name:     "java/after",
			anchor:   "lib/Widget.java#Widget.size",
			insert:   "after",
			newDecl:  "  public int area() { return 1; }",
			newAddr:  "lib/Widget.java#Widget.area",
			boundary: "public int size() { return 1; }",
		},
		{
			name:     "java/before",
			anchor:   "lib/Widget.java#Widget.size",
			insert:   "before",
			newDecl:  "  public String label() { return \"widget\"; }",
			newAddr:  "lib/Widget.java#Widget.label",
			boundary: "public int size() { return 1; }",
		},
		// KOTLIN: a class-member boundary for both directions.
		{
			name:     "kotlin/after",
			anchor:   "note/Note.kt#Note.open",
			insert:   "after",
			newDecl:  "  fun closed(): Boolean = false",
			newAddr:  "note/Note.kt#Note.closed",
			boundary: "fun open(): String = \"open\"",
		},
		{
			name:     "kotlin/before",
			anchor:   "note/Note.kt#Note.open",
			insert:   "before",
			newDecl:  "  fun fresh(): Boolean = true",
			newAddr:  "note/Note.kt#Note.fresh",
			boundary: "fun open(): String = \"open\"",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			insertAndAssert(t, s, dir, tc)
		})
	}
}
