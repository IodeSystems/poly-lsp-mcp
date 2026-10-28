package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
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

	// A root's lifetime follows its clients (holders). lspGrace: its child
	// LSPs stop this long after its last client left; the index stays.
	// idleCap: the client-less roots kept warm (index and watcher) are an
	// LRU bounded by their estimated resident bytes (rootResident); past
	// it the least recently used is evicted. A root with a client is never
	// evicted. idleCap < 0 = no bound.
	lspGrace time.Duration
	idleCap  int64

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

	// The user's servers.yaml, re-read on each client open (see
	// SetServersFile, checkServers). gen counts the registries cfg/reg
	// have been: a root built under an older one is rebuilt on its next
	// open. cfg, reg and gen are guarded by mu.
	serversMu   sync.Mutex
	serversPath string
	serversBase []config.Language // nil: not watched
	serversSeen serversStamp
	gen         uint64

	mu      sync.Mutex
	servers map[string]*entry
}

// serversStamp is what checkServers compares: the file's mtime and size,
// then its hash.
type serversStamp struct {
	mod  time.Time
	size int64
	sum  [32]byte
	ok   bool // the file existed
}

// OpenOptions are what a client chooses about the index of a root it
// opens: the stdio server's --exclude, --include, --index-budget,
// --index-anyway, --symlink-depth and --lsp-roots, carried by the proxy.
// One index per root, and the last open wins: an open whose options
// differ from those the root was built with rebuilds it with its own, and
// every session on the root is told (see get).
type OpenOptions struct {
	Excludes     []string `json:"excludes,omitempty"`
	Includes     []string `json:"includes,omitempty"`
	Budget       string   `json:"budget,omitempty"`       // "" = the daemon's
	IndexAnyway  bool     `json:"indexAnyway,omitempty"`  // or the daemon's
	SymlinkDepth *int     `json:"symlinkDepth,omitempty"` // nil = the daemon's
	LSPRoots     string   `json:"lspRoots,omitempty"`     // "", single or auto; "" = the daemon's
	// LSPOff / LSPOn are this client's child-LSP switch (mcp --lsp-off,
	// --lsp-on; see mcp/lsp_off.go). Per session, not per root: they do
	// not change the build, so a different switch never rebuilds.
	LSPOff []string `json:"lspOff,omitempty"`
	LSPOn  []string `json:"lspOn,omitempty"`
	// FormatOnEdit / NoFormatOnEdit are this client's format-on-edit
	// switch over each language's format_on_edit (mcp --format-on-edit,
	// --no-format-on-edit; see mcp/format.go). Per session, as LSPOff.
	FormatOnEdit   []string `json:"formatOnEdit,omitempty"`
	NoFormatOnEdit []string `json:"noFormatOnEdit,omitempty"`
}

// lspPrefs is o's child-LSP and format-on-edit switch.
func (o OpenOptions) lspPrefs() mcp.LSPPrefs {
	return mcp.LSPPrefs{On: o.LSPOn, Off: o.LSPOff, FormatOn: o.FormatOnEdit, FormatOff: o.NoFormatOnEdit}
}

// buildOptions is o without the per-session switch: what a root is built
// with.
func (o OpenOptions) buildOptions() OpenOptions {
	o.LSPOff, o.LSPOn = nil, nil
	o.FormatOnEdit, o.NoFormatOnEdit = nil, nil
	return o
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
	// holders are the sessions currently keeping this root warm (ref
	// count = len): its clients. A root is built on first acquire; when the
	// last holder leaves, lspTimer stops its child LSPs (gopls et al.,
	// hundreds of MB) after Registry.lspGrace, and the root joins the LRU
	// of client-less roots (see trimLocked). Guarded by Registry.mu.
	holders     map[string]struct{}
	lspTimer    *time.Timer
	lspsStopped bool
	// size is rootResident once the bring-up ended, 0 before.
	size int64
	// lsp is each holder's child-LSP switch, from its open; carried to a
	// rebuild. gen is the registry generation the root was built with.
	// Guarded by Registry.mu.
	lsp map[string]mcp.LSPPrefs
	gen uint64
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
		lspGrace:  DefaultRootLSPGrace,
		idleCap:   DefaultIdleRootsMB << 20,
		idx:       IndexDefaults{SymlinkDepth: symbols.DefaultSymlinkDepth},
		hub:       newHub(),
		servers:   map[string]*entry{},
	}
}

// The root-lifetime defaults (daemon --root-lsp-grace, --idle-roots-mb).
// The grace is long enough that a client reconnecting (an upgrade, an
// editor restart) finds the child LSPs warm; the cap keeps a few large
// indexes for clients that come back without letting abandoned roots
// accrete.
const (
	DefaultRootLSPGrace = time.Minute
	DefaultIdleRootsMB  = 1024
)

// SetRootLifetime sets how long a root's child LSPs outlive its last
// client (grace) and the bound, in bytes, on the client-less roots kept
// warm (idleCap; < 0 = none, 0 = keep none). Call before Serve.
func (r *Registry) SetRootLifetime(grace time.Duration, idleCap int64) {
	r.lspGrace, r.idleCap = grace, idleCap
}

// residentFactor is the live Go heap of a hosted root per byte its index
// read. Measured 2026-10-24 on this repo (393 files, 3.3 MiB): 5.8 for the
// index with its parse-cache entries (cold cache), 3.6 for the index alone
// (TestMeasureRootResident, -tags measure, 2 runs); ~9 after one resolved
// query (TestMeasureWorktreeSharing: +28 MiB). 8 sits between a fresh and
// a used root. One repository, so a rough figure; the process's RSS runs
// above live heap by the GC's headroom. See rootResident.
const residentFactor = 8

// rootResident estimates what a hosted root holds in memory: the bytes its
// index read (CostReport.Bytes) times residentFactor. Cheap — no heap walk
// — and per root, which a heap measurement of a process hosting many roots
// cannot be. Child LSPs are not in it: they are processes of their own,
// stopped --root-lsp-grace after the root's last client left. The shared
// parse cache is bounded by its own entry LRU.
func rootResident(srv *mcp.Server) int64 {
	return srv.IndexCost().Bytes * residentFactor
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
// nil is an /open: it rebuilds a failed entry, and an entry built with
// other index options (see sameBuild) — the sessions on the root are
// told, and the old server's open batches rolled back. lsps false builds
// without starting child LSPs (a takeover's warm); a later get with lsps
// starts them.
func (r *Registry) get(root, holder string, opts *OpenOptions, lsps bool) (*mcp.Server, error) {
	if opts != nil {
		r.checkServers()
	}
	// The client's switch is the session's; set it when the open names a
	// session.
	setPrefs := holder != "" && opts != nil
	r.mu.Lock()
	var carried map[string]struct{}
	var carriedLSP map[string]mcp.LSPPrefs
	var replaced *entry
	serversChanged := false
	if e, ok := r.servers[root]; ok {
		failed, changed := false, false
		if opts != nil {
			select {
			case <-e.ready:
				failed = e.err != nil
			default:
			}
			serversChanged = !failed && e.gen != r.gen
			changed = !failed && (!r.sameBuild(e.opts, *opts) || serversChanged)
		}
		if !failed && !changed {
			if holder != "" {
				r.holdLocked(e, holder)
			}
			if setPrefs {
				if e.lsp == nil {
					e.lsp = map[string]mcp.LSPPrefs{}
				}
				e.lsp[holder] = opts.lspPrefs()
			}
			e.lastUsed = time.Now()
			r.mu.Unlock()
			if holder != "" {
				r.hub.replay(holder, root)
			}
			<-e.ready
			if e.err == nil && setPrefs {
				e.srv.SetSessionLSP(holder, opts.lspPrefs())
			}
			if e.err == nil && lsps {
				e.srv.StartLSPs()
			}
			return e.srv, e.err
		}
		// A failed root opened again, or a root opened with other index
		// options: build it afresh for this client, keeping the holders
		// the old one had.
		if e.lspTimer != nil {
			e.lspTimer.Stop()
		}
		carried = e.holders
		carriedLSP = e.lsp
		delete(r.servers, root)
		r.hub.forget(root)
		if changed {
			replaced = e
		}
	}
	e := &entry{ready: make(chan struct{}), holders: map[string]struct{}{}, lastUsed: time.Now(),
		lsp: map[string]mcp.LSPPrefs{}, gen: r.gen}
	for h := range carried {
		e.holders[h] = struct{}{}
	}
	for h, p := range carriedLSP {
		e.lsp[h] = p
	}
	if opts != nil {
		e.opts = opts.buildOptions()
	}
	if holder != "" {
		e.holders[holder] = struct{}{}
	}
	if setPrefs {
		e.lsp[holder] = opts.lspPrefs()
	}
	r.servers[root] = e
	var to []string
	for h := range e.holders {
		to = append(to, h)
	}
	prefs := make(map[string]mcp.LSPPrefs, len(e.lsp))
	for h, p := range e.lsp {
		prefs[h] = p
	}
	cfg, reg := r.cfg, r.reg
	r.mu.Unlock()
	if replaced != nil {
		r.retire(root, replaced, e.opts, to, serversChanged)
	}

	srv := r.build(root, e, e.opts, cfg, reg)
	// Before the bring-up: it starts only the children a client wants.
	for h, p := range prefs {
		srv.SetSessionLSP(h, p)
	}
	err := srv.BringUp(lsps)
	e.srv, e.err = srv, err
	close(e.ready)

	if err != nil {
		log.Printf("daemon: open root %s failed: %v", root, err)
	} else {
		log.Printf("daemon: opened root %s", root)
	}
	var size int64
	if err == nil {
		size = rootResident(srv)
	}
	r.mu.Lock()
	var victims []string
	if cur, ok := r.servers[root]; ok && cur == e {
		e.size = size
		// An unheld root (a one-shot /query, a takeover's warm) is
		// client-less from the start.
		if len(e.holders) == 0 {
			victims = r.unheldLocked(root, e)
		}
		victims = append(victims, r.trimLocked()...)
	}
	evicted := r.dropLocked(victims)
	r.mu.Unlock()
	r.shutdown(evicted)
	return srv, err
}

// holdLocked adds a holder and cancels a pending child-LSP stop. Caller
// holds mu.
func (r *Registry) holdLocked(e *entry, holder string) {
	if e.holders == nil {
		e.holders = map[string]struct{}{}
	}
	e.holders[holder] = struct{}{}
	if e.lspTimer != nil {
		e.lspTimer.Stop()
		e.lspTimer = nil
	}
	e.lspsStopped = false
}

// Release drops `holder`'s ref on every root it holds: called when a
// client's session stream drops. A root whose last holder just left
// becomes client-less (see unheldLocked), and the client-less roots are
// trimmed to idleCap.
func (r *Registry) Release(holder string) {
	r.mu.Lock()
	var victims []string
	for root, e := range r.servers {
		if _, ok := e.holders[holder]; !ok {
			continue
		}
		delete(e.holders, holder)
		delete(e.lsp, holder)
		if len(e.holders) == 0 {
			victims = append(victims, r.unheldLocked(root, e)...)
		}
	}
	victims = append(victims, r.trimLocked()...)
	evicted := r.dropLocked(victims)
	r.mu.Unlock()
	r.shutdown(evicted)
}

// unheldLocked starts a client-less root's grace: its child LSPs stop
// lspGrace from now unless a client comes back. A root whose bring-up
// failed holds nothing worth keeping and is returned to evict now (a new
// open builds it again). One still building is left to its bring-up's
// end, which calls this again. Caller holds mu.
func (r *Registry) unheldLocked(root string, e *entry) []string {
	select {
	case <-e.ready:
	default:
		return nil
	}
	if e.err != nil {
		return []string{root}
	}
	if e.lspTimer != nil {
		e.lspTimer.Stop()
	}
	e.lspTimer = time.AfterFunc(max(r.lspGrace, 0), func() { r.stopLSPs(root, e) })
	return nil
}

// stopLSPs stops root's child LSPs if it is still e and client-less: the
// end of its grace. The index and watcher stay; a client that comes back
// starts each child again on its first request that needs one.
func (r *Registry) stopLSPs(root string, e *entry) {
	r.mu.Lock()
	if r.servers[root] != e || len(e.holders) > 0 {
		r.mu.Unlock()
		return
	}
	e.lspTimer, e.lspsStopped = nil, true
	r.mu.Unlock()
	if n := e.srv.StopLSPs(); n > 0 {
		log.Printf("daemon: root %s: no client for %s; stopped %d child LSP(s)", root, r.lspGrace, n)
	}
}

// trimLocked picks the client-less roots to evict so that what the rest
// hold (their rootResident estimates) is within idleCap: the least
// recently used first. A root with a client, or still building, is never
// picked. Caller holds mu.
func (r *Registry) trimLocked() []string {
	if r.idleCap < 0 {
		return nil
	}
	type idle struct {
		root string
		e    *entry
	}
	var lru []idle
	var total int64
	for root, e := range r.servers {
		if len(e.holders) > 0 {
			continue
		}
		select {
		case <-e.ready:
		default:
			continue
		}
		lru = append(lru, idle{root, e})
		total += e.size
	}
	// A cap of 0 keeps no client-less root, whatever its size.
	over := func() bool { return total > r.idleCap || r.idleCap == 0 }
	if !over() {
		return nil
	}
	slices.SortFunc(lru, func(a, b idle) int { return a.e.lastUsed.Compare(b.e.lastUsed) })
	var out []string
	for _, v := range lru {
		if !over() {
			break
		}
		out = append(out, v.root)
		total -= v.e.size
		log.Printf("daemon: evicting client-less root %s (~%d MiB; --idle-roots-mb %d)", v.root, v.e.size>>20, r.idleCap>>20)
	}
	return out
}

// dropLocked removes roots from the registry and their kept notifications,
// and returns their entries for shutdown. Caller holds mu.
func (r *Registry) dropLocked(roots []string) []*entry {
	var out []*entry
	for _, root := range roots {
		e, ok := r.servers[root]
		if !ok {
			continue
		}
		if e.lspTimer != nil {
			e.lspTimer.Stop()
		}
		delete(r.servers, root)
		r.hub.forget(root)
		out = append(out, e)
	}
	return out
}

// shutdown shuts evicted servers down (index, child LSPs, watcher), in
// the background: the caller may be a request handler. The shared parse
// cache is daemon-owned, so a re-open still comes up warm on parses.
func (r *Registry) shutdown(evicted []*entry) {
	for _, e := range evicted {
		go func() {
			<-e.ready
			if e.srv != nil {
				e.srv.Shutdown()
			}
		}()
	}
}

// Len is how many roots the daemon hosts, with a client or not.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.servers)
}

// RootStat is one hosted root, for /health and doctor.
type RootStat struct {
	Root    string `json:"root"`
	State   string `json:"state"` // building | up | failed
	Clients int    `json:"clients"`
	// Resident is the root's estimated resident bytes (rootResident: the
	// bytes its index read × residentFactor); child LSPs not included.
	Resident    int64 `json:"residentBytes"`
	IdleSec     int64 `json:"idleSec"`               // since a request last named it
	LSPsStopped bool  `json:"lspsStopped,omitempty"` // by --root-lsp-grace, since its last client left
}

// RootStats lists the hosted roots, sorted, and the client-less cap.
func (r *Registry) RootStats() ([]RootStat, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RootStat, 0, len(r.servers))
	for root, e := range r.servers {
		st := RootStat{Root: root, State: "building", Clients: len(e.holders), Resident: e.size,
			IdleSec: int64(time.Since(e.lastUsed) / time.Second), LSPsStopped: e.lspsStopped}
		select {
		case <-e.ready:
			st.State = "up"
			if e.err != nil {
				st.State = "failed"
			}
		default:
		}
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b RootStat) int { return strings.Compare(a.Root, b.Root) })
	return out, r.idleCap
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

// retire tells the sessions on root (to) that its index options changed
// and it is rebuilding, then rolls back their open batches on the old
// server and shuts it down once its own bring-up has ended.
func (r *Registry) retire(root string, old *entry, now OpenOptions, to []string, serversChanged bool) {
	from, next := old.opts.String(), now.String()
	msg := fmt.Sprintf("poly-lsp: the index options of %s changed (%s → %s), by a client's open; rebuilding the index", root, from, next)
	data := map[string]any{"state": "rebuilding", "root": root, "from": from, "to": next}
	if serversChanged && r.sameBuild(old.opts, now) {
		msg = fmt.Sprintf("poly-lsp: the language servers changed (%s); rebuilding the index of %s and restarting its child LSPs", r.serversPath, root)
		data["reason"] = "servers"
	}
	data["message"] = msg
	params, err := json.Marshal(map[string]any{"level": "info", "logger": daemonLogger, "data": data})
	if err == nil {
		log.Printf("daemon: root %s: %s", root, strings.TrimPrefix(msg, "poly-lsp: "))
		r.hub.publish(root, daemonLogger, "notifications/message", params, to)
	}
	go func() {
		<-old.ready
		if old.srv == nil {
			return
		}
		for _, sess := range to {
			if n, ok := old.srv.RollbackSession(sess); ok {
				log.Printf("daemon: root %s rebuilt: rolled back session %s's open batch (%d edit(s))", root, sess, n)
			}
		}
		old.srv.Shutdown()
	}()
}

// sameBuild reports whether two opens' options build the same root: the
// same effective excludes (in any order), includes, budget, index-anyway,
// symlink depth and child-LSP roots, with the daemon's defaults filled in.
func (r *Registry) sameBuild(a, b OpenOptions) bool {
	return reflect.DeepEqual(r.effective(a), r.effective(b))
}

// effective is o with the daemon's defaults filled in, its excludes
// cleaned and sorted: what a build from o uses.
func (r *Registry) effective(o OpenOptions) OpenOptions {
	var ex []string
	for _, d := range o.Excludes {
		ex = append(ex, git.CleanExclude(d))
	}
	slices.Sort(ex)
	ex = slices.Compact(ex)
	out := OpenOptions{Excludes: ex, Includes: o.Includes, Budget: o.Budget, IndexAnyway: r.idx.Anyway || o.IndexAnyway, LSPRoots: o.LSPRoots}
	if len(out.Includes) == 0 {
		out.Includes = nil
	}
	if out.Budget == "" {
		out.Budget = r.idx.Budget
	}
	depth := r.idx.SymlinkDepth
	if o.SymlinkDepth != nil {
		depth = *o.SymlinkDepth
	}
	out.SymlinkDepth = &depth
	if out.LSPRoots == "" {
		out.LSPRoots = "single"
		if r.lspRootsAuto {
			out.LSPRoots = "auto"
		}
	}
	return out
}

// String names the options an open set, for a person: "exclude vendor,
// budget 10s", or "the daemon's defaults".
func (o OpenOptions) String() string {
	var parts []string
	if len(o.Excludes) > 0 {
		parts = append(parts, "exclude "+strings.Join(o.Excludes, ","))
	}
	if len(o.Includes) > 0 {
		parts = append(parts, "include "+strings.Join(o.Includes, ","))
	}
	if o.Budget != "" {
		parts = append(parts, "budget "+o.Budget)
	}
	if o.IndexAnyway {
		parts = append(parts, "index-anyway")
	}
	if o.SymlinkDepth != nil {
		parts = append(parts, fmt.Sprintf("symlink-depth %d", *o.SymlinkDepth))
	}
	if o.LSPRoots != "" {
		parts = append(parts, "lsp-roots "+o.LSPRoots)
	}
	if len(parts) == 0 {
		return "the daemon's defaults"
	}
	return strings.Join(parts, ", ")
}

// build constructs (but does not bring up) a server for root, as entry e,
// with the shared cache, daemon-wide policy and the client's index
// options. Schemas are detected per root when auto-schemas is on, since
// detection is workspace-specific.
func (r *Registry) build(root string, e *entry, opts OpenOptions, cfg *config.Config, reg *config.Registry) *mcp.Server {
	schemas := cfg.Schemas
	if cfg.AutoSchemas {
		if detected := config.DetectSchemas(root, cfg.Schemas); len(detected) > 0 {
			schemas = append(append([]config.Schema{}, cfg.Schemas...), detected...)
		}
	}
	srv := mcp.New(reg, root, cfg.Bindings, schemas)
	srv.SetQuiet(true) // one daemon log for every root: the notifications carry the report
	srv.SetReadOnly(r.readOnly)
	srv.SetValidateEdits(r.validate)
	srv.SetParseCache(r.cache)
	srv.SetUsageLog(r.usageLog)
	srv.SetNotifier(func(method string, params map[string]any) { r.notify(root, e, method, params) })
	srv.SetExcludes(append(append([]string{}, r.idx.Excludes...), opts.Excludes...))
	srv.SetIncludes(append(append(append([]string{}, cfg.Include...), r.idx.Includes...), opts.Includes...))
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
	mgr := multiplex.NewManager(reg)
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
// kept for a session that attaches later. A server that is no longer the
// root's (e, rebuilt with other options, or evicted) says nothing.
func (r *Registry) notify(root string, e *entry, method string, params map[string]any) {
	raw, err := json.Marshal(params)
	if err != nil {
		return
	}
	logger, _ := params["logger"].(string)
	r.mu.Lock()
	if r.servers[root] != e {
		r.mu.Unlock()
		return
	}
	var to []string
	for h := range e.holders {
		to = append(to, h)
	}
	r.mu.Unlock()
	r.hub.publish(root, logger, method, raw, to)
}

// Tools is the tool catalog a hosted root serves. It does not depend on
// the index, so it is answered without building one: tools/list must not
// wait for a bring-up that a tool call has not asked for yet.
func (r *Registry) Tools() []mcp.ToolDescriptor {
	r.catalogOnce.Do(func() {
		srv := mcp.New(r.registry(), "", nil, nil)
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
	lang := r.registry().LookupByExt(ext)
	if lang == nil {
		return ""
	}
	return lang.Name
}

// registry is the language registry roots are built with now.
func (r *Registry) registry() *config.Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reg
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

// optionsOf is the index options root was built with (zero when it is
// not hosted, or was built with the daemon's own).
func (r *Registry) optionsOf(root string) OpenOptions {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.servers[root]; ok {
		return e.opts
	}
	return OpenOptions{}
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
		if e.lspTimer != nil {
			e.lspTimer.Stop()
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
