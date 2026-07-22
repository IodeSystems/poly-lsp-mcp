package mcp

import "testing"

// The definition cache's mechanics: memoize within a generation, drop the
// whole thing when the index moves on, and never store a stale-gen answer.
func TestDefCacheGenInvalidation(t *testing.T) {
	s := &Server{}
	hit := defEntry{defAbs: "/x/a.go", defLine: 10, found: true}

	// First get at gen 1 is a miss and arms the cache for gen 1.
	if _, ok := s.defCacheGet(1, "k"); ok {
		t.Fatal("empty cache must miss")
	}
	s.defCachePut(1, "k", hit)
	if got, ok := s.defCacheGet(1, "k"); !ok || got != hit {
		t.Fatalf("same-gen get must hit with the stored value; got %+v ok=%v", got, ok)
	}

	// A new generation drops everything — the old key is gone.
	if _, ok := s.defCacheGet(2, "k"); ok {
		t.Fatal("a generation bump must invalidate the cache")
	}

	// A put tagged with a now-stale gen must not poison the fresh cache.
	s.defCacheGet(3, "other")  // arm gen 3
	s.defCachePut(2, "k", hit) // stale gen 2
	if _, ok := s.defCacheGet(3, "k"); ok {
		t.Error("a stale-gen put must not land in the current-gen cache")
	}
}

// A no-answer (found=false) is cached too, so an unresolvable site is not
// re-asked within the same generation.
func TestDefCacheCachesNegatives(t *testing.T) {
	s := &Server{}
	s.defCacheGet(1, "miss") // arm gen 1 (as resolveDefinition does before a put)
	s.defCachePut(1, "miss", defEntry{})
	if d, ok := s.defCacheGet(1, "miss"); !ok || d.found {
		t.Errorf("a negative answer must cache as a hit with found=false; got %+v ok=%v", d, ok)
	}
}
