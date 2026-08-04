package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The poll-shaped substitute for a server→client event: a query that returns
// symbols from a conflicted file says so, attached to the rows a caller is
// about to act on.
func TestQueryWarnsWhenResultsComeFromAConflictedFile(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	if err := os.WriteFile(filepath.Join(dir, "m.go"), []byte(straddleSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	q := query(t, s, map[string]any{"selector": "path=m.go func", "limit": 10})
	if q.Note == "" {
		t.Fatal("symbols from a conflicted file must carry a warning")
	}
	for _, want := range []string{"UNRESOLVED merge conflict", "m.go", "::conflict", `accept:"mine"`} {
		if !strings.Contains(q.Note, want) {
			t.Errorf("the note should name the problem and the way out (%q missing): %s", want, q.Note)
		}
	}
	// The dangerous rows are named, not just the file.
	if !strings.Contains(q.Note, "STRADDLE") || !strings.Contains(q.Note, "m.go#B[1]") {
		t.Errorf("rows that exist on neither side must be named: %s", q.Note)
	}
}

// Reading a chimera is where a caller SEES the impossible declaration, so
// that is where it has to be said. node_edit already refuses to write it.
func TestReadWarnsOnASpanStraddlingAConflict(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	if err := os.WriteFile(filepath.Join(dir, "m.go"), []byte(straddleSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	m, isErr := readNode(t, s, map[string]any{"node": "m.go#B[1]"})
	if isErr {
		t.Fatalf("reading is allowed, only writing is refused: %+v", m)
	}
	note, _ := m["note"].(string)
	for _, want := range []string{"straddles", "part MINE and part THEIRS", "m.go@5-17", "::theirs"} {
		if !strings.Contains(note, want) {
			t.Errorf("the read note should explain and offer a way out (%q missing): %s", want, note)
		}
	}
	// The text still comes back — reading is safe and sometimes necessary.
	if txt, _ := m["text"].(string); !strings.Contains(txt, "=======") {
		t.Errorf("the span's real text should still be returned; got %q", txt)
	}
}

// A clean workspace must stay silent, or the warning becomes noise callers
// learn to skip.
func TestNoConflictWarningOnACleanWorkspace(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	q := query(t, s, map[string]any{"selector": "path=main.go func", "limit": 10})
	if strings.Contains(q.Note, "merge conflict") {
		t.Errorf("a clean file must not be warned about: %s", q.Note)
	}
	m, _ := readNode(t, s, map[string]any{"node": "main.go#Free"})
	if note, _ := m["note"].(string); strings.Contains(note, "straddles") {
		t.Errorf("a clean symbol must not be warned about: %s", note)
	}
}

// The conflict VIEWS are about the conflict; they are not victims of it, so
// they must not be listed as straddling their own region.
func TestConflictViewsAreNotFlaggedAsChimeras(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	if err := os.WriteFile(filepath.Join(dir, "m.go"), []byte(straddleSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, sel := range []string{"#'m.go'::conflict", "#'m.go'::mine", "#'m.go'::theirs"} {
		q := query(t, s, map[string]any{"selector": sel, "limit": 5})
		if strings.Contains(q.Note, "STRADDLE") {
			t.Errorf("%s: the conflict views must not flag themselves: %s", sel, q.Note)
		}
	}
}
