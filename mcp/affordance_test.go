package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two hints that pointed at nothing. Both were correct advice naming a thing
// the caller could not reach, which is worse than no advice: the model spends
// calls looking for it, then falls back to whatever IS actionable — paging a
// file one window at a time, or rewriting a whole file to insert one function.

// The read hint's "don't page, search instead" clause has to name a call
// the caller can actually make. On the modern surface it used to name the
// classic `search` tool (pattern=<regex>), which is not on that surface —
// leaving "call again with startLine=N" as the only actionable advice, i.e.
// the chunk-by-chunk paging the hint exists to prevent.
func TestReadHintNamesASearchThisSurfaceHas(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	// A file long enough to trip the auto char cap.
	var big strings.Builder
	for i := 0; i < 400; i++ {
		big.WriteString("// a reasonably long filler line so the char budget fires quickly\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "big.go"), []byte(big.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	m, isErr := readNode(t, s, map[string]any{"node": "big.go"})
	if isErr {
		t.Fatalf("read errored: %+v", m)
	}
	hint, _ := m["hint"].(string)
	if hint == "" {
		t.Fatalf("a truncated read should carry a hint; got %+v", m)
	}
	if strings.Contains(hint, "search tool") || strings.Contains(hint, "pattern=<regex>") {
		t.Errorf("the modern surface has no `search` tool; hint = %q", hint)
	}
	if !strings.Contains(hint, `node_query(selector: "path=big.go ::grep(`) {
		t.Errorf("the hint should spell a runnable node_query over THIS file; hint = %q", hint)
	}
}

// Addressing a symbol that isn't there yet is as often "add it" as "typo".
// The error answered only the typo, so a model that meant to insert had
// nowhere to go and rewrote the whole file instead.
func TestMissingSymbolErrorTeachesHowToAddOne(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	r := s.callTool("node_edit", map[string]any{
		"node": "main.go#NotThereYet", "newText": "func NotThereYet() {}",
	})
	if !r.IsError {
		t.Fatal("newText alone on a missing SYMBOL should not silently create a file")
	}
	msg := r.Content[0].Text
	// Both readings: the typo (did-you-mean) and the insert (the idiom).
	for _, want := range []string{"did you mean", "To ADD it instead", "NEIGHBOUR", "oldText"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error should cover both readings (%q missing); got %q", want, msg)
		}
	}
}

// A truncated read is usually a "what's in here?" that had to be spelled as a
// read. The outline answers it in the SAME response — the observed sequence was
// node_read(tui.go) → lines 1-56 of 2694 → node_query(path=… func) →
// node_query(path=… type), two calls to rebuild what the first could carry.
func TestTruncatedReadCarriesAnOutline(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	var big strings.Builder
	big.WriteString("package big\n\nimport \"fmt\"\n\ntype T struct{ A, B int }\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&big, "\n// filler doc line for f%d, long enough to spend the char budget\nfunc f%d() { fmt.Println(%d) }\n", i, i, i)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.go"), []byte(big.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	m, isErr := readNode(t, s, map[string]any{"node": "big.go"})
	if isErr {
		t.Fatalf("read errored: %+v", m)
	}
	outline, _ := m["outline"].(string)
	if outline == "" {
		t.Fatalf("a truncated read of a structured file should carry an outline; got %+v", m)
	}
	if !strings.Contains(outline, "60 func") {
		t.Errorf("the outline should count every func, nested included; got %q", outline)
	}
	// Biggest first — that is the shape of the file.
	if !strings.HasPrefix(outline, "60 func") {
		t.Errorf("classes should be ordered by count; got %q", outline)
	}
	// Signature detail is not content: it would outnumber the declarations.
	for _, noise := range []string{"argument", "return", "module"} {
		if strings.Contains(outline, noise) {
			t.Errorf("%s should not be counted; got %q", noise, outline)
		}
	}
	// The hint must hand back a call whose numbers match the outline.
	hint, _ := m["hint"].(string)
	if !strings.Contains(hint, `node_query(selector: "path=big.go func")`) {
		t.Errorf("the hint should name this file's biggest kind as a runnable call; got %q", hint)
	}
}

// The counts have to agree with the selector the hint hands back. `path=F <tag>`
// is the DESCENDANT form and returns nested symbols; a child-only tour
// (#'F' > *) would not, because Go methods nest under their receiver type. A
// hint whose numbers disagree with its own call is the failure this whole line
// of work is about.
func TestOutlineCountsMatchTheSelectorItSuggests(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	src := "package m\n\ntype S struct{ X int }\n\nfunc (s S) A() {}\nfunc (s S) B() {}\nfunc top() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "m.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got := fileOutline("go", []byte(src))
	if !strings.Contains(got, "2 method") {
		t.Fatalf("methods nest under their receiver but are still the file's content; got %q", got)
	}
	// And the selector that outline implies returns exactly that many.
	q := query(t, s, map[string]any{"selector": "path=m.go method"})
	if q.TotalMatches != 2 {
		t.Errorf("path=m.go method should return the 2 the outline counted; got %d", q.TotalMatches)
	}
}

// An outline that says nothing costs tokens for nothing. A long file with one
// or two declarations is a data blob or a table — ::grep is the only door, and
// the payload should not pretend otherwise.
func TestOutlineIsOmittedWhenThereIsNoStructure(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	var blob strings.Builder
	blob.WriteString("package blob\n\nvar Table = []string{\n")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&blob, "\t\"row %d — a reasonably long entry so the char budget fires\",\n", i)
	}
	blob.WriteString("}\n")
	if err := os.WriteFile(filepath.Join(dir, "blob.go"), []byte(blob.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	m, _ := readNode(t, s, map[string]any{"node": "blob.go"})
	if _, ok := m["outline"]; ok {
		t.Errorf("a file with almost no declarations should not carry an outline; got %+v", m["outline"])
	}
	if hint, _ := m["hint"].(string); strings.Contains(hint, "narrow by kind") {
		t.Errorf("and the hint should not offer to narrow by kind; got %q", hint)
	}
	if hint, _ := m["hint"].(string); !strings.Contains(hint, "::grep") {
		t.Errorf("::grep is the remaining door and must still be offered; got %q", hint)
	}
}
