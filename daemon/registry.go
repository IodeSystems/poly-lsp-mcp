package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// Registry maps an absolute workspace root → a warm *mcp.Server, mirroring
// raglit's OpenScopedRegistry but keyed by canonical path instead of a
// project name (our roots are already unique paths). One shared,
// content-keyed ParseCache backs every server so identical file bytes
// across roots (e.g. a worktree branch and its parent) parse once. Child
// LSPs stay per-root — gopls binds to a module root and can't be shared —
// so each server gets its own Manager.
//
// Opening is lazy and serialized per root (never across roots): the first
// caller builds the index + spawns child LSPs behind a ready channel;
// concurrent callers for the same root wait on it, callers for other
// roots proceed in parallel.
type Registry struct {
	cfg       *config.Config
	reg       *config.Registry
	cache     *symbols.ParseCache
	cachePath string // daemon-owned persistence of the SHARED cache; "" disables
	readOnly  bool
	validate  bool

	// idleTimeout is how long a root with zero holders lingers before its
	// server (index + child LSPs + watcher) is evicted. 0 disables eviction
	// (a root, once opened, stays warm forever — the pre-step-5 behavior).
	idleTimeout time.Duration

	mu      sync.Mutex
	servers map[string]*entry
}

type entry struct {
	srv   *mcp.Server
	err   error
	ready chan struct{}
	// holders are the sessions currently keeping this root warm (ref count =
	// len). A root is built on first acquire and evicted idleTimeout after the
	// last holder leaves — child LSPs (gopls et al., hundreds of MB) are the
	// expensive thing this frees. Guarded by Registry.mu.
	holders   map[string]struct{}
	evictTimer *time.Timer
}

// NewRegistry builds the host registry. cfg/reg come from the daemon's
// config load; readOnly/validate are the daemon-wide defaults applied to
// every hosted server (per-connection policy overrides are a later slice).
func NewRegistry(cfg *config.Config, reg *config.Registry, readOnly, validate bool) *Registry {
	return &Registry{
		cfg:         cfg,
		reg:         reg,
		cache:       symbols.NewParseCache(),
		cachePath:   filepath.Join(ConfigHome(), "cache.gob"),
		readOnly:    readOnly,
		validate:    validate,
		idleTimeout: defaultIdleTimeout,
		servers:     map[string]*entry{},
	}
}

// defaultIdleTimeout is how long an unheld root stays warm before eviction.
// Long enough that a client reconnecting (a new agent turn, an editor
// restart) finds it warm; short enough that abandoned roots don't pin a
// gopls fleet.
const defaultIdleTimeout = 5 * time.Minute

// LoadCache seeds the shared parse cache from disk at daemon startup so a
// restarted daemon comes up warm instead of re-parsing every file. A
// missing or unreadable file is fine (first run / version mismatch) — the
// cache stays empty and rebuilds. One load, daemon-wide (not the N racing
// per-root loads the stdio path does), because the cache is shared.
func (r *Registry) LoadCache() {
	if r.cachePath == "" {
		return
	}
	f, err := os.Open(r.cachePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("daemon: open cache %s: %v", r.cachePath, err)
		}
		return
	}
	defer f.Close()
	if err := r.cache.Load(f); err != nil {
		log.Printf("daemon: load cache %s: %v (continuing empty)", r.cachePath, err)
		return
	}
	log.Printf("daemon: loaded %d cache entries from %s", r.cache.Len(), r.cachePath)
}

// SaveCache persists the shared parse cache at daemon shutdown via a
// temp-file + rename so a crash never leaves a half-written cache. One
// save, daemon-wide. Errors are logged, never fatal — failing to save a
// cache must not fail shutdown.
func (r *Registry) SaveCache() {
	if r.cachePath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.cachePath), 0o700); err != nil {
		log.Printf("daemon: mkdir cache dir: %v", err)
		return
	}
	tmp := r.cachePath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		log.Printf("daemon: create cache tmp: %v", err)
		return
	}
	if err := r.cache.Save(f); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		log.Printf("daemon: save cache: %v", err)
		return
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		log.Printf("daemon: close cache tmp: %v", err)
		return
	}
	if err := os.Rename(tmp, r.cachePath); err != nil {
		_ = os.Remove(tmp)
		log.Printf("daemon: rename cache: %v", err)
		return
	}
	log.Printf("daemon: saved %d cache entries to %s", r.cache.Len(), r.cachePath)
}

// Get returns the warm server for a canonical root, building it on first
// use, WITHOUT taking a ref (for read/call paths whose session already holds
// the root via Acquire). root MUST already be resolved+allow-checked by the
// caller (the AllowList) — the registry trusts its key.
func (r *Registry) Get(root string) (*mcp.Server, error) { return r.get(root, "") }

// Acquire is Get plus a ref: it records that `holder` (a session) is keeping
// root warm and cancels any pending eviction. The /open path calls it so a
// client formally holds its root for the session's lifetime; Release drops
// the ref on disconnect.
func (r *Registry) Acquire(holder, root string) (*mcp.Server, error) {
	return r.get(root, holder)
}

// get fetches-or-builds the server for root. When holder != "" it also
// registers the hold and cancels eviction (under the same lock as the
// fetch, so it can't race an in-flight evict of the same entry).
func (r *Registry) get(root, holder string) (*mcp.Server, error) {
	r.mu.Lock()
	if e, ok := r.servers[root]; ok {
		if holder != "" {
			r.holdLocked(e, holder)
		}
		r.mu.Unlock()
		<-e.ready
		return e.srv, e.err
	}
	e := &entry{ready: make(chan struct{}), holders: map[string]struct{}{}}
	if holder != "" {
		e.holders[holder] = struct{}{}
	}
	r.servers[root] = e
	r.mu.Unlock()

	srv := r.build(root)
	e.srv, e.err = srv, srv.Init()
	close(e.ready)

	if e.err != nil {
		// Drop the failed entry so a later call can retry a clean build
		// (a transient index/LSP failure shouldn't poison the root
		// forever).
		r.mu.Lock()
		delete(r.servers, root)
		r.mu.Unlock()
		log.Printf("daemon: open root %s failed: %v", root, e.err)
		return nil, e.err
	}
	log.Printf("daemon: opened root %s", root)
	return e.srv, nil
}

// holdLocked adds a holder and cancels any pending eviction. Caller holds mu.
func (r *Registry) holdLocked(e *entry, holder string) {
	if e.holders == nil {
		e.holders = map[string]struct{}{}
	}
	e.holders[holder] = struct{}{}
	if e.evictTimer != nil {
		e.evictTimer.Stop()
		e.evictTimer = nil
	}
}

// Release drops `holder`'s ref on every root it holds. A root whose last
// holder just left is scheduled for eviction after idleTimeout (a
// reconnecting client cancels it via Acquire). Called when a client's
// /session/watch connection drops.
func (r *Registry) Release(holder string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for root, e := range r.servers {
		if _, ok := e.holders[holder]; !ok {
			continue
		}
		delete(e.holders, holder)
		if len(e.holders) == 0 {
			r.scheduleEvictLocked(root, e)
		}
	}
}

// scheduleEvictLocked arms the idle-eviction timer for an unheld root. Caller
// holds mu. idleTimeout <= 0 disables eviction.
func (r *Registry) scheduleEvictLocked(root string, e *entry) {
	if r.idleTimeout <= 0 {
		return
	}
	if e.evictTimer != nil {
		e.evictTimer.Stop()
	}
	e.evictTimer = time.AfterFunc(r.idleTimeout, func() { r.evict(root) })
}

// evict shuts down a root's server (index + child LSPs + watcher) IF it is
// still unheld. Re-checks under mu, so an Acquire that landed after the timer
// fired (but before this ran) safely cancels the eviction. The shared parse
// cache is daemon-owned, so a re-open still comes up warm on parses.
func (r *Registry) evict(root string) {
	r.mu.Lock()
	e, ok := r.servers[root]
	if !ok || len(e.holders) > 0 {
		r.mu.Unlock()
		return // gone already, or re-acquired
	}
	delete(r.servers, root)
	e.evictTimer = nil
	r.mu.Unlock()

	<-e.ready
	if e.srv != nil {
		e.srv.Shutdown()
	}
	log.Printf("daemon: evicted idle root %s", root)
}

// build constructs (but does not Init) a server for root with the shared
// cache and daemon-wide policy. Schemas are detected per root when
// auto-schemas is on, since detection is workspace-specific.
func (r *Registry) build(root string) *mcp.Server {
	schemas := r.cfg.Schemas
	if r.cfg.AutoSchemas {
		if detected := config.DetectSchemas(root, r.cfg.Schemas); len(detected) > 0 {
			schemas = append(append([]config.Schema{}, r.cfg.Schemas...), detected...)
		}
	}
	srv := mcp.New(r.reg, root, r.cfg.Bindings, schemas)
	srv.SetReadOnly(r.readOnly)
	srv.SetValidateEdits(r.validate)
	srv.SetParseCache(r.cache)
	// One Manager per root: child LSPs bind to the module root and can't
	// be shared across roots.
	srv.SetManager(multiplex.NewManager(r.reg))
	return srv
}

// LanguageForPath derives the tree-sitter grammar name from a path's
// extension (via the config registry), or "" if none is registered. Used
// by the /filesymbols op to fragment a file whose language the caller
// didn't name.
func (r *Registry) LanguageForPath(path string) string {
	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	if ext == "" {
		return ""
	}
	lang := r.reg.LookupByExt(ext)
	if lang == nil {
		return ""
	}
	return lang.Name
}

// Roots lists the currently-open canonical roots (for /health).
func (r *Registry) Roots() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.servers))
	for k := range r.servers {
		out = append(out, k)
	}
	return out
}

// RollbackSession discards any open edit batch the session holds on ANY
// hosted root, reverting its staged files to their pre-batch bytes. Called
// when a client's /session/watch connection drops, so a vanished client
// can't strand a broken intermediate on disk. A session's batches are few
// (one per root it edited) and roots are few, so a sweep over built servers
// is cheap; unbuilt roots hold no batch. Returns the roots actually rolled
// back, for logging.
func (r *Registry) RollbackSession(sess string) []string {
	r.mu.Lock()
	servers := make([]*entry, 0, len(r.servers))
	for _, e := range r.servers {
		servers = append(servers, e)
	}
	r.mu.Unlock()
	var rolled []string
	for _, e := range servers {
		select {
		case <-e.ready:
		default:
			continue // still building — no batch to strand yet
		}
		if e.srv == nil {
			continue
		}
		if n, ok := e.srv.RollbackSession(sess); ok {
			rolled = append(rolled, fmt.Sprintf("%s(%d)", e.srv.Root(), n))
		}
	}
	return rolled
}

// Close tears down every hosted server (child LSPs + watchers). Called on
// daemon shutdown.
func (r *Registry) Close() {
	r.mu.Lock()
	servers := make([]*entry, 0, len(r.servers))
	for _, e := range r.servers {
		if e.evictTimer != nil {
			e.evictTimer.Stop()
		}
		servers = append(servers, e)
	}
	r.servers = map[string]*entry{}
	r.mu.Unlock()
	for _, e := range servers {
		<-e.ready
		if e.srv != nil {
			e.srv.Shutdown()
		}
	}
}
