package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

// A declaration's doc comment is the `::comment` pseudo-element — the
// contiguous comment run above the decl, joined into one node, GENERATED
// on demand and invisible to `*` (like ::grep). Go emits a comment node
// per `//` line, so the join is the point: ::comment is the block, not
// its first line. Contiguity is required — a comment separated from the
// decl by a blank line is not its doc.
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
func Foo(x int) {}

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

func TestCommentPseudoElementJoinsAndAttaches(t *testing.T) {
	s := newQueryServer(t, writeCommentFixture(t))

	// The two Go doc lines join into one node spanning both (3-4),
	// addressed at the block's first line.
	e, _ := s.buildTree()
	list, _ := parseModernSelector(`#'d.go#Foo'::comment`)
	rows := e.evaluate(list)
	if len(rows) != 1 {
		t.Fatalf("Foo should have one joined ::comment; got %d", len(rows))
	}
	if rows[0].at[0] != 3 || rows[0].at[1] != 4 {
		t.Errorf("Foo's comment should span lines 3-4 (both doc lines joined); got %v", rows[0].at)
	}
	if rows[0].addr() != "d.go@3" {
		t.Errorf("a generated comment addresses its first line; got %s", rows[0].addr())
	}

	// TS /** */ and Python # blocks each attach to their function.
	if got := annNodeSet(t, s, `#'w.ts#render'::comment`); len(got) != 1 {
		t.Errorf("TS render should have a doc comment; got %v", got)
	}
	if got := annNodeSet(t, s, `#'p.py#load'::comment`); len(got) != 1 {
		t.Errorf("Python load should have a doc comment; got %v", got)
	}

	// Documented vs not — the structural, grep-free spelling.
	if got := annNodeSet(t, s, `func:any(::comment)`); len(got) != 3 { // Foo, render, load
		t.Errorf("expected 3 documented funcs; got %v", got)
	}
	undoc := map[string]bool{}
	for _, n := range annNodeSet(t, s, `func:not(:any(::comment))`) {
		undoc[n] = true
	}
	if !undoc["d.go#Gap"] {
		t.Error("Gap's comment is detached by a blank line; Gap must read as undocumented")
	}
	if !undoc["d.go#Bare"] {
		t.Error("Bare has no comment; must read as undocumented")
	}
	if undoc["d.go#Foo"] {
		t.Error("Foo IS documented; must not be in the undocumented set")
	}
}

// The pseudo-element contract: ::comment is GENERATED, so `*` and the
// containment walk never see it. Foo has both a doc comment and an
// argument; `> *` must return the argument and NOT the comment.
func TestCommentIsInvisibleToStar(t *testing.T) {
	s := newQueryServer(t, writeCommentFixture(t))
	for _, n := range annNodeSet(t, s, `#'d.go#Foo' > *`) {
		if n == "d.go@3" || n == "d.go#Foo.comment" {
			t.Errorf("::comment leaked into `> *`: %s — a pseudo-element must be invisible", n)
		}
	}
	// It IS reachable by naming it.
	if got := annNodeSet(t, s, `#'d.go#Foo'::comment`); len(got) != 1 {
		t.Errorf("::comment must be reachable when named; got %v", got)
	}
	// And it is no longer a tag: `> comment` matches nothing.
	if got := annNodeSet(t, s, `#'d.go#Foo' > comment`); len(got) != 0 {
		t.Errorf("comment is a pseudo-element now, not a tag; `> comment` got %v", got)
	}
}

// The joined comment is grep-able as one unit.
func TestCommentGrepableAsOneBlock(t *testing.T) {
	s := newQueryServer(t, writeCommentFixture(t))
	if got := annNodeSet(t, s, `::comment:contains('second line')`); len(got) != 1 {
		t.Errorf("the joined comment should match text on its second line; got %v", got)
	}
}
