package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

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

	mu      sync.Mutex
	servers map[string]*entry
}

type entry struct {
	srv   *mcp.Server
	err   error
	ready chan struct{}
}

// NewRegistry builds the host registry. cfg/reg come from the daemon's
// config load; readOnly/validate are the daemon-wide defaults applied to
// every hosted server (per-connection policy overrides are a later slice).
func NewRegistry(cfg *config.Config, reg *config.Registry, readOnly, validate bool) *Registry {
	return &Registry{
		cfg:       cfg,
		reg:       reg,
		cache:     symbols.NewParseCache(),
		cachePath: filepath.Join(ConfigHome(), "cache.gob"),
		readOnly:  readOnly,
		validate:  validate,
		servers:   map[string]*entry{},
	}
}

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
// use. root MUST already be resolved+allow-checked by the caller (the
// AllowList) — the registry trusts its key.
func (r *Registry) Get(root string) (*mcp.Server, error) {
	r.mu.Lock()
	if e, ok := r.servers[root]; ok {
		r.mu.Unlock()
		<-e.ready
		return e.srv, e.err
	}
	e := &entry{ready: make(chan struct{})}
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
