package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const conflictedSrc = `package main

func Before() int { return 1 }

<<<<<<< HEAD
func Pick() string { return "ours" }
=======
func Pick() string { return "theirs" }
>>>>>>> e0cfa59 (feat: x)

func After() int { return 2 }
`

func writeConflicted(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(conflictedSrc), 0o644); err != nil {
		t.Fatal(err)
	}
}

// accept keeps one side and takes the MARKERS with it — a resolve that left
// them behind would leave the file just as unbuildable as before.
func TestAcceptResolvesAndDropsMarkers(t *testing.T) {
	for _, tc := range []struct{ accept, want string }{
		{"mine", "ours"}, {"ours", "ours"}, {"theirs", "theirs"}, {"THEIRS", "theirs"},
	} {
		t.Run(tc.accept, func(t *testing.T) {
			s, dir := startModern(t)
			defer s.close()
			writeConflicted(t, dir, "c.go")

			r := s.callTool("node_edit", map[string]any{"node": "c.go", "accept": tc.accept})
			if r.IsError {
				t.Fatalf("accept %q errored: %s", tc.accept, r.Content[0].Text)
			}
			got, err := os.ReadFile(filepath.Join(dir, "c.go"))
			if err != nil {
				t.Fatal(err)
			}
			out := string(got)
			for _, marker := range []string{"<<<<<<<", "=======", ">>>>>>>"} {
				if strings.Contains(out, marker) {
					t.Errorf("marker %q survived the resolve:\n%s", marker, out)
				}
			}
			if !strings.Contains(out, `return "`+tc.want+`"`) {
				t.Errorf("accept %q should keep the %s side:\n%s", tc.accept, tc.want, out)
			}
			other := "theirs"
			if tc.want == "theirs" {
				other = "ours"
			}
			if strings.Contains(out, `return "`+other+`"`) {
				t.Errorf("the %s side should be gone:\n%s", other, out)
			}
			// Everything outside the conflict is untouched.
			if !strings.Contains(out, "func Before() int { return 1 }") ||
				!strings.Contains(out, "func After() int { return 2 }") {
				t.Errorf("surrounding code must survive:\n%s", out)
			}
		})
	}
}

// A file with no conflict must say so rather than silently rewriting itself.
func TestAcceptOnACleanFileIsAnError(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	r := s.callTool("node_edit", map[string]any{"node": "main.go", "accept": "mine"})
	if !r.IsError {
		t.Fatal("accept on a clean file should be refused")
	}
	if !strings.Contains(r.Content[0].Text, "no merge conflict") {
		t.Errorf("the error should name the reason; got %s", r.Content[0].Text)
	}
}

// An unknown side is refused — "accept: latest" silently picking one would be
// the worst possible outcome for an operation that discards code.
func TestAcceptRejectsAnUnknownSide(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	writeConflicted(t, dir, "c.go")

	r := s.callTool("node_edit", map[string]any{"node": "c.go", "accept": "latest"})
	if !r.IsError {
		t.Fatal("an unknown side must be refused, not guessed")
	}
	if !strings.Contains(r.Content[0].Text, `"mine"`) || !strings.Contains(r.Content[0].Text, `"theirs"`) {
		t.Errorf("the error should name the two real answers; got %s", r.Content[0].Text)
	}
	// And the file is untouched — a rejected op writes nothing.
	got, _ := os.ReadFile(filepath.Join(dir, "c.go"))
	if string(got) != conflictedSrc {
		t.Error("a refused accept must not modify the file")
	}
}

// Conflicts rarely all go the same way, so a narrower address resolves ONE
// region and leaves the rest conflicted.
func TestAcceptScopedToOneRegion(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	two := "package main\n\n<<<<<<< HEAD\nA1\n=======\nB1\n>>>>>>> x\n\nmid\n\n<<<<<<< HEAD\nA2\n=======\nB2\n>>>>>>> x\n"
	if err := os.WriteFile(filepath.Join(dir, "two.go"), []byte(two), 0o644); err != nil {
		t.Fatal(err)
	}
	// Address the FIRST region by its span (lines 3-7).
	r := s.callTool("node_edit", map[string]any{"node": "two.go@3-7", "accept": "theirs"})
	if r.IsError {
		t.Fatalf("scoped accept errored: %s", r.Content[0].Text)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "two.go"))
	out := string(got)
	if !strings.Contains(out, "B1") || strings.Contains(out, "A1") {
		t.Errorf("region 1 should be resolved to theirs:\n%s", out)
	}
	if !strings.Contains(out, "<<<<<<< HEAD\nA2") {
		t.Errorf("region 2 must remain conflicted:\n%s", out)
	}
}

// The third resolution: neither side. This needs no special case — a
// conflict region is addressed as a SPAN, and newText alone already replaces
// a span (the rule that makes `::body newText:…` rewrite a body without
// quoting it back). Pinned here because the merge workflow depends on it.
func TestNewTextAloneResolvesAConflictRegion(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	writeConflicted(t, dir, "c.go")

	r := s.callTool("node_edit", map[string]any{
		"node": "c.go@5-9", "newText": `func Pick() string { return "merged" }`,
	})
	if r.IsError {
		t.Fatalf("newText on a conflict region errored: %s", r.Content[0].Text)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "c.go"))
	out := string(got)
	if !strings.Contains(out, `return "merged"`) {
		t.Errorf("the custom resolution should land:\n%s", out)
	}
	for _, gone := range []string{"<<<<<<<", ">>>>>>>", `"ours"`, `"theirs"`} {
		if strings.Contains(out, gone) {
			t.Errorf("%q should be gone after a custom resolve:\n%s", gone, out)
		}
	}
	if !strings.Contains(out, "func Before() int { return 1 }") {
		t.Errorf("surrounding code must survive:\n%s", out)
	}
}

// The span rule is general, not a conflict exemption: newText alone replaces
// ANY addressed span. Pinned because it is the reason no conflict-specific
// path is needed — and because it is a sharp edge worth stating out loud,
// since the same call against a SYMBOL address is refused until oldText is
// supplied.
func TestNewTextAloneReplacesAnySpanNotJustConflicts(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	writeConflicted(t, dir, "c.go")

	if r := s.callTool("node_edit", map[string]any{"node": "c.go@3-3", "newText": "// gone"}); r.IsError {
		t.Fatalf("a span rewrite needs no oldText: %s", r.Content[0].Text)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "c.go"))
	if !strings.Contains(string(got), "// gone") {
		t.Errorf("the span should have been replaced:\n%s", got)
	}
	// A SYMBOL address is still guarded — there the whole declaration would
	// be at stake and oldText is the compare-and-swap.
	r := s.callTool("node_edit", map[string]any{"node": "main.go#Free", "newText": "func Free() {}"})
	if !r.IsError || !strings.Contains(r.Content[0].Text, "oldText") {
		t.Errorf("a symbol still needs oldText; got isErr=%v %s", r.IsError, r.Content[0].Text)
	}
}

// Resolving the TEXT does not resolve the MERGE — git keeps the file staged
// as unmerged, so `git status` still blocks. Finishing silently would leave a
// caller believing the job is done.
func TestAcceptSaysTheMergeIsNotFinished(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	writeConflicted(t, dir, "c.go")

	r := s.callTool("node_edit", map[string]any{"node": "c.go", "accept": "mine"})
	if r.IsError {
		t.Fatalf("accept errored: %s", r.Content[0].Text)
	}
	note := decodeNote(t, r)
	if !strings.Contains(note, "git add c.go") {
		t.Errorf("the note must name the remaining step; got %q", note)
	}
	if !strings.Contains(note, "resolved 1 conflict") {
		t.Errorf("the note should say what it did; got %q", note)
	}
}

// With conflicts left over, the note reports the remainder instead of
// claiming the file is ready to stage.
func TestAcceptReportsRemainingConflicts(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	two := "package main\n\n<<<<<<< HEAD\nA1\n=======\nB1\n>>>>>>> x\n\nmid\n\n<<<<<<< HEAD\nA2\n=======\nB2\n>>>>>>> x\n"
	if err := os.WriteFile(filepath.Join(dir, "two.go"), []byte(two), 0o644); err != nil {
		t.Fatal(err)
	}
	r := s.callTool("node_edit", map[string]any{"node": "two.go@3-7", "accept": "mine"})
	if r.IsError {
		t.Fatalf("scoped accept errored: %s", r.Content[0].Text)
	}
	note := decodeNote(t, r)
	if !strings.Contains(note, "1 still unresolved") {
		t.Errorf("a partial resolve must not read as finished; got %q", note)
	}
	if strings.Contains(note, "git add") {
		t.Errorf("staging advice is wrong while conflicts remain; got %q", note)
	}
}

func decodeNote(t *testing.T, r toolResp) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Content[0].Text), &m); err != nil {
		t.Fatalf("decode: %v (%s)", err, r.Content[0].Text)
	}
	s, _ := m["note"].(string)
	return s
}
