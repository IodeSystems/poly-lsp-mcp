package symbols

import (
	"container/list"
	"crypto/sha256"
	"sync"
)

// defaultCacheEntries is the LRU cap NewParseCache picks when the
// caller doesn't specify one. Sized so a workspace of a few thousand
// files fits without eviction, and a long-running agent walking many
// branches still has a stable memory ceiling.
const defaultCacheEntries = 5000

// ParseCache caches extractor output by (language, content hash). Two
// files with identical content — across branches, across worktrees,
// across renames — share one parse, so rebuilding the symbol index
// after a branch switch only re-parses files whose bytes actually
// changed.
//
// Eviction: LRU with a configurable entry cap. NewParseCache picks a
// sane default (5000) so long-running agents don't accrete entries
// unboundedly. NewParseCacheLRU(0) keeps the old "no eviction"
// behavior for tests that want predictable shape.
type ParseCache struct {
	mu sync.Mutex

	maxEntries int

	m  map[cacheKey]*list.Element
	ll *list.List // elements are *cacheEntry, newest at the front
}

type cacheKey struct {
	Language string
	Hash     [32]byte
}

type cacheEntry struct {
	key  cacheKey
	hits []Hit
}

// NewParseCache returns an LRU cache with the default entry cap.
// Sufficient for most workspaces; use NewParseCacheLRU(n) for an
// explicit cap or NewParseCacheLRU(0) for unbounded.
func NewParseCache() *ParseCache {
	return NewParseCacheLRU(defaultCacheEntries)
}

// NewParseCacheLRU returns a cache that evicts least-recently-used
// entries when it exceeds maxEntries. maxEntries == 0 disables
// eviction (the cache grows without bound — useful in tests, risky
// in long-running processes).
func NewParseCacheLRU(maxEntries int) *ParseCache {
	return &ParseCache{
		maxEntries: maxEntries,
		m:          map[cacheKey]*list.Element{},
		ll:         list.New(),
	}
}

// Get returns cached hits for (language, content) or false on miss.
// A hit moves the entry to the front of the LRU queue. The returned
// slice is the same underlying memory as the cached entry; callers
// must NOT mutate it.
func (c *ParseCache) Get(language string, content []byte) ([]Hit, bool) {
	if c == nil {
		return nil, false
	}
	key := cacheKey{Language: language, Hash: sha256.Sum256(content)}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(e)
	return e.Value.(*cacheEntry).hits, true
}

// Put stores hits for (language, content). Updating an existing key
// moves the entry to the front; new entries become the newest. When
// the cache exceeds maxEntries the oldest entry is evicted.
func (c *ParseCache) Put(language string, content []byte, hits []Hit) {
	if c == nil {
		return
	}
	key := cacheKey{Language: language, Hash: sha256.Sum256(content)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok {
		e.Value.(*cacheEntry).hits = hits
		c.ll.MoveToFront(e)
		return
	}
	entry := &cacheEntry{key: key, hits: hits}
	c.m[key] = c.ll.PushFront(entry)
	if c.maxEntries > 0 && c.ll.Len() > c.maxEntries {
		oldest := c.ll.Back()
		if oldest != nil {
			oldEntry := oldest.Value.(*cacheEntry)
			c.ll.Remove(oldest)
			delete(c.m, oldEntry.key)
		}
	}
}

// Len returns the number of entries currently in the cache. Diagnostic
// and test use only.
func (c *ParseCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
