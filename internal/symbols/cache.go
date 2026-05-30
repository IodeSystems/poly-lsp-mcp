package symbols

import (
	"crypto/sha256"
	"sync"
)

// ParseCache caches extractor output by (language, content hash). Two
// files with identical content — across branches, across worktrees,
// across renames — share one parse, so rebuilding the symbol index
// after a branch switch only re-parses files whose bytes actually
// changed.
//
// Key choice: language + SHA-256 of content. Language is part of the
// key because the same bytes parsed as Go vs Markdown yield different
// hits. SHA-256 is overkill cryptographically but stdlib-only and
// fast enough; xxhash would shave wall-clock but add a dep.
//
// Eviction: none in v0.1. A long-running server that walks many
// branches will grow its cache unboundedly. In practice agent
// processes restart often enough that this isn't a memory hazard, and
// adding an LRU is a clean follow-up slice when it becomes one.
type ParseCache struct {
	mu sync.RWMutex
	m  map[cacheKey][]Hit
}

type cacheKey struct {
	Language string
	Hash     [32]byte
}

// NewParseCache returns an empty cache.
func NewParseCache() *ParseCache {
	return &ParseCache{m: map[cacheKey][]Hit{}}
}

// Get returns cached hits for (language, content) or false on miss.
// The returned slice is the same underlying memory as the cached
// entry; callers must NOT mutate it. Hit lists are read-only by
// convention through the package.
func (c *ParseCache) Get(language string, content []byte) ([]Hit, bool) {
	if c == nil {
		return nil, false
	}
	key := cacheKey{Language: language, Hash: sha256.Sum256(content)}
	c.mu.RLock()
	defer c.mu.RUnlock()
	hits, ok := c.m[key]
	return hits, ok
}

// Put stores hits for (language, content). Safe to call concurrently;
// concurrent Puts for the same key resolve to last-write-wins, which
// is fine because the value is deterministic for identical input.
func (c *ParseCache) Put(language string, content []byte, hits []Hit) {
	if c == nil {
		return
	}
	key := cacheKey{Language: language, Hash: sha256.Sum256(content)}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = hits
}

// Len returns the number of entries currently in the cache. Diagnostic
// and test use only.
func (c *ParseCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.m)
}
