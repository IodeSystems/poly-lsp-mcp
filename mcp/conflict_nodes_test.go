package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const diff3Src = `package cv

func Keep() {}

<<<<<<< HEAD
func Pick() string { return "ours" }
||||||| merged common ancestors
func Pick() string { return "base" }
=======
func Pick() string { return "theirs" }
>>>>>>> feature/x

func Also() {}
`

func startConflicted(t *testing.T) (*mcpSession, string) {
	t.Helper()
	s, dir := startModern(t)
	if err := os.WriteFile(filepath.Join(dir, "cv.go"), []byte(diff3Src), 0o644); err != nil {
		t.Fatal(err)
	}
	return s, dir
}

// A conflict is a REGION with two (or three) versions under it, not a pile of
// peer declarations. Without this the index reports `Pick` twice, as ordinary
// top-level funcs at adjacent lines, describing a file that exists in no
// commit and on no branch.
func TestConflictRegionAndSidesAreNodes(t *testing.T) {
	s, _ := startConflicted(t)
	defer s.close()

	region := query(t, s, map[string]any{"selector": "#'cv.go'::conflict", "limit": 5})
	if len(region.Matches) != 1 {
		t.Fatalf("want one conflict region; got %+v", region.Matches)
	}
	r := region.Matches[0]
	if r.Class != "::conflict" {
		t.Errorf("the type should model the selector that finds it; got %q", r.Class)
	}
	// The region spans marker to marker, so replacing it resolves the block.
	if r.At[0] != 5 || r.At[1] != 11 {
		t.Errorf("region should span marker to marker (5-11); got %v", r.At)
	}
	if !strings.HasPrefix(r.Text, "<<<<<<<") || !strings.HasSuffix(r.Text, "feature/x") {
		t.Errorf("a region carries its source verbatim, markers included; got %q", r.Text)
	}

	for _, tc := range []struct{ sel, class, want string }{
		{"#'cv.go'::mine", "::mine", `return "ours"`},
		{"#'cv.go'::theirs", "::theirs", `return "theirs"`},
		{"#'cv.go'::base", "::base", `return "base"`},
	} {
		q := query(t, s, map[string]any{"selector": tc.sel, "limit": 5})
		if len(q.Matches) != 1 {
			t.Fatalf("%s: want one side; got %+v", tc.sel, q.Matches)
		}
		m := q.Matches[0]
		if m.Class != tc.class {
			t.Errorf("%s: type %q, want %q", tc.sel, m.Class, tc.class)
		}
		if !strings.Contains(m.Text, tc.want) {
			t.Errorf("%s: text %q should contain %q", tc.sel, m.Text, tc.want)
		}
		// A side hangs off its REGION, not off the file.
		if m.In != "cv.go@5-11" {
			t.Errorf("%s: a side belongs to its region; got in=%q", tc.sel, m.In)
		}
	}
}

// Provenance is the side's real identity. Under a rebase "ours" is the
// upstream being replayed onto and "theirs" is your own commit, so position
// alone misleads exactly when a caller is most confused.
func TestConflictSidesCarryTheirRef(t *testing.T) {
	s, _ := startConflicted(t)
	defer s.close()

	mine := query(t, s, map[string]any{"selector": "#'cv.go'::mine", "limit": 2})
	if got := mine.Matches[0].Ref; got != "HEAD" {
		t.Errorf("mine should name the ref git wrote; got %q", got)
	}
	theirs := query(t, s, map[string]any{"selector": "#'cv.go'::theirs", "limit": 2})
	if got := theirs.Matches[0].Ref; got != "feature/x" {
		t.Errorf("theirs should name its branch; got %q", got)
	}
	// The region summarizes all sides, so one query answers "what is in
	// conflict and between which refs".
	region := query(t, s, map[string]any{"selector": "#'cv.go'::conflict", "limit": 2})
	sides := region.Matches[0].Sides
	if sides["mine"] != "HEAD" || sides["theirs"] != "feature/x" || sides["base"] != "merged common ancestors" {
		t.Errorf("the region should name every side's ref; got %+v", sides)
	}
}

// diff3's base is what makes a three-way read possible — "they changed the
// return, you changed the name" rather than "these blocks differ". Without a
// base written, it must not be invented.
func TestConflictBaseOnlyWhenDiff3WroteOne(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	two := "package m\n\n<<<<<<< HEAD\nA\n=======\nB\n>>>>>>> x\n"
	if err := os.WriteFile(filepath.Join(dir, "two.go"), []byte(two), 0o644); err != nil {
		t.Fatal(err)
	}
	if q := query(t, s, map[string]any{"selector": "#'two.go'::base", "limit": 5}); len(q.Matches) != 0 {
		t.Errorf("no diff3 base was written, so none should be reported; got %+v", q.Matches)
	}
	if q := query(t, s, map[string]any{"selector": "#'two.go'::mine", "limit": 5}); len(q.Matches) != 1 {
		t.Errorf("the two ordinary sides must still be there; got %+v", q.Matches)
	}
}

// Each region is separately addressable, because conflicts rarely all go the
// same way and per-region resolution is the thing that matters most.
func TestEveryRegionIsItsOwnNode(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	two := "package m\n\n<<<<<<< HEAD\nA1\n=======\nB1\n>>>>>>> x\n\nmid\n\n<<<<<<< HEAD\nA2\n=======\nB2\n>>>>>>> x\n"
	if err := os.WriteFile(filepath.Join(dir, "two.go"), []byte(two), 0o644); err != nil {
		t.Fatal(err)
	}
	q := query(t, s, map[string]any{"selector": "#'two.go'::conflict", "limit": 10})
	if len(q.Matches) != 2 {
		t.Fatalf("want a node per region; got %d", len(q.Matches))
	}
	if q.Matches[0].Node == q.Matches[1].Node {
		t.Error("regions must be distinctly addressable")
	}
	// And each side query yields one per region.
	if s := query(t, s, map[string]any{"selector": "#'two.go'::theirs", "limit": 10}); len(s.Matches) != 2 {
		t.Errorf("want one theirs per region; got %d", len(s.Matches))
	}
}

// A clean file has no conflict nodes — the check must not fire on prose that
// merely looks like a marker.
func TestNoConflictNodesOnACleanFile(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()
	for _, sel := range []string{"#'main.go'::conflict", "#'main.go'::mine", "#'notes.md'::theirs"} {
		if q := query(t, s, map[string]any{"selector": sel, "limit": 5}); len(q.Matches) != 0 {
			t.Errorf("%s should match nothing on a clean file; got %+v", sel, q.Matches)
		}
	}
}

// The address a side reports must read back as that side — the round trip
// that makes "look at theirs, then take it" two calls instead of a guess.
func TestConflictNodeAddressRoundTrips(t *testing.T) {
	s, _ := startConflicted(t)
	defer s.close()

	q := query(t, s, map[string]any{"selector": "#'cv.go'::theirs", "limit": 2})
	addr := q.Matches[0].Node
	m, isErr := readNode(t, s, map[string]any{"node": addr})
	if isErr {
		t.Fatalf("node_read %s errored: %+v", addr, m)
	}
	if got, _ := m["text"].(string); !strings.Contains(got, `return "theirs"`) {
		t.Errorf("reading a side's address should return that side; got %q", got)
	}
}
