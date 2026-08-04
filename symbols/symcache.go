package symbols

import (
	"container/list"
	"crypto/sha256"
	"sync"
)

// SymbolCache memoizes FileSymbols output by (language, content hash).
//
// ParseCache does the same for the reference-site extractor, and the two are
// deliberately separate: they cache different products of the same parse, and
// a query that walks the node tree wants Symbols while the index wants Hits.
//
// It exists because the symbol tree was rebuilt from scratch on EVERY query.
// buildTree() makes a fresh engine per node_query and the old memo lived on
// that engine, so a long-running server re-read and re-parsed every file a
// selector descended into, every time. Profiling a cold query put 64% of CPU
// in tree-sitter's ParseCtx — all of it repeat work after the first query.
//
// Content-hash keying makes invalidation free: an edited file simply misses,
// and two files with identical bytes — across branches, worktrees, renames —
// share one entry.
type SymbolCache struct {
	mu         sync.Mutex
	maxEntries int
	m          map[cacheKey]*list.Element
	ll         *list.List
}

type symEntry struct {
	key  cacheKey
	syms []Symbol
}

// NewSymbolCache returns a cache with the same default cap as ParseCache, so
// a long-running agent cannot accrete entries unboundedly.
func NewSymbolCache() *SymbolCache {
	return &SymbolCache{maxEntries: defaultCacheEntries, m: map[cacheKey]*list.Element{}, ll: list.New()}
}

// Get returns cached symbols for (language, content) or false on miss. The
// returned slice is the cached memory; callers must NOT mutate it.
func (c *SymbolCache) Get(language string, content []byte) ([]Symbol, bool) {
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
	return e.Value.(*symEntry).syms, true
}

// Put stores symbols for (language, content), evicting the oldest entry when
// the cache is full.
func (c *SymbolCache) Put(language string, content []byte, syms []Symbol) {
	if c == nil {
		return
	}
	key := cacheKey{Language: language, Hash: sha256.Sum256(content)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok {
		c.ll.MoveToFront(e)
		e.Value.(*symEntry).syms = syms
		return
	}
	c.m[key] = c.ll.PushFront(&symEntry{key: key, syms: syms})
	if c.maxEntries > 0 && c.ll.Len() > c.maxEntries {
		if old := c.ll.Back(); old != nil {
			c.ll.Remove(old)
			delete(c.m, old.Value.(*symEntry).key)
		}
	}
}

// Len reports the entry count. Diagnostic and test use only.
func (c *SymbolCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
