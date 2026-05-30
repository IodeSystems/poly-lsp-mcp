package symbols

import (
	"container/list"
	"crypto/sha256"
	"encoding/gob"
	"fmt"
	"io"
	"sync"
)

// cacheFileVersion is bumped whenever the on-disk format changes in a
// non-backwards-compatible way. Load returns ErrCacheVersion on
// mismatch so callers can drop the file and start fresh instead of
// reading garbage.
const cacheFileVersion uint32 = 1

// ErrCacheVersion is returned by ParseCache.Load when the on-disk
// version doesn't match cacheFileVersion. Callers typically respond
// by discarding the file and rebuilding from scratch.
var ErrCacheVersion = fmt.Errorf("symbols: cache file version mismatch")

// cacheFile is the on-disk shape. Version-tagged so future schema
// changes can be detected (and the file regenerated) without
// corrupting indexes built from stale data.
type cacheFile struct {
	Version uint32
	Entries []cacheFileEntry
}

type cacheFileEntry struct {
	Language string
	Hash     [32]byte
	Hits     []Hit
}

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
	c.putRaw(cacheKey{Language: language, Hash: sha256.Sum256(content)}, hits)
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

// Save serializes the cache to w in a gob-encoded version-tagged
// format. Iteration order is newest-first (front of the LRU queue)
// so a subsequent Load reproduces the same LRU shape: the first
// entry Loaded becomes the newest, matching Save's traversal order.
func (c *ParseCache) Save(w io.Writer) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	entries := make([]cacheFileEntry, 0, c.ll.Len())
	for e := c.ll.Front(); e != nil; e = e.Next() {
		ce := e.Value.(*cacheEntry)
		entries = append(entries, cacheFileEntry{
			Language: ce.key.Language,
			Hash:     ce.key.Hash,
			Hits:     ce.hits,
		})
	}

	return gob.NewEncoder(w).Encode(cacheFile{
		Version: cacheFileVersion,
		Entries: entries,
	})
}

// Load reads a previously-saved cache from r and merges its entries
// into c. The on-disk newest-first ordering is reproduced: the entry
// that was newest at Save time ends up at the front of the LRU queue.
// Entries already present in c are updated (their position promotes)
// rather than duplicated.
//
// Returns ErrCacheVersion when the file's schema version doesn't
// match the current build — callers typically catch that and drop
// the file. Other decode errors propagate as-is.
func (c *ParseCache) Load(r io.Reader) error {
	if c == nil {
		return fmt.Errorf("symbols: Load on nil cache")
	}
	var file cacheFile
	if err := gob.NewDecoder(r).Decode(&file); err != nil {
		return fmt.Errorf("decode cache file: %w", err)
	}
	if file.Version != cacheFileVersion {
		return ErrCacheVersion
	}
	// Iterate back-to-front so the LAST putRaw ends up at the LRU
	// front, matching the entry that was newest at Save time.
	for i := len(file.Entries) - 1; i >= 0; i-- {
		e := file.Entries[i]
		c.putRaw(cacheKey{Language: e.Language, Hash: e.Hash}, e.Hits)
	}
	return nil
}

// putRaw is the hash-keyed insertion the public Put builds on. Load
// uses it directly so it can replay entries whose original content
// isn't available anymore.
func (c *ParseCache) putRaw(key cacheKey, hits []Hit) {
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
