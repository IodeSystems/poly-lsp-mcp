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
	// Rows that exist on neither side are WITHHELD rather than named — see
	// TestChimerasAreWithheldFromQueryResults — so the note accounts for the
	// removal instead of listing phantoms.
	if !strings.Contains(q.Note, "WITHHELD") {
		t.Errorf("the removal must be accounted for: %s", q.Note)
	}
	for _, m := range q.Matches {
		if strings.Contains(m.Node, "B[1]") {
			t.Errorf("a phantom must not be listed at all: %s", m.Node)
		}
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

// "0 matches" is not a reliable "not found" while a conflict is open.
//
// tree-sitter recovers past the markers by SWALLOWING what follows: a file
// conflicted at one function had the declaration above the markers absorb
// every declaration below it, so those symbols were never parsed — not
// withheld, absent. Asking for one returned totalMatches:0 with no note at
// all, and "0" reads as "that code does not exist" rather than "the file it
// lives in is mid-merge".
func TestZeroResultSaysTheWorkspaceIsMidMerge(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	if err := os.WriteFile(filepath.Join(dir, "m.go"), []byte(straddleSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	// The set is normally seeded from git at index time; state it directly so
	// the note is tested without standing up a repo mid-merge (git's side is
	// covered by TestUnmergedPaths).
	s.srv.conflictMu.Lock()
	s.srv.conflicted = map[string]bool{"m.go": true}
	s.srv.conflictMu.Unlock()

	q := query(t, s, map[string]any{"selector": "func[name=DefinitelyNotIndexed]"})
	if q.TotalMatches != 0 {
		t.Fatalf("fixture drift: expected zero matches, got %d", q.TotalMatches)
	}
	for _, want := range []string{"NOT a reliable", "m.go", "::conflict", `accept:"mine"`} {
		if !strings.Contains(q.Note, want) {
			t.Errorf("zero-result note missing %q: %q", want, q.Note)
		}
	}
}

// A clean workspace must not acquire a conflict note — the warning is only
// worth anything if it means something.
func TestZeroResultIsQuietWithoutAConflict(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()
	q := query(t, s, map[string]any{"selector": "func[name=DefinitelyNotIndexed]"})
	if strings.Contains(q.Note, "merge conflict") {
		t.Errorf("clean workspace warned about a conflict: %q", q.Note)
	}
}
