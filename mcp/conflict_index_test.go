package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A declaration stitched out of BOTH sides exists nowhere, so listing it is a
// wrong answer rather than a partial one. This is the original bug: the index
// reported `func B[1]` spanning ours' header, a marker, and theirs' tail.
func TestChimerasAreWithheldFromQueryResults(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	if err := os.WriteFile(filepath.Join(dir, "m.go"), []byte(straddleSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	q := query(t, s, map[string]any{"selector": "path=m.go func", "limit": 20})
	for _, m := range q.Matches {
		if strings.Contains(m.Node, "B[1]") {
			t.Errorf("a declaration from neither side must not be listed: %s", m.Node)
		}
	}
	// Silence would be worse than the phantom: say what was withheld and why.
	if !strings.Contains(q.Note, "WITHHELD") {
		t.Errorf("the removal must be reported; got %q", q.Note)
	}
	if !strings.Contains(q.Note, "::mine") {
		t.Errorf("and must point at what DOES answer; got %q", q.Note)
	}
}

// Only STRADDLING rows go. A symbol that CONTAINS a conflict is real on both
// sides — it just has two bodies — and one away from conflicts is untouched.
// Withholding those would make a conflicted file unusable rather than honest.
func TestOnlyStraddlingRowsAreWithheld(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	src := "package i\n\nfunc Keep1() {}\n\nfunc Mid() int {\n<<<<<<< HEAD\n\treturn 1\n=======\n\treturn 2\n>>>>>>> f\n}\n\nfunc Keep2() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "i.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	q := query(t, s, map[string]any{"selector": "path=i.go func", "limit": 20})
	got := map[string]bool{}
	for _, m := range q.Matches {
		got[m.Node] = true
	}
	for _, want := range []string{"i.go#Keep1", "i.go#Mid", "i.go#Keep2"} {
		if !got[want] {
			t.Errorf("%s should survive; got %v", want, q.Matches)
		}
	}
	if strings.Contains(q.Note, "WITHHELD") {
		t.Errorf("nothing straddles here, so nothing should be withheld: %q", q.Note)
	}
	// The file-level warning still stands: Mid's body differs by side.
	if !strings.Contains(q.Note, "UNRESOLVED merge conflict") {
		t.Errorf("a conflicted file is still worth warning about; got %q", q.Note)
	}
}

// The conflict node reports what each side really DECLARES — the two-version
// read, from whole-file reconstructions that actually parse.
func TestConflictNodeReportsWhatEachSideDeclares(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	if err := os.WriteFile(filepath.Join(dir, "m.go"), []byte(straddleSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	q := query(t, s, map[string]any{"selector": "#'m.go'::conflict", "limit": 3})
	if len(q.Matches) != 1 {
		t.Fatalf("want one region; got %+v", q.Matches)
	}
	m := q.Matches[0]
	names := func(ds []map[string]string) []string {
		var out []string
		for _, d := range ds {
			out = append(out, d["sym"])
		}
		return out
	}
	mine, theirs := names(m.MineDeclares), names(m.TheirsDeclares)
	for _, side := range [][]string{mine, theirs} {
		if len(side) != 2 || side[0] != "A" || side[1] != "B" {
			t.Errorf("each side declares A and B once — no chimera, no duplicate; got %v", side)
		}
	}
}

// Names only, never addresses. A symbol found in a RECONSTRUCTION has a span
// in reconstructed coordinates while the file on disk still holds markers, so
// an address minted here would resolve against different bytes than the ones
// it came from.
func TestSideDeclarationsCarryNoAddresses(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	if err := os.WriteFile(filepath.Join(dir, "m.go"), []byte(straddleSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	q := query(t, s, map[string]any{"selector": "#'m.go'::conflict", "limit": 3})
	for _, d := range append(q.Matches[0].MineDeclares, q.Matches[0].TheirsDeclares...) {
		for k := range d {
			if k != "sym" && k != "class" {
				t.Errorf("a side declaration must carry no address-like field; got %q", k)
			}
		}
	}
}

// A side that does not reconstruct into parseable source declares nothing we
// can honestly report — symbols recovered from broken input are the chimeras
// again, one level down.
func TestNoDeclarationsWhenASideDoesNotParse(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	src := "package m\n\nvar s = \"text\n<<<<<<< HEAD\nours unterminated\n=======\ntheirs also broken\n>>>>>>> y\n"
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	m := query(t, s, map[string]any{"selector": "#'bad.go'::conflict", "limit": 3}).Matches[0]
	if len(m.MineDeclares) != 0 || len(m.TheirsDeclares) != 0 {
		t.Errorf("an unparseable side must declare nothing; got %v / %v", m.MineDeclares, m.TheirsDeclares)
	}
	if m.Diff == "" {
		t.Error("and the region should fall back to diffed text")
	}
}
