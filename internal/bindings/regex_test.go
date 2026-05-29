package bindings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/tslsmcp/internal/config"
	"github.com/iodesystems/tslsmcp/internal/symbols"
)

func TestEvalRegexSinglePatternNoCapture(t *testing.T) {
	content := []byte("UserID is here\nand UserID is there\n")
	hits, err := evalRegex(content, []string{"UserID"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2: %+v", len(hits), hits)
	}
	want := []regexHit{
		{Line: 1, Col: 1, Text: "UserID"},
		{Line: 2, Col: 5, Text: "UserID"},
	}
	for i, h := range hits {
		if h != want[i] {
			t.Errorf("hit %d = %+v, want %+v", i, h, want[i])
		}
	}
}

func TestEvalRegexCaptureGroupSelectsToken(t *testing.T) {
	content := []byte(`name="UserID" id=1`)
	hits, err := evalRegex(content, []string{`name="(UserID)"`})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1: %+v", len(hits), hits)
	}
	// The capture is "UserID" starting at byte 6, col 7.
	if hits[0].Text != "UserID" {
		t.Errorf("text = %q, want UserID", hits[0].Text)
	}
	if hits[0].Col != 7 {
		t.Errorf("col = %d, want 7", hits[0].Col)
	}
}

func TestEvalRegexTooManyCaptureGroupsRejected(t *testing.T) {
	_, err := evalRegex([]byte("abc"), []string{`(a)(b)`})
	if err == nil {
		t.Fatal("expected error for two capture groups")
	}
	if !strings.Contains(err.Error(), "at most 1 allowed") {
		t.Errorf("error message %q should mention the limit", err)
	}
}

func TestEvalRegexMultiplePatternsUnion(t *testing.T) {
	content := []byte("UserID at top\nuser_id at bottom\n")
	hits, err := evalRegex(content, []string{"UserID", "user_id"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h.Text] = true
	}
	if !got["UserID"] || !got["user_id"] {
		t.Errorf("missing expected text: %+v", got)
	}
}

func TestEvalRegexBadPatternErrors(t *testing.T) {
	_, err := evalRegex([]byte("abc"), []string{`[invalid`})
	if err == nil {
		t.Error("expected error for invalid regex")
	}
}

func TestEvalRegexEmptyPatternListErrors(t *testing.T) {
	_, err := evalRegex([]byte("abc"), nil)
	if err == nil {
		t.Error("expected error for empty pattern list")
	}
}

func TestEvalRegexLineColTracking(t *testing.T) {
	content := []byte("a\n  bb\n   ccc\n")
	hits, err := evalRegex(content, []string{`\w+`})
	if err != nil {
		t.Fatal(err)
	}
	want := []regexHit{
		{Line: 1, Col: 1, Text: "a"},
		{Line: 2, Col: 3, Text: "bb"},
		{Line: 3, Col: 4, Text: "ccc"},
	}
	if len(hits) != len(want) {
		t.Fatalf("got %d, want %d", len(hits), len(want))
	}
	for i, h := range hits {
		if h != want[i] {
			t.Errorf("hit %d = %+v, want %+v", i, h, want[i])
		}
	}
}

// Resolver-level tests: file I/O, language tagging, aliasing-skip,
// size cap.

func TestResolverRegexFormReadsMarkdown(t *testing.T) {
	dir := t.TempDir()
	mdPath := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(mdPath,
		[]byte("# UserID\n\nThe `UserID` is the primary key.\nAlso UserID matters.\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	idx := symbols.NewIndex()
	r := NewResolver(dir)
	n, err := r.Apply(idx, []config.Binding{{
		Name:  "UserID",
		Sites: []config.BindingSite{{File: "notes.md", Regex: []string{"UserID"}}},
	}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n != 3 {
		t.Errorf("inserted = %d, want 3 (heading + backtick + sentence)", n)
	}
	sites := idx.Lookup("UserID")
	if len(sites) != 3 {
		t.Errorf("Lookup = %d sites, want 3", len(sites))
	}
	for _, s := range sites {
		if s.Confidence != symbols.ConfidenceDeclared {
			t.Errorf("confidence = %d, want Declared", s.Confidence)
		}
		if s.Language != "markdown" {
			t.Errorf("language = %q, want markdown", s.Language)
		}
	}
}

func TestResolverRegexFormSkipsAliasingMatches(t *testing.T) {
	// Binding name "UserID" but the regex matches "user_id" tokens —
	// resolver should skip every match because the text doesn't equal
	// the binding name, then fail the site overall.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.txt"),
		[]byte("create user_id int;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx := symbols.NewIndex()
	r := NewResolver(dir)
	n, err := r.Apply(idx, []config.Binding{{
		Name:  "UserID",
		Sites: []config.BindingSite{{File: "schema.txt", Regex: []string{"user_id"}}},
	}})
	if err != nil {
		t.Errorf("Apply returned a top-level error; per-site failure should be logged not aggregated as binding error: %v", err)
	}
	if n != 0 {
		t.Errorf("inserted = %d, want 0 (aliasing match should be skipped)", n)
	}
}

func TestResolverRegexFormHonorsSizeCap(t *testing.T) {
	dir := t.TempDir()
	huge := make([]byte, maxRegexFileSize+1)
	for i := range huge {
		huge[i] = 'a'
	}
	if err := os.WriteFile(filepath.Join(dir, "blob.txt"), huge, 0o644); err != nil {
		t.Fatal(err)
	}
	idx := symbols.NewIndex()
	r := NewResolver(dir)
	n, err := r.Apply(idx, []config.Binding{{
		Name:  "aaaa",
		Sites: []config.BindingSite{{File: "blob.txt", Regex: []string{"aaaa"}}},
	}})
	if err != nil {
		t.Errorf("Apply returned err: %v", err)
	}
	if n != 0 {
		t.Errorf("inserted = %d, want 0 (file should be skipped by size cap)", n)
	}
}

func TestResolverRegexFormMultiplePatternsCoOperate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"),
		[]byte("## UserID heading\n\nUserID is in prose.\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	idx := symbols.NewIndex()
	r := NewResolver(dir)
	n, err := r.Apply(idx, []config.Binding{{
		Name: "UserID",
		Sites: []config.BindingSite{{
			File: "notes.md",
			Regex: []string{
				`^## (UserID)`, // heading-only via anchor + capture
				`(UserID) is`,  // sentence form — capture isolates "UserID"
			},
		}},
	}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n != 2 {
		t.Errorf("inserted = %d, want 2 (one per pattern)", n)
	}
}
