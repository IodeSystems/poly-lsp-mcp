package mcp

import "testing"

func TestParseConflicts_TwoSides(t *testing.T) {
	src := []byte("package p\n" +
		"<<<<<<< HEAD\n" +
		"func Ours() {}\n" +
		"=======\n" +
		"func Theirs() {}\n" +
		">>>>>>> e0cfa59 (feat: x)\n" +
		"func After() {}\n")
	cs := parseConflicts(src)
	if len(cs) != 1 {
		t.Fatalf("want 1 conflict, got %d", len(cs))
	}
	c := cs[0]
	// The span covers the MARKERS too, so replacing it resolves the block.
	if c.at != [2]int{2, 6} {
		t.Errorf("span should cover marker to marker; got %v", c.at)
	}
	if c.ours.text() != "func Ours() {}" || c.theirs.text() != "func Theirs() {}" {
		t.Errorf("sides wrong: ours=%q theirs=%q", c.ours.text(), c.theirs.text())
	}
	// Provenance: the label is the only thing that says WHICH commit a side
	// is, and under a rebase it is the only way to tell whose work it is.
	if c.ours.label != "HEAD" || c.theirs.label != "e0cfa59 (feat: x)" {
		t.Errorf("labels wrong: ours=%q theirs=%q", c.ours.label, c.theirs.label)
	}
	if c.base != nil {
		t.Error("no diff3 base was written, so none should be reported")
	}
}

// diff3 style adds the common ancestor. It is what makes a three-way choice
// possible, so it must survive parsing rather than be folded into a side.
func TestParseConflicts_Diff3Base(t *testing.T) {
	src := []byte("<<<<<<< HEAD\nA\n||||||| merged common ancestors\nBASE\n=======\nB\n>>>>>>> other\n")
	cs := parseConflicts(src)
	if len(cs) != 1 || cs[0].base == nil {
		t.Fatalf("want one conflict with a base; got %+v", cs)
	}
	if cs[0].base.text() != "BASE" || cs[0].ours.text() != "A" || cs[0].theirs.text() != "B" {
		t.Errorf("three-way split wrong: %+v", cs[0])
	}
}

// An empty side is normal — one alternative deletes what the other adds.
func TestParseConflicts_EmptySide(t *testing.T) {
	cs := parseConflicts([]byte("<<<<<<< HEAD\n=======\nadded\n>>>>>>> b\n"))
	if len(cs) != 1 {
		t.Fatalf("want 1, got %d", len(cs))
	}
	if len(cs[0].ours.lines) != 0 || cs[0].theirs.text() != "added" {
		t.Errorf("empty ours should stay empty: %+v", cs[0])
	}
}

func TestParseConflicts_Multiple(t *testing.T) {
	src := []byte("a\n<<<<<<< HEAD\n1\n=======\n2\n>>>>>>> b\nmid\n<<<<<<< HEAD\n3\n=======\n4\n>>>>>>> b\nz\n")
	if cs := parseConflicts(src); len(cs) != 2 {
		t.Fatalf("want 2 conflicts, got %d", len(cs))
	}
}

// Claiming a conflict that is not there would make a clean file unreadable,
// so anything unpaired is dropped rather than swallowed.
func TestParseConflicts_QuietOnNonConflicts(t *testing.T) {
	for name, src := range map[string]string{
		"clean file":          "package p\n\nfunc A() {}\n",
		"markdown rule":       "Title\n=======\nbody\n",
		"unterminated opener": "<<<<<<< HEAD\nstuff\nmore\n",
		"longer run":          "========\n>>>>>>>>\n",
		"split with no open":  "a\n=======\nb\n>>>>>>> x\n",
	} {
		if cs := parseConflicts([]byte(src)); len(cs) != 0 {
			t.Errorf("%s: should report no conflict, got %+v", name, cs)
		}
	}
}
