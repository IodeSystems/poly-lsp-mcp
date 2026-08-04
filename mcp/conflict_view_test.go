package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A side alone is usually a syntactic FRAGMENT — a conflict opening in one
// function and closing in another leaves each side holding half a
// declaration. The whole file WITH that side is what git writes on
// --ours/--theirs, and it parses. That is the difference between a version
// and a snippet, and it is why reconstruction is whole-file.
func TestSideContentRebuildsAWholeParseableFile(t *testing.T) {
	src := []byte("package m\n\nfunc A() int {\n\tx := 1\n<<<<<<< HEAD\n\treturn x\n}\n\nfunc B() int {\n\tz := 3\n=======\n\treturn x + 1\n}\n\nfunc B() int {\n\tz := 4\n>>>>>>> y\n\treturn z\n}\n")
	mine, theirs := sideContent(src, "mine"), sideContent(src, "theirs")

	for name, got := range map[string][]byte{"mine": mine, "theirs": theirs} {
		if strings.Contains(string(got), "<<<<<<<") || strings.Contains(string(got), "=======") {
			t.Errorf("%s still holds markers:\n%s", name, got)
		}
		if !strings.Contains(string(got), "func A() int {") || !strings.Contains(string(got), "func B() int {") {
			t.Errorf("%s should be a whole file, not the region:\n%s", name, got)
		}
	}
	if !strings.Contains(string(mine), "return x\n") || strings.Contains(string(mine), "return x + 1") {
		t.Errorf("mine took the wrong side:\n%s", mine)
	}
	if !strings.Contains(string(theirs), "return x + 1") {
		t.Errorf("theirs took the wrong side:\n%s", theirs)
	}
	// A clean file reconstructs to itself.
	clean := []byte("package m\n\nfunc A() {}\n")
	if string(sideContent(clean, "mine")) != string(clean) {
		t.Error("a file with no conflicts must be returned unchanged")
	}
}

// BEST EFFORT: when both sides reconstruct into parseable source, the
// structural view is real and no diff is needed.
func TestViewIsStructuralWhenSidesParse(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	src := "package m\n\nfunc A() int {\n\tx := 1\n<<<<<<< HEAD\n\treturn x\n}\n\nfunc B() int {\n\tz := 3\n=======\n\treturn x + 1\n}\n\nfunc B() int {\n\tz := 4\n>>>>>>> y\n\treturn z\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "ok.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	q := query(t, s, map[string]any{"selector": "#'ok.go'::conflict", "limit": 3})
	if len(q.Matches) != 1 {
		t.Fatalf("want one region; got %+v", q.Matches)
	}
	m := q.Matches[0]
	if !m.MineParses || !m.TheirsParses {
		t.Errorf("both reconstructions should parse; got mine=%v theirs=%v", m.MineParses, m.TheirsParses)
	}
	if m.Diff != "" {
		t.Errorf("no diff fallback is needed when a side parses; got %q", m.Diff)
	}
}

// And when NEITHER side reconstructs — a conflict inside an unterminated
// string, a half-written branch — there is no honest tree to show. A
// structural answer would be invented, so fall back to what is always true:
// the two texts and their difference.
func TestViewFallsBackToDiffWhenNeitherSideParses(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	src := "package m\n\nvar s = \"text\n<<<<<<< HEAD\nours unterminated\n=======\ntheirs also broken\n>>>>>>> y\n"
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	q := query(t, s, map[string]any{"selector": "#'bad.go'::conflict", "limit": 3})
	if len(q.Matches) != 1 {
		t.Fatalf("want one region; got %+v", q.Matches)
	}
	m := q.Matches[0]
	if m.MineParses || m.TheirsParses {
		t.Fatalf("fixture drift: neither side should parse; got mine=%v theirs=%v", m.MineParses, m.TheirsParses)
	}
	if m.Diff == "" {
		t.Fatal("with no parseable side the region must be rendered as diffed TEXT")
	}
	if !strings.Contains(m.Diff, "- ours unterminated") || !strings.Contains(m.Diff, "+ theirs also broken") {
		t.Errorf("the diff should mark mine with - and theirs with +; got:\n%s", m.Diff)
	}
	// And it must SAY the structural view is unavailable, not imply one.
	if !strings.Contains(m.Note, "neither side") {
		t.Errorf("the note must explain why this is text; got %q", m.Note)
	}
}

// A one-sided failure is still usable — the common shape when one branch is
// mid-edit — but the caller has to be told which half to distrust.
func TestViewNamesTheSideThatDoesNotParse(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	src := "package m\n\n<<<<<<< HEAD\nfunc A() {}\n=======\nfunc A( {\n>>>>>>> y\n"
	if err := os.WriteFile(filepath.Join(dir, "half.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	m := query(t, s, map[string]any{"selector": "#'half.go'::conflict", "limit": 3}).Matches[0]
	if !m.MineParses || m.TheirsParses {
		t.Fatalf("fixture drift: mine should parse and theirs should not; got %v/%v", m.MineParses, m.TheirsParses)
	}
	if m.Diff != "" {
		t.Errorf("one good side is enough for a structural view; got a diff:\n%s", m.Diff)
	}
	if !strings.Contains(m.Note, "theirs does not parse") {
		t.Errorf("the note must name the unreliable side; got %q", m.Note)
	}
}

// The diff keeps common lines common — a whole block reading as replaced
// would hide the one line that actually differs.
func TestLineDiffKeepsUnchangedLines(t *testing.T) {
	got := lineDiff([]string{"a", "b", "c"}, []string{"a", "B", "c"})
	want := "  a\n- b\n+ B\n  c"
	if got != want {
		t.Errorf("lineDiff =\n%s\nwant\n%s", got, want)
	}
	if got := lineDiff(nil, []string{"only theirs"}); got != "+ only theirs" {
		t.Errorf("an empty side should render as pure addition; got %q", got)
	}
}
