package symbols

import "testing"

// Content-hash keying is what makes invalidation free: an edited file simply
// misses, and byte-identical files — across branches, worktrees, renames —
// share one entry.
func TestSymbolCacheKeysOnContent(t *testing.T) {
	c := NewSymbolCache()
	src := []byte("package p\n\nfunc A() {}\n")
	if _, ok := c.Get("go", src); ok {
		t.Fatal("empty cache should miss")
	}
	syms, err := FileSymbols("go", src)
	if err != nil {
		t.Fatal(err)
	}
	c.Put("go", src, syms)

	got, ok := c.Get("go", src)
	if !ok || len(got) != len(syms) {
		t.Errorf("identical content should hit: ok=%v got=%d want=%d", ok, len(got), len(syms))
	}
	// A different language is a different entry, same bytes.
	if _, ok := c.Get("python", src); ok {
		t.Error("the language is part of the key")
	}
	// Edited content misses — that IS the invalidation.
	if _, ok := c.Get("go", []byte("package p\n\nfunc B() {}\n")); ok {
		t.Error("changed content must miss")
	}
}

// Bounded, so a long-running agent cannot accrete entries unboundedly.
func TestSymbolCacheEvictsOldest(t *testing.T) {
	c := NewSymbolCache()
	c.maxEntries = 2
	for _, name := range []string{"A", "B", "C"} {
		src := []byte("package p\n\nfunc " + name + "() {}\n")
		c.Put("go", src, []Symbol{{Sym: name}})
	}
	if c.Len() != 2 {
		t.Errorf("cache should stay at its cap; got %d", c.Len())
	}
	if _, ok := c.Get("go", []byte("package p\n\nfunc A() {}\n")); ok {
		t.Error("the oldest entry should have been evicted")
	}
	if _, ok := c.Get("go", []byte("package p\n\nfunc C() {}\n")); !ok {
		t.Error("the newest entry should survive")
	}
}
