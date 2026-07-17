package symbols

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// SitesByFile is the file→sites inversion every edge query wants. It must
// (a) equal a manual inversion of Lookup over every name, (b) be memoized
// on the generation so a repeat call reuses the same map, and (c) rebuild
// after any mutation.
func TestSitesByFileEquivalenceAndMemo(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(rel, body string) {
		abs := filepath.Join(dir, rel)
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("a.go", "package a\nfunc Alpha() {}\nvar x = Alpha\n")
	writeFile("b.go", "package a\nfunc Beta() { Alpha() }\n")

	idx := NewIndex()
	// Two files' worth of hits, inserted the way Build does.
	idx.addHits(filepath.Join(dir, "a.go"), "go", []Hit{
		{Name: "Alpha", Line: 2, Col: 6}, {Name: "x", Line: 3, Col: 5}, {Name: "Alpha", Line: 3, Col: 9},
	})
	idx.addHits(filepath.Join(dir, "b.go"), "go", []Hit{
		{Name: "Beta", Line: 2, Col: 6}, {Name: "Alpha", Line: 2, Col: 14},
	})

	// (a) Equivalence: build the inversion by hand from Lookup over Names.
	want := map[string]map[[3]any]bool{}
	for _, name := range idx.Names() {
		for _, s := range idx.Lookup(name) {
			if want[s.File] == nil {
				want[s.File] = map[[3]any]bool{}
			}
			want[s.File][[3]any{name, s.Line, s.Col}] = true
		}
	}
	got := idx.SitesByFile()
	if len(got) != len(want) {
		t.Fatalf("inversion covers %d files, manual has %d", len(got), len(want))
	}
	for file, sites := range got {
		if len(sites) != len(want[file]) {
			t.Errorf("%s: %d sites, want %d", file, len(sites), len(want[file]))
		}
		for _, s := range sites {
			if !want[file][[3]any{s.Name, s.Line, s.Col}] {
				t.Errorf("%s: unexpected site %v", file, s)
			}
		}
	}

	// (b) Memo: same generation returns the SAME map object (identity via
	// the map header pointer), not a fresh rebuild, and does not bump gen.
	genBefore := idx.Generation()
	if p1, p2 := reflect.ValueOf(idx.SitesByFile()).Pointer(), reflect.ValueOf(idx.SitesByFile()).Pointer(); p1 != p2 {
		t.Error("same generation must reuse the cached inversion, not rebuild it")
	}
	if idx.Generation() != genBefore {
		t.Error("a cache HIT must not bump the generation")
	}

	// (c) Invalidation: a Refresh bumps gen; the inversion must reflect the
	// new content, not the stale cache.
	idx.Refresh(filepath.Join(dir, "b.go"), "go", []Hit{
		{Name: "Beta", Line: 2, Col: 6}, // Alpha() call removed
	})
	after := idx.SitesByFile()
	bpath := filepath.Join(dir, "b.go")
	for _, s := range after[bpath] {
		if s.Name == "Alpha" {
			t.Error("SitesByFile served a stale Alpha site after Refresh removed it")
		}
	}
}

// A vanished file's sites are dropped AND evicted during the build — the
// self-heal LookupExisting does per query, now done once per generation.
func TestSitesByFileEvictsVanishedFiles(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.go")
	if err := os.WriteFile(live, []byte("package a\nfunc Live() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(dir, "gone.go") // never created

	idx := NewIndex()
	idx.addHits(live, "go", []Hit{{Name: "Live", Line: 2, Col: 6}})
	idx.addHits(gone, "go", []Hit{{Name: "Ghost", Line: 1, Col: 1}})

	inv := idx.SitesByFile()
	if _, ok := inv[gone]; ok {
		t.Error("a vanished file must not appear in the inversion")
	}
	if len(inv[live]) != 1 {
		t.Errorf("the live file's site survives; got %d", len(inv[live]))
	}
	// Evicted from the index too — not just filtered from the view.
	if idx.NameFreq("Ghost") != 0 {
		t.Error("the vanished file's sites must be evicted from the index, not just hidden")
	}
}
