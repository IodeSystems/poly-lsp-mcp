package symbols

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

func TestLexicalExtractorFiltersKeywords(t *testing.T) {
	e := &LexicalExtractor{Keywords: keywordSet("func", "package", "type")}
	hits := e.Extract([]byte("package main\nfunc Foo() {}\ntype Bar int\n"))
	got := map[string]bool{}
	for _, h := range hits {
		got[h.Name] = true
	}
	for _, kw := range []string{"package", "func", "type"} {
		if got[kw] {
			t.Errorf("keyword %q leaked through filter", kw)
		}
	}
	for _, want := range []string{"main", "Foo", "Bar", "int"} {
		if !got[want] {
			t.Errorf("missing expected token %q", want)
		}
	}
}

func TestLexicalExtractorTracksLineCol(t *testing.T) {
	e := &LexicalExtractor{}
	hits := e.Extract([]byte("alpha beta\n  gamma\n"))
	want := []Hit{
		{Name: "alpha", Line: 1, Col: 1},
		{Name: "beta", Line: 1, Col: 7},
		{Name: "gamma", Line: 2, Col: 3},
	}
	if len(hits) != len(want) {
		t.Fatalf("got %d hits, want %d: %+v", len(hits), len(want), hits)
	}
	for i, h := range hits {
		if h != want[i] {
			t.Errorf("hit %d: got %+v, want %+v", i, h, want[i])
		}
	}
}

// fixturePath resolves a path relative to the repo root, regardless of
// the test binary's working directory.
func fixturePath(t *testing.T, parts ...string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "..")
	return filepath.Join(append([]string{root}, parts...)...)
}

func TestBuildPolyglotFindsUserID(t *testing.T) {
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Build(fixturePath(t, "testdata", "fixtures", "polyglot"), reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	sites := idx.Lookup("UserID")
	if len(sites) < 6 {
		t.Errorf("UserID: got %d sites, want >= 6 (one per file plus uses): %+v", len(sites), sites)
	}
	langs := map[string]bool{}
	files := map[string]bool{}
	for _, s := range sites {
		langs[s.Language] = true
		files[filepath.Base(s.File)] = true
	}
	for _, want := range []string{"go", "typescript", "python", "markdown", "yaml"} {
		if !langs[want] {
			t.Errorf("UserID missing from language %q (sites: %+v)", want, sites)
		}
	}
	for _, want := range []string{"main.go", "client.ts", "worker.py", "README.md", "config.yaml"} {
		if !files[want] {
			t.Errorf("UserID missing from file %q", want)
		}
	}
}

func TestBuildPolyglotFindsGreetUserCrossLanguage(t *testing.T) {
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Build(fixturePath(t, "testdata", "fixtures", "polyglot"), reg)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{}
	for _, s := range idx.Lookup("GreetUser") {
		files[filepath.Base(s.File)] = true
	}
	// Go declares + uses; YAML and Markdown name it as a string.
	for _, want := range []string{"main.go", "config.yaml", "README.md"} {
		if !files[want] {
			t.Errorf("GreetUser missing from %q (found in: %v)", want, files)
		}
	}
}

func TestGoKeywordsFiltered(t *testing.T) {
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Build(fixturePath(t, "testdata", "fixtures", "lsp-only"), reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, kw := range []string{"package", "func", "return", "import"} {
		if sites := idx.Lookup(kw); len(sites) > 0 {
			// Allow markdown/yaml/json to mention them, but lsp-only has none.
			for _, s := range sites {
				if s.Language == "go" {
					t.Errorf("Go keyword %q leaked into index at %s:%d:%d",
						kw, s.File, s.Line, s.Col)
				}
			}
		}
	}
}

func TestRefreshReplacesFile(t *testing.T) {
	idx := NewIndex()
	idx.addHits("/a/foo.go", "go", []Hit{
		{Name: "X", Line: 1, Col: 1},
		{Name: "Y", Line: 2, Col: 1},
	})
	idx.addHits("/a/bar.go", "go", []Hit{
		{Name: "X", Line: 5, Col: 1},
	})
	if got := len(idx.Lookup("X")); got != 2 {
		t.Fatalf("pre-refresh X count = %d, want 2", got)
	}
	// Refresh foo.go: drop X, keep nothing else, replace with Z.
	idx.Refresh("/a/foo.go", "go", []Hit{{Name: "Z", Line: 1, Col: 1}})
	if got := idx.Lookup("X"); len(got) != 1 || got[0].File != "/a/bar.go" {
		t.Errorf("X after refresh: got %+v, want one site in bar.go", got)
	}
	if got := idx.Lookup("Y"); len(got) != 0 {
		t.Errorf("Y after refresh: got %+v, want empty (foo.go was sole source)", got)
	}
	if got := idx.Lookup("Z"); len(got) != 1 || got[0].File != "/a/foo.go" {
		t.Errorf("Z after refresh: got %+v, want one site in foo.go", got)
	}
}

func TestIndexLanguagesReportsDistinct(t *testing.T) {
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Build(fixturePath(t, "testdata", "fixtures", "polyglot"), reg)
	if err != nil {
		t.Fatal(err)
	}
	got := idx.Languages()
	want := map[string]bool{"go": true, "typescript": true, "python": true, "markdown": true, "yaml": true, "json": true, "sql": true, "proto": true}
	if len(got) != len(want) {
		t.Errorf("Languages = %v, want %d distinct entries", got, len(want))
	}
	for _, lang := range got {
		if !want[lang] {
			t.Errorf("unexpected language %q", lang)
		}
	}
}

func TestDeclaredNamesReturnsOnlyDeclaredBindings(t *testing.T) {
	idx := NewIndex()
	idx.addHits("/a.go", "go", []Hit{{Name: "Lexical", Line: 1, Col: 1}})
	idx.InsertDeclared("Bound", "/a.go", "go", 2, 1)
	idx.InsertDeclared("AlsoBound", "/b.go", "go", 5, 3)

	got := idx.DeclaredNames()
	if len(got) != 2 || got[0] != "AlsoBound" || got[1] != "Bound" {
		t.Errorf("DeclaredNames = %+v, want sorted [AlsoBound, Bound]", got)
	}
	for _, name := range got {
		if name == "Lexical" {
			t.Errorf("Lexical-only name leaked into DeclaredNames: %+v", got)
		}
	}
}

func TestSkipDirsObeyed(t *testing.T) {
	// Sanity: building the repo root must not descend into .git.
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Build(fixturePath(t), reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range idx.Names() {
		for _, s := range idx.Lookup(name) {
			if strings.Contains(s.File, "/.git/") {
				t.Fatalf("indexed file under .git: %s", s.File)
			}
		}
	}
}

// TestLookupExistingEvictsMissingFiles proves the self-healing path:
// sites pointing at a file that no longer exists on disk are dropped
// from the result and permanently evicted from the index (a plain
// Lookup afterward no longer sees them). Covers lexical and declared
// stores, since an external git mv / checkout orphans both.
func TestLookupExistingEvictsMissingFiles(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.go")
	if err := os.WriteFile(live, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(dir, "gone.go") // never created on disk

	idx := NewIndex()
	idx.addHits(live, "go", []Hit{{Name: "Shared", Line: 1, Col: 1}})
	idx.addHits(gone, "go", []Hit{{Name: "Shared", Line: 2, Col: 1}})
	idx.InsertDeclared("Shared", gone, "go", 3, 1)

	got := idx.LookupExisting("Shared")
	if len(got) != 1 || got[0].File != live {
		t.Fatalf("LookupExisting = %+v, want one site in %s", got, live)
	}
	// Eviction persists: a plain Lookup no longer sees the gone file.
	if got := idx.Lookup("Shared"); len(got) != 1 || got[0].File != live {
		t.Errorf("post-evict Lookup = %+v, want one site in %s", got, live)
	}
}

func hitNames(hits []Hit) map[string]bool {
	out := make(map[string]bool, len(hits))
	for _, h := range hits {
		out[h.Name] = true
	}
	return out
}

func TestCExtractorSkipsCommentsStringsAndKeywords(t *testing.T) {
	src := []byte(`#include <stdio.h>
// mentions ghost_in_comment
static const char *label = "ghost_in_string";
int add(int lhs, int rhs) { return lhs + rhs; }
`)
	names := hitNames(DefaultExtractor("c").Extract(src))
	for _, want := range []string{"add", "lhs", "rhs", "label"} {
		if !names[want] {
			t.Errorf("missing %q; have %v", want, names)
		}
	}
	// The precision win over the lexical tier: identifier-shaped tokens
	// in comments and string literals are not names.
	for _, unwanted := range []string{"ghost_in_comment", "ghost_in_string", "NULL"} {
		if names[unwanted] {
			t.Errorf("%q leaked into the index", unwanted)
		}
	}
}

func TestCppExtractorIndexesNamespaceScope(t *testing.T) {
	src := []byte(`namespace app {
class Widget { int width; };
}
int app::Widget::area() const { return width; }
`)
	names := hitNames(DefaultExtractor("cpp").Extract(src))
	// Both halves of a qualified name are indexed separately, which is
	// what makes Widget findable from an out-of-line definition.
	for _, want := range []string{"app", "Widget", "area", "width"} {
		if !names[want] {
			t.Errorf("missing %q; have %v", want, names)
		}
	}
}

func TestKotlinExtractorSkipsCommentsStringsAndIt(t *testing.T) {
	src := []byte(`package app
// mentions ghostInComment
val label = "ghostInString"
fun scroll(up: Boolean): Int = listOf(1).map { it + 1 }.first()
`)
	names := hitNames(DefaultExtractor("kotlin").Extract(src))
	for _, want := range []string{"scroll", "up", "label", "listOf", "map"} {
		if !names[want] {
			t.Errorf("missing %q; have %v", want, names)
		}
	}
	// Comments and string literals are not names — the precision win
	// over the lexical tier. `it` is filtered as noise: it is the
	// implicit lambda parameter, file-local by construction, and one of
	// the most frequent tokens in any Kotlin repo.
	for _, unwanted := range []string{"ghostInComment", "ghostInString", "it", "Boolean"} {
		if names[unwanted] {
			t.Errorf("%q leaked into the index", unwanted)
		}
	}
}

func TestGroovyExtractorSkipsCommentsAndStrings(t *testing.T) {
	src := []byte(`package app
// mentions ghostInComment
def label = "ghostInString"
class Widget { int onKey(String event) { return 1 } }
`)
	names := hitNames(DefaultExtractor("groovy").Extract(src))
	for _, want := range []string{"Widget", "onKey", "event", "label"} {
		if !names[want] {
			t.Errorf("missing %q; have %v", want, names)
		}
	}
	for _, unwanted := range []string{"ghostInComment", "ghostInString", "String"} {
		if names[unwanted] {
			t.Errorf("%q leaked into the index", unwanted)
		}
	}
}
