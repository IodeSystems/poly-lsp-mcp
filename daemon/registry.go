package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/git"
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

	// lifecycle and statusDir are applied to every root's child-LSP
	// manager.
	lifecycle multiplex.Lifecycle
	statusDir string
	// lspRootsAuto / lspRootsMax: --lsp-roots for every root's server.
	lspRootsAuto bool
	lspRootsMax  int
	// usageLog is every root's node_query usage log; see mcp/usage.go.
	usageLog string
	// The daemon's index flags, for every root (see IndexDefaults). An
	// /open adds its own excludes and includes and may set the rest.
	idx IndexDefaults

	// hub fans each root's notifications out to the sessions holding it.
	hub *hub

	catalogOnce sync.Once
	catalog     []mcp.ToolDescriptor

	mu      sync.Mutex
	servers map[string]*entry
}

// OpenOptions are what a client chooses about the index of a root it
// opens: the stdio server's --exclude, --include, --index-budget,
// --index-anyway, --symlink-depth and --lsp-roots, carried by the proxy.
// They apply when the open builds the root; a root already built for
// another client keeps the options it was built with.
type OpenOptions struct {
	Excludes     []string `json:"excludes,omitempty"`
	Includes     []string `json:"includes,omitempty"`
	Budget       string   `json:"budget,omitempty"`       // "" = the daemon's
	IndexAnyway  bool     `json:"indexAnyway,omitempty"`  // or the daemon's
	SymlinkDepth *int     `json:"symlinkDepth,omitempty"` // nil = the daemon's
	LSPRoots     string   `json:"lspRoots,omitempty"`     // "", single or auto; "" = the daemon's
}

// IndexDefaults are the daemon's own index flags, for every root it hosts:
// --exclude, --include (after the config's include:), --index-budget,
// --index-anyway, --symlink-depth.
type IndexDefaults struct {
	Excludes     []string
	Includes     []string
	Budget       string
	Anyway       bool
	SymlinkDepth int
}

type entry struct {
	srv   *mcp.Server
	err   error
	ready chan struct{}
	opts  OpenOptions
	// lastUsed is the last request that named this root; a takeover warms
	// the recently used roots. Guarded by Registry.mu.
	lastUsed time.Time
	// holders are the sessions currently keeping this root warm (ref count =
	// len). A root is built on first acquire and evicted idleTimeout after the
	// last holder leaves — child LSPs (gopls et al., hundreds of MB) are the
	// expensive thing this frees. Guarded by Registry.mu.
	holders    map[string]struct{}
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
		idx:         IndexDefaults{SymlinkDepth: symbols.DefaultSymlinkDepth},
		hub:         newHub(),
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

// SaveCache persists the shared parse cache via a temp file + rename so a
// crash never leaves a half-written cache. The temp name carries the pid:
// during a takeover two daemons save the same cache. Errors are logged,
// never fatal — failing to save a cache must not fail shutdown.
func (r *Registry) SaveCache() {
	if r.cachePath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.cachePath), 0o700); err != nil {
		log.Printf("daemon: mkdir cache dir: %v", err)
		return
	}
	tmp := fmt.Sprintf("%s.%d.tmp", r.cachePath, os.Getpid())
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

// Get returns the server for a canonical root, building it on first use,
// WITHOUT taking a ref (for read/call paths whose session already holds
// the root via Acquire). root MUST already be resolved+allow-checked by the
// caller (the AllowList) — the registry trusts its key. A failed bring-up
// returns the server AND its error, so the caller can answer with the
// server's post-mortem (IndexFailure), as a stdio server does.
func (r *Registry) Get(root string) (*mcp.Server, error) { return r.get(root, "", nil, true) }

// Acquire is Get plus a ref: it records that `holder` (a session) is keeping
// root warm and cancels any pending eviction. The /open path calls it so a
// client formally holds its root for the session's lifetime; Release drops
// the ref on disconnect.
func (r *Registry) Acquire(holder, root string) (*mcp.Server, error) {
	return r.get(root, holder, nil, true)
}

// Open is Acquire with the client's index options. Unlike Get it retries
// a root whose bring-up failed — the daemon's equivalent of starting a new
// stdio server, which is the remedy a failed bring-up names.
func (r *Registry) Open(holder, root string, opts OpenOptions) (*mcp.Server, error) {
	return r.get(root, holder, &opts, true)
}

// get fetches-or-builds the server for root. When holder != "" it also
// registers the hold and cancels eviction (under the same lock as the
// fetch, so it can't race an in-flight evict of the same entry). opts !=
// nil is an /open: it rebuilds a failed entry. lsps false builds without
// starting child LSPs (a takeover's warm); a later get with lsps starts
// them.
func (r *Registry) get(root, holder string, opts *OpenOptions, lsps bool) (*mcp.Server, error) {
	r.mu.Lock()
	var carried map[string]struct{}
	if e, ok := r.servers[root]; ok {
		failed := false
		if opts != nil {
			select {
			case <-e.ready:
				failed = e.err != nil
			default:
			}
		}
		if !failed {
			if holder != "" {
				r.holdLocked(e, holder)
			}
			e.lastUsed = time.Now()
			r.mu.Unlock()
			if holder != "" {
				r.hub.replay(holder, root)
			}
			<-e.ready
			if e.err == nil && lsps {
				e.srv.StartLSPs()
			}
			return e.srv, e.err
		}
		// A failed root, opened again: build it afresh for this client,
		// keeping the holders the failed one had.
		if e.evictTimer != nil {
			e.evictTimer.Stop()
		}
		carried = e.holders
		delete(r.servers, root)
		r.hub.forget(root)
	}
	e := &entry{ready: make(chan struct{}), holders: map[string]struct{}{}, lastUsed: time.Now()}
	for h := range carried {
		e.holders[h] = struct{}{}
	}
	if opts != nil {
		e.opts = *opts
	}
	if holder != "" {
		e.holders[holder] = struct{}{}
	}
	r.servers[root] = e
	r.mu.Unlock()

	srv := r.build(root, e.opts)
	err := srv.BringUp(lsps)
	e.srv, e.err = srv, err
	close(e.ready)

	if err != nil {
		log.Printf("daemon: open root %s failed: %v", root, err)
	} else {
		log.Printf("daemon: opened root %s", root)
	}
	r.mu.Lock()
	// An unheld root (a one-shot /query, a takeover's warm) is evicted
	// like a released one, so nothing nobody holds stays warm forever.
	if cur, ok := r.servers[root]; ok && cur == e && len(e.holders) == 0 {
		r.scheduleEvictLocked(root, e)
	}
	r.mu.Unlock()
	return srv, err
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

// heldBy lists the roots holder holds.
func (r *Registry) heldBy(holder string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for root, e := range r.servers {
		if _, ok := e.holders[holder]; ok {
			out = append(out, root)
		}
	}
	return out
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
	e.evictTimer = time.AfterFunc(r.idleTimeout, func() { r.evict(root, e) })
}

// evict shuts down a root's server (index + child LSPs + watcher) IF it is
// still unheld. Re-checks under mu, so an Acquire that landed after the timer
// fired (but before this ran) safely cancels the eviction. The shared parse
// cache is daemon-owned, so a re-open still comes up warm on parses.
func (r *Registry) evict(root string, want *entry) {
	r.mu.Lock()
	e, ok := r.servers[root]
	if !ok || e != want || len(e.holders) > 0 {
		r.mu.Unlock()
		return // gone already, rebuilt, or re-acquired
	}
	delete(r.servers, root)
	e.evictTimer = nil
	r.mu.Unlock()
	r.hub.forget(root)

	<-e.ready
	if e.srv != nil {
		e.srv.Shutdown()
	}
	log.Printf("daemon: evicted idle root %s", root)
}

// build constructs (but does not bring up) a server for root with the
// shared cache, daemon-wide policy and the client's index options. Schemas
// are detected per root when auto-schemas is on, since detection is
// workspace-specific.
func (r *Registry) build(root string, opts OpenOptions) *mcp.Server {
	schemas := r.cfg.Schemas
	if r.cfg.AutoSchemas {
		if detected := config.DetectSchemas(root, r.cfg.Schemas); len(detected) > 0 {
			schemas = append(append([]config.Schema{}, r.cfg.Schemas...), detected...)
		}
	}
	srv := mcp.New(r.reg, root, r.cfg.Bindings, schemas)
	srv.SetQuiet(true) // one daemon log for every root: the notifications carry the report
	srv.SetReadOnly(r.readOnly)
	srv.SetValidateEdits(r.validate)
	srv.SetParseCache(r.cache)
	srv.SetUsageLog(r.usageLog)
	srv.SetNotifier(func(method string, params map[string]any) { r.notify(root, method, params) })
	srv.SetExcludes(append(append([]string{}, r.idx.Excludes...), opts.Excludes...))
	srv.SetIncludes(append(append(append([]string{}, r.cfg.Include...), r.idx.Includes...), opts.Includes...))
	srv.SetIndexAnyway(r.idx.Anyway || opts.IndexAnyway)
	depth := r.idx.SymlinkDepth
	if opts.SymlinkDepth != nil {
		depth = *opts.SymlinkDepth
	}
	srv.SetSymlinkDepth(depth)
	budget := r.idx.Budget
	if opts.Budget != "" {
		budget = opts.Budget
	}
	// Validated at the boundary (checkOpenOptions) and at daemon start.
	if err := srv.SetIndexBudget(budget); err != nil {
		log.Printf("daemon: root %s: %v (unbounded)", root, err)
	}
	// One Manager per root: child LSPs bind to the module root and can't
	// be shared across roots.
	mgr := multiplex.NewManager(r.reg)
	mgr.Lifecycle = r.lifecycle
	mgr.StatusDir = r.statusDir
	srv.SetManager(mgr)
	auto := r.lspRootsAuto
	switch opts.LSPRoots {
	case "auto":
		auto = true
	case "single":
		auto = false
	}
	srv.SetLSPRoots(auto, r.lspRootsMax)
	return srv
}

// checkOpenOptions refuses what the stdio server's flag parsing refuses.
func checkOpenOptions(o OpenOptions) error {
	for _, d := range o.Excludes {
		if git.CleanExclude(d) == "" {
			return fmt.Errorf("exclude %q: want a directory inside the root, relative to it", d)
		}
	}
	if o.Budget != "" {
		if err := mcp.New(nil, "", nil, nil).SetIndexBudget(o.Budget); err != nil {
			return err
		}
	}
	if o.SymlinkDepth != nil && *o.SymlinkDepth < 0 {
		return fmt.Errorf("symlinkDepth %d: want 0 or more", *o.SymlinkDepth)
	}
	switch o.LSPRoots {
	case "", "single", "auto":
	default:
		return fmt.Errorf("lspRoots %q: want single or auto", o.LSPRoots)
	}
	return nil
}

// notify is every hosted server's notification sink: it goes to the
// sessions that hold the root, and the latest index and setup state is
// kept for a session that attaches later.
func (r *Registry) notify(root, method string, params map[string]any) {
	raw, err := json.Marshal(params)
	if err != nil {
		return
	}
	logger, _ := params["logger"].(string)
	r.mu.Lock()
	var to []string
	if e, ok := r.servers[root]; ok {
		for h := range e.holders {
			to = append(to, h)
		}
	}
	r.mu.Unlock()
	r.hub.publish(root, logger, method, raw, to)
}

// Tools is the tool catalog a hosted root serves. It does not depend on
// the index, so it is answered without building one: tools/list must not
// wait for a bring-up that a tool call has not asked for yet.
func (r *Registry) Tools() []mcp.ToolDescriptor {
	r.catalogOnce.Do(func() {
		srv := mcp.New(r.reg, "", nil, nil)
		srv.SetReadOnly(r.readOnly)
		r.catalog = srv.Tools()
	})
	return slices.Clone(r.catalog)
}

// SetIndexDefaults sets the daemon's index flags for roots built after
// the call. Call before Serve.
func (r *Registry) SetIndexDefaults(d IndexDefaults) { r.idx = d }

// SetLSPRoots sets --lsp-roots for roots built after the call: auto = a
// child LSP per project root, at most max running per language. Call
// before Serve.
func (r *Registry) SetLSPRoots(auto bool, max int) {
	r.lspRootsAuto, r.lspRootsMax = auto, max
}

// SetLifecycle sets the child-LSP lifecycle policy for roots built after
// the call. Call before Serve.
func (r *Registry) SetLifecycle(lc multiplex.Lifecycle) { r.lifecycle = lc }

// SetStatusDir makes roots built after the call write child-LSP status
// files there. Call before Serve.
func (r *Registry) SetStatusDir(dir string) { r.statusDir = dir }

// SetUsageLog makes roots built after the call append node_query usage to
// path. Call before Serve.
func (r *Registry) SetUsageLog(path string) { r.usageLog = path }

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

// RecentRoots are the roots a takeover warms: held by a session now, or
// requested within window, and up. Warming every root the daemon ever
// opened would cost the swap the memory of roots nobody is using.
func (r *Registry) RecentRoots(window time.Duration) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for root, e := range r.servers {
		select {
		case <-e.ready:
			if e.err != nil {
				continue
			}
		default:
		}
		if len(e.holders) > 0 || time.Since(e.lastUsed) <= window {
			out = append(out, root)
		}
	}
	slices.Sort(out)
	return out
}

// OpenBatches counts the open (commit:false) edit batches on every root.
func (r *Registry) OpenBatches() int {
	r.mu.Lock()
	servers := make([]*entry, 0, len(r.servers))
	for _, e := range r.servers {
		servers = append(servers, e)
	}
	r.mu.Unlock()
	n := 0
	for _, e := range servers {
		select {
		case <-e.ready:
			if e.srv != nil {
				n += e.srv.OpenBatches()
			}
		default:
		}
	}
	return n
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
