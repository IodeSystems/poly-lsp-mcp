package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A conflict that opens in one function and closes in another. tree-sitter
// recovers past the markers and stitches a declaration out of BOTH sides:
// here `B[1]` is ours' header, a `=======` marker, and theirs' tail — source
// from no commit and no branch, which is not valid Go in any of them.
const straddleSrc = `package m

func A() int {
	x := 1
<<<<<<< HEAD
	return x
}

func B() int {
	z := 3
=======
	return x + 1
}

func B() int {
	z := 4
>>>>>>> feature/y
	return z
}
`

func startStraddle(t *testing.T) (*mcpSession, string) {
	t.Helper()
	s, dir := startModern(t)
	if err := os.WriteFile(filepath.Join(dir, "m.go"), []byte(straddleSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	return s, dir
}

// Reading a chimera is misleading; EDITING one writes across the markers and
// corrupts the merge. There is no correct oldText for a span that is half of
// each side, so this is refused rather than warned about.
func TestEditRefusedOnASpanStraddlingAConflict(t *testing.T) {
	s, dir := startStraddle(t)
	defer s.close()

	before, _ := os.ReadFile(filepath.Join(dir, "m.go"))
	r := s.callTool("node_edit", map[string]any{
		"node": "m.go#B[1]", "oldText": "z := 3", "newText": "z := 9",
	})
	if !r.IsError {
		t.Fatal("editing across a conflict marker must be refused")
	}
	msg := r.Content[0].Text
	for _, want := range []string{"straddles", "part MINE and part THEIRS", `accept:"mine"`, "::theirs"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal should explain and offer a way out (%q missing): %s", want, msg)
		}
	}
	// The suggested address must be runnable as printed.
	if !strings.Contains(msg, "m.go@5-17") {
		t.Errorf("the suggested conflict address needs its file; got %s", msg)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "m.go"))
	if string(before) != string(after) {
		t.Error("a refused edit must not touch the file")
	}
}

// The guard is about STRADDLING, not proximity. A node that wholly CONTAINS a
// conflict is a legitimate target — accept: and a whole-node rewrite both
// work on it — and a node nowhere near one is untouched by any of this.
func TestEditStillWorksAroundAndAwayFromConflicts(t *testing.T) {
	s, dir := startStraddle(t)
	defer s.close()

	// A clean file in the same workspace is unaffected.
	if r := s.callTool("node_edit", map[string]any{
		"node": "main.go#Free", "oldText": "func Free(only int) {}", "newText": "func Free(only, extra int) {}",
	}); r.IsError {
		t.Errorf("an unrelated file must still be editable: %s", r.Content[0].Text)
	}
	// The whole conflicted FILE is addressable — that is how accept works.
	if r := s.callTool("node_edit", map[string]any{"node": "m.go", "accept": "mine"}); r.IsError {
		t.Errorf("accept on the conflicted file must work: %s", r.Content[0].Text)
	}
	// And once resolved, ordinary edits come back.
	got, _ := os.ReadFile(filepath.Join(dir, "m.go"))
	if strings.Contains(string(got), "<<<<<<<") {
		t.Fatalf("accept should have resolved the file:\n%s", got)
	}
	if r := s.callTool("node_edit", map[string]any{
		"node": "m.go#A", "oldText": "x := 1", "newText": "x := 2",
	}); r.IsError {
		t.Errorf("after resolving, a normal edit should succeed: %s", r.Content[0].Text)
	}
}

// Two conflicts, one crossing a function boundary and one wholly inside a
// function, is the shape a real messy merge takes. Each is its own region and
// each is separately resolvable.
func TestTwoConflictsOneCrossingOneInside(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	src := straddleSrc[:len(straddleSrc)-len("\treturn z\n}\n")] +
		"\tw := z\n<<<<<<< HEAD\n\treturn w\n=======\n\treturn w * 2\n>>>>>>> feature/y\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "m.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	q := query(t, s, map[string]any{"selector": "#'m.go'::conflict", "limit": 10})
	if len(q.Matches) != 2 {
		t.Fatalf("want two regions; got %d (%+v)", len(q.Matches), q.Matches)
	}
	// Resolve only the inner one; the straddling one must remain.
	inner := q.Matches[1].Node
	if r := s.callTool("node_edit", map[string]any{"node": inner, "accept": "theirs"}); r.IsError {
		t.Fatalf("resolving one region errored: %s", r.Content[0].Text)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "m.go"))
	if !strings.Contains(string(got), "return w * 2") {
		t.Errorf("the inner conflict should be resolved to theirs:\n%s", got)
	}
	if !strings.Contains(string(got), "<<<<<<< HEAD") {
		t.Errorf("the straddling conflict must remain:\n%s", got)
	}
}
