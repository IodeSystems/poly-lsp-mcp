package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

// A declaration's doc comment is a `comment` CHILD node — the contiguous
// comment run above the decl, joined into ONE span. Go emits a comment
// node per `//` line, so the join is the whole point: `#'f' > comment`
// is the block, not its first line. Contiguity is required, so a comment
// separated from the decl by a blank line is NOT its doc.
func writeCommentFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
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
	write("go.mod", "module cmt\ngo 1.21\n")
	write("d.go", `package cmt

// Foo does the first thing.
// It has a second line.
func Foo() {}

func Bare() {}

// Detached, blank line below.

func Gap() {}
`)
	write("w.ts", `/** Renders it.
 * @param id the id */
export function render(id: string) { return id; }

export function nodoc() { return 1; }
`)
	write("p.py", `# Loads config.
# Falls back to defaults.
def load():
    return {}

def nodoc():
    return 1
`)
	return dir
}

func TestCommentNodeJoinsAndAttaches(t *testing.T) {
	s := newQueryServer(t, writeCommentFixture(t))

	// The two Go doc lines join into one node spanning both (3-4).
	foo := annNodeSet(t, s, `#'d.go#Foo' > comment`)
	if len(foo) != 1 {
		t.Fatalf("Foo should have exactly one joined comment child; got %v", foo)
	}
	e, _ := s.buildTree()
	list, _ := parseModernSelector(`#'d.go#Foo' > comment`)
	for _, r := range e.evaluate(list) {
		if r.at[0] != 3 || r.at[1] != 4 {
			t.Errorf("Foo's comment should span lines 3-4 (both doc lines joined); got %v", r.at)
		}
	}

	// TS /** */ and Python # blocks each attach to their function.
	if got := annNodeSet(t, s, `#'w.ts#render' > comment`); len(got) != 1 {
		t.Errorf("TS render should have a doc comment; got %v", got)
	}
	if got := annNodeSet(t, s, `#'p.py#load' > comment`); len(got) != 1 {
		t.Errorf("Python load should have a doc comment; got %v", got)
	}

	// Documented vs not — the structural, grep-free spelling.
	documented := annNodeSet(t, s, `func:any(comment)`)
	if len(documented) != 3 { // Foo, render, load
		t.Errorf("expected 3 documented funcs (Foo, render, load); got %v", documented)
	}

	// Contiguity: Gap's comment is separated by a blank line, so Gap is
	// NOT documented — a floating comment is not a doc block.
	undoc := map[string]bool{}
	for _, n := range annNodeSet(t, s, `func:not(:any(comment))`) {
		undoc[n] = true
	}
	if !undoc["d.go#Gap"] {
		t.Error("Gap's comment is detached by a blank line; Gap must read as undocumented")
	}
	if !undoc["d.go#Bare"] {
		t.Error("Bare has no comment at all; must read as undocumented")
	}
	if undoc["d.go#Foo"] {
		t.Error("Foo IS documented; must not appear in the undocumented set")
	}
}

// The joined comment is grep-able as one unit: a marker on the second
// doc line is found via the single comment node.
func TestCommentNodeIsGrepableAsOneBlock(t *testing.T) {
	s := newQueryServer(t, writeCommentFixture(t))
	if got := annNodeSet(t, s, `comment:contains('second line')`); len(got) != 1 {
		t.Errorf("the joined comment should match text on its second line; got %v", got)
	}
}
