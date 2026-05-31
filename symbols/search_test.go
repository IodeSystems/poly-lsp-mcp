package symbols_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSearchFindsMatches(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.go":      "package main\n// TODO: clean this up\nfunc foo() {}\n",
		"sub/b.go":  "package sub\n// TODO(carl): another\n",
		"c.md":      "# Header\n\nTODO write docs\n",
		"no-match.txt": "nothing interesting here\n",
	})

	pat := regexp.MustCompile(`TODO`)
	hits, dropped, err := symbols.Search(dir, pat, symbols.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3: %+v", len(hits), hits)
	}
	for _, h := range hits {
		if h.Line < 1 || h.Col < 1 {
			t.Errorf("hit has bad position: %+v", h)
		}
		if !regexpMatches(h.Text, `TODO`) {
			t.Errorf("hit text %q doesn't contain TODO", h.Text)
		}
		if h.MatchEndCol <= h.Col {
			t.Errorf("MatchEndCol %d not past Col %d", h.MatchEndCol, h.Col)
		}
	}
}

func TestSearchRespectsGlob(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.go":  "TODO go\n",
		"b.py":  "TODO py\n",
		"c.md":  "TODO md\n",
	})

	pat := regexp.MustCompile(`TODO`)
	hits, _, err := symbols.Search(dir, pat, symbols.SearchOptions{Glob: "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1: %+v", len(hits), hits)
	}
	if filepath.Base(hits[0].File) != "a.go" {
		t.Errorf("hit not in a.go: %+v", hits[0])
	}
}

func TestSearchSkipsBinaryFiles(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"text.txt": "TODO real match\n",
	})
	// Binary file: starts with a null byte. Even if it contains
	// the literal bytes "TODO", we drop it.
	binary := append([]byte{0, 0, 0}, []byte("TODO embedded")...)
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), binary, 0o644); err != nil {
		t.Fatal(err)
	}

	pat := regexp.MustCompile(`TODO`)
	hits, _, err := symbols.Search(dir, pat, symbols.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1 (binary file should be skipped): %+v", len(hits), hits)
	}
}

func TestSearchSkipsNoiseDirs(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"src/a.go":              "TODO real\n",
		"node_modules/foo.js":   "TODO ignored\n",
		"vendor/foo.go":         "TODO ignored\n",
		".git/hooks/post-merge": "TODO ignored\n",
	})

	pat := regexp.MustCompile(`TODO`)
	hits, _, err := symbols.Search(dir, pat, symbols.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1: %+v", len(hits), hits)
	}
	if filepath.Base(filepath.Dir(hits[0].File)) != "src" {
		t.Errorf("hit not in src/: %+v", hits[0])
	}
}

func TestSearchLimitTrips(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.txt": "TODO\nTODO\nTODO\nTODO\nTODO\n",
	})
	pat := regexp.MustCompile(`TODO`)
	hits, dropped, err := symbols.Search(dir, pat, symbols.SearchOptions{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Errorf("len(hits) = %d, want 3", len(hits))
	}
	if dropped != 2 {
		t.Errorf("dropped = %d, want 2", dropped)
	}
}

func TestSearchContextLines(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.txt": "line 1\nline 2\nMATCH here\nline 4\nline 5\n",
	})
	pat := regexp.MustCompile(`MATCH`)
	hits, _, err := symbols.Search(dir, pat, symbols.SearchOptions{ContextLines: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	h := hits[0]
	if len(h.Before) != 2 || h.Before[0] != "line 1" || h.Before[1] != "line 2" {
		t.Errorf("Before = %+v", h.Before)
	}
	if len(h.After) != 2 || h.After[0] != "line 4" || h.After[1] != "line 5" {
		t.Errorf("After = %+v", h.After)
	}
}

func TestSearchCaseInsensitiveViaInlineFlag(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.txt": "Hello World\nhello there\nHELLO\n",
	})
	pat := regexp.MustCompile(`(?i)hello`)
	hits, _, err := symbols.Search(dir, pat, symbols.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Errorf("case-insensitive hits = %d, want 3", len(hits))
	}
}

func TestSearchEmptyPatternErrors(t *testing.T) {
	dir := t.TempDir()
	_, _, err := symbols.Search(dir, nil, symbols.SearchOptions{})
	if err == nil {
		t.Error("expected error for nil pattern")
	}
}

func TestSearchSortedDeterministic(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"z.txt":       "TODO at z\n",
		"a.txt":       "line\nTODO at a\nTODO again\n",
		"sub/m.txt":   "TODO in sub\n",
	})
	pat := regexp.MustCompile(`TODO`)
	hits, _, err := symbols.Search(dir, pat, symbols.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Hits should be sorted by file path then line.
	for i := 1; i < len(hits); i++ {
		prev, cur := hits[i-1], hits[i]
		if prev.File > cur.File {
			t.Errorf("hits out of file order: %s before %s", prev.File, cur.File)
		}
		if prev.File == cur.File && prev.Line > cur.Line {
			t.Errorf("hits out of line order in %s: %d before %d", prev.File, prev.Line, cur.Line)
		}
	}
}

func regexpMatches(s, pat string) bool {
	return regexp.MustCompile(pat).MatchString(s)
}
