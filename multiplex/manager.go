package multiplex

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"maps"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/lspbin"
)

// Manager owns the set of running Child LSPs and routes incoming
// textDocument/* requests to the right one by file extension.
//
// Spawn policy in v0.1 is eager: Start spawns one child per language in
// the supplied list, blocks on each Initialize, and discards children
// that fail. Children that survive are kept until Shutdown.
//
// Restart-on-crash: a watchdog goroutine waits on each child's Done.
// If the child exits without an orderly Shutdown call from the manager,
// the watchdog respawns via exponential backoff up to RestartMaxAttempts
// before giving up and removing the language from the table.
//
// Lifecycle (opt-in, see Lifecycle): a monitor goroutine stops children
// that are idle, over an RSS limit, or chosen under machine memory
// pressure. A stopped child is not a crash: it does not use up
// RestartMaxAttempts, and the next RouteByURI for its language starts it
// again and calls OnStart so the caller can replay its open documents.
//
// Project roots (opt-in, see Roots): a language Roots names gets one child
// per project root instead of one at the workspace root, each started on
// the first request for a file under it. A file goes to the nearest root
// that encloses it, else to a child at the workspace root. At most
// RootsMax run per language; the least recently used is stopped to make
// room (reason "lru"), and starts again on demand like any policy-stopped
// child.
type Manager struct {
	registry *config.Registry

	// Restart policy. Zero values mean: 1s initial backoff, 30s max
	// backoff, 5 attempts. Tests override before calling Start.
	RestartInitialBackoff time.Duration
	RestartMaxBackoff     time.Duration
	RestartMaxAttempts    int

	// Lifecycle policy. Zero value = all off (the pre-lifecycle
	// behavior). Set before Start.
	Lifecycle Lifecycle

	// OnStart, if set, is called after a child for lang is (re)started
	// by a crash restart or a lazy wake — not by the initial Start. The
	// new child knows no documents; the callback replays didOpen for the
	// documents Running routes to child (with Roots, one language can
	// have several children). RouteByURI does not return a woken child
	// until OnStart returns.
	OnStart func(lang string, child *Child)

	// OnStop, if set, is called after a policy stop (idle, memory,
	// pressure, lru) of lang's child at dir. Not called by Shutdown.
	OnStop func(lang, dir, reason string)

	// Roots maps a language to its project roots, absolute (see
	// symbols.DetectProjectRoots). A language with at least one root here
	// gets a child per root, started on demand; the others keep one child
	// at the workspace root. nil = one child per language, the default
	// (--lsp-roots single). Set before Start.
	Roots map[string][]string
	// RootsMax caps the running children of one Roots language. Zero = 8.
	RootsMax int

	// Defer, if set, names the languages Start does not spawn: no client
	// wants their child yet (lsp_off, off_by_default). Each is left
	// stopped, to start on its first RouteByURI, as a lifecycle-stopped
	// child does. Set before Start.
	Defer func(lang string) bool

	// StatusDir, if set, gets a StatusFile from this manager every
	// Lifecycle interval and after each stop or start, removed at
	// Shutdown. See ReadStatusFiles.
	StatusDir string
	statusMu  sync.Mutex
	// StatusIndex, if set, supplies the status file's index field. Set
	// before Start.
	StatusIndex func() any

	// Test seams; nil = read /proc.
	readRSS func(pid int) (int64, error)
	readPSI func() (float64, error)

	// Captured at Start so the watchdog can respawn with the same
	// shape it was originally given.
	startCwd     string
	startRootURI string

	mu       sync.RWMutex
	children map[childKey]*Child
	caps     map[string]json.RawMessage // language name → capabilities
	slots    map[childKey]*slot         // lifecycle state; present once started
	perRoot  map[string]bool            // languages whose children start per project root

	// diagnostics is a single store across every spawned child. Wired
	// into each child via Attach during Start / restart so callers can
	// WaitAfter(uri) for the next publishDiagnostics from whichever
	// child owns that URI.
	diagnostics *DiagnosticStore

	shutdownMu     sync.Mutex
	shutdownCalled bool
	monitorStop    chan struct{}
}

// childKey names one child: a language, and the directory it runs in and
// advertises as its root — the workspace root, or a project root.
type childKey struct{ lang, dir string }

func NewManager(reg *config.Registry) *Manager {
	return &Manager{
		registry:    reg,
		children:    map[childKey]*Child{},
		caps:        map[string]json.RawMessage{},
		slots:       map[childKey]*slot{},
		perRoot:     map[string]bool{},
		diagnostics: NewDiagnosticStore(),
		monitorStop: make(chan struct{}),
	}
}

func (m *Manager) stopping() bool {
	m.shutdownMu.Lock()
	defer m.shutdownMu.Unlock()
	return m.shutdownCalled
}

func (m *Manager) rootsMax() int {
	if m.RootsMax > 0 {
		return m.RootsMax
	}
	return 8
}

// Diagnostics returns the manager's shared diagnostic store. Callers
// use this to subscribe to publishDiagnostics across every child.
func (m *Manager) Diagnostics() *DiagnosticStore {
	return m.diagnostics
}

// restartPolicy returns the effective policy values, applying defaults
// for fields the caller left as zero.
func (m *Manager) restartPolicy() (initial, max time.Duration, attempts int) {
	initial = m.RestartInitialBackoff
	if initial <= 0 {
		initial = time.Second
	}
	max = m.RestartMaxBackoff
	if max <= 0 {
		max = 30 * time.Second
	}
	attempts = m.RestartMaxAttempts
	if attempts <= 0 {
		attempts = 5
	}
	return
}

// Start spawns one child per language in `languages` that is registered
// with an LSP binary. Languages without an LSP (tree-sitter only) and
// languages whose binary is missing are silently skipped — the symbol
// index still serves their requests via the fallback path.
//
// cwd is the filesystem path each child runs in; rootURI is the LSP
// workspace root advertised to each child during Initialize.
//
// A language with project roots in Roots spawns nothing here: its
// children start on the first request routed to them. Its binary must be
// on PATH or in a well-known user bin dir (internal/lspbin), or it is skipped like any other missing binary.
//
// Start may be called only once per Manager; concurrent invocations are
// not supported.
func (m *Manager) Start(ctx context.Context, cwd, rootURI string, languages []string) error {
	m.startCwd = cwd
	m.startRootURI = rootURI
	for _, name := range languages {
		lang := m.registry.LookupByName(name)
		if lang == nil || lang.LSP == nil {
			continue
		}
		if n := len(m.Roots[name]); n > 0 {
			if _, err := lspbin.Resolve(lang.LSP.Cmd); err != nil {
				log.Printf("multiplex: spawn %s (%s): %v", name, lang.LSP.Cmd, err)
				continue
			}
			m.mu.Lock()
			m.perRoot[name] = true
			m.mu.Unlock()
			log.Printf("multiplex: %s: %d project root(s), a child each on first use (at most %d running)", name, n, m.rootsMax())
			continue
		}
		if m.Defer != nil && m.Defer(name) {
			if _, err := lspbin.Resolve(lang.LSP.Cmd); err != nil {
				log.Printf("multiplex: spawn %s (%s): %v", name, lang.LSP.Cmd, err)
				continue
			}
			sl := newSlot()
			sl.stopped = true
			m.mu.Lock()
			m.slots[childKey{name, cwd}] = sl
			m.mu.Unlock()
			log.Printf("multiplex: %s: no client wants its child LSP; it starts on first use", name)
			continue
		}
		child, err := Spawn(name, lang.LSP, cwd)
		if err != nil {
			log.Printf("multiplex: spawn %s (%s): %v", name, lang.LSP.Cmd, err)
			continue
		}
		// Subscribe to publishDiagnostics BEFORE Initialize so any
		// startup diagnostics aren't lost.
		m.diagnostics.Attach(child)
		caps, err := child.Initialize(ctx, rootURI)
		if err != nil {
			log.Printf("multiplex: initialize %s: %v", name, err)
			_ = child.Kill()
			_ = child.Wait()
			continue
		}
		k := childKey{name, cwd}
		m.mu.Lock()
		m.children[k] = child
		m.caps[name] = caps
		m.slots[k] = newSlot()
		m.mu.Unlock()
		log.Printf("multiplex: %s ready (pid %d)", name, child.Pid())
		go m.watch(k, child)
	}
	if m.Lifecycle.enabled() || m.StatusDir != "" {
		m.writeStatus()
		go m.monitor()
	}
	return nil
}

// watch blocks on child.Done. If the child exits while the manager
// is still live, the watchdog kicks off a restart attempt with the
// configured backoff policy. Watchdogs from previous incarnations
// exit when their child is replaced — the new child gets its own.
func (m *Manager) watch(k childKey, child *Child) {
	<-child.Done()
	// Reap. Without this every crashed child stays a zombie for the
	// life of the server. Reap kills after the grace period: a child that
	// closed stdout but is still running would otherwise block forever.
	if err := child.Reap(2 * time.Second); err != nil {
		log.Printf("multiplex: %s (pid %d) reaped: %v", k.lang, child.Pid(), err)
	}

	if m.stopping() {
		return
	}

	// Replaced by a previous restart pass, or removed by a lifecycle
	// stop? Then this exit was not a crash: drop out.
	m.mu.RLock()
	current := m.children[k]
	m.mu.RUnlock()
	if current != child {
		return
	}

	log.Printf("multiplex: %s exited unexpectedly: %v", k.lang, child.Err())
	m.restart(k)
}

// rootURI is the rootUri the child at dir advertises.
func (m *Manager) rootURI(dir string) string {
	if dir == m.startCwd {
		return m.startRootURI
	}
	return "file://" + dir
}

// dropCapsLocked forgets lang's capabilities once no child of lang is
// left. Call with m.mu held.
func (m *Manager) dropCapsLocked(lang string) {
	for k := range m.children {
		if k.lang == lang {
			return
		}
	}
	delete(m.caps, lang)
}

// restart attempts to respawn the child k with exponential backoff.
// Each attempt re-checks the shutdown flag so a Shutdown call arriving
// mid-restart cancels promptly. On exhaustion the child is dropped from
// the table so RouteByURI returns nil (callers fall back to the symbol
// index) — for a project root's child, until the next request starts it
// again.
func (m *Manager) restart(k childKey) {
	name := k.lang
	lang := m.registry.LookupByName(name)
	if lang == nil || lang.LSP == nil {
		// Language vanished from the registry; nothing to restart.
		m.mu.Lock()
		delete(m.children, k)
		m.dropCapsLocked(name)
		m.mu.Unlock()
		return
	}

	initial, maxBackoff, maxAttempts := m.restartPolicy()
	backoff := initial

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		time.Sleep(backoff)

		if m.stopping() {
			return
		}

		log.Printf("multiplex: restart %s attempt %d/%d", name, attempt, maxAttempts)

		child, err := Spawn(name, lang.LSP, k.dir)
		if err != nil {
			log.Printf("multiplex: restart %s spawn failed: %v", name, err)
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}
		m.diagnostics.Attach(child)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		caps, err := child.Initialize(ctx, m.rootURI(k.dir))
		cancel()
		if err != nil {
			log.Printf("multiplex: restart %s initialize failed: %v", name, err)
			_ = child.Kill()
			_ = child.Wait()
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		m.mu.Lock()
		m.children[k] = child
		m.caps[name] = caps
		if sl := m.slots[k]; sl != nil {
			sl.stats.CrashRestarts++
			sl.touch()
		}
		m.mu.Unlock()
		log.Printf("multiplex: %s restarted (pid %d)", name, child.Pid())
		go m.watch(k, child)
		if m.OnStart != nil {
			m.OnStart(name, child)
		}
		return
	}

	log.Printf("multiplex: gave up restarting %s after %d attempts", name, maxAttempts)
	m.mu.Lock()
	delete(m.children, k)
	delete(m.slots, k)
	m.dropCapsLocked(name)
	m.mu.Unlock()
}

func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		return max
	}
	return next
}

// RouteByURI returns the Child for the given file URI, or nil if no
// language matches or its child is no longer running. Callers that get
// nil should fall back to the symbol-index path.
//
// RouteByURI counts as use of the child (resets its idle clock). A child
// stopped by lifecycle policy, or a project root's child not started yet,
// is started here, synchronously; use Running for a lookup that must not
// start one.
func (m *Manager) RouteByURI(uri string) *Child {
	k, ok := m.route(uri)
	if !ok {
		return nil
	}
	m.mu.RLock()
	child := m.children[k]
	sl := m.slots[k]
	stopped := sl != nil && sl.stopped
	perRoot := m.perRoot[k.lang]
	m.mu.RUnlock()
	if child != nil && alive(child) {
		if sl != nil {
			sl.touch()
		}
		return child
	}
	if sl == nil && perRoot {
		sl, stopped = m.lazySlot(k), true
	}
	if !stopped {
		return nil
	}
	return m.wake(k, sl)
}

// lazySlot returns k's slot, adding one marked stopped — so that wake
// starts it — when k has never run.
func (m *Manager) lazySlot(k childKey) *slot {
	m.mu.Lock()
	defer m.mu.Unlock()
	sl := m.slots[k]
	if sl == nil {
		sl = newSlot()
		sl.stopped = true
		m.slots[k] = sl
	}
	return sl
}

// route names the child that owns uri: its language, at the nearest
// project root that encloses it when the language runs per root, else at
// the workspace root.
func (m *Manager) route(uri string) (childKey, bool) {
	lang := m.languageForURI(uri)
	if lang == nil {
		return childKey{}, false
	}
	k := childKey{lang.Name, m.startCwd}
	m.mu.RLock()
	perRoot := m.perRoot[lang.Name]
	m.mu.RUnlock()
	if perRoot {
		k.dir = NearestRoot(uriToPath(uri), m.Roots[lang.Name], m.startCwd)
	}
	return k, true
}

// NearestRoot returns the deepest of roots that is path or encloses it,
// or fallback when none does.
func NearestRoot(path string, roots []string, fallback string) string {
	best := ""
	for _, r := range roots {
		if len(r) > len(best) && (path == r || strings.HasPrefix(path, r+string(filepath.Separator))) {
			best = r
		}
	}
	if best == "" {
		return fallback
	}
	return best
}

// Running returns the live Child for uri without waking a stopped child
// and without resetting its idle clock. For notifications that a stopped
// child does not need (it re-reads documents from disk when it wakes).
func (m *Manager) Running(uri string) *Child {
	k, ok := m.route(uri)
	if !ok {
		return nil
	}
	m.mu.RLock()
	child := m.children[k]
	m.mu.RUnlock()
	if child != nil && alive(child) {
		return child
	}
	return nil
}

// Serves reports whether RouteByURI could return a child for uri: one is
// running, or is stopped by lifecycle policy or not started yet and will
// start on demand. Does not start it.
func (m *Manager) Serves(uri string) bool {
	k, ok := m.route(uri)
	if !ok {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if c := m.children[k]; c != nil && alive(c) {
		return true
	}
	if m.perRoot[k.lang] {
		return true
	}
	sl := m.slots[k]
	return sl != nil && sl.stopped
}

func alive(c *Child) bool {
	select {
	case <-c.Done():
		return false
	default:
		return true
	}
}

func (m *Manager) languageForURI(uri string) *config.Language {
	path := uriToPath(uri)
	if path == "" {
		return nil
	}
	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	if ext == "" {
		return nil
	}
	return m.registry.LookupByExt(ext)
}

// Capabilities returns each running child's reported ServerCapabilities,
// keyed by language name. The server uses this to union capabilities for
// its own initialize response.
func (m *Manager) Capabilities() map[string]json.RawMessage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]json.RawMessage, len(m.caps))
	maps.Copy(out, m.caps)
	return out
}

// Broadcast sends one notification to every running child. Errors are
// logged per child but don't abort the fanout — workspace-scoped
// notifications (didChangeConfiguration, didChangeWorkspaceFolders)
// are best-effort by design.
func (m *Manager) Broadcast(method string, params any) {
	m.mu.RLock()
	children := make([]*Child, 0, len(m.children))
	for _, c := range m.children {
		children = append(children, c)
	}
	m.mu.RUnlock()
	for _, c := range children {
		if err := c.Notify(method, params); err != nil {
			log.Printf("multiplex: broadcast %s to %s: %v", method, c.name, err)
		}
	}
}

// Languages returns the names of the languages with a running child, a
// child stopped by lifecycle policy (it starts on demand), or project
// roots whose children start on demand, sorted. Useful for diagnostics
// and as a sanity check after Start.
func (m *Manager) Languages() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	set := map[string]bool{}
	for k := range m.children {
		set[k.lang] = true
	}
	for k, sl := range m.slots {
		if sl.stopped && m.children[k] == nil {
			set[k.lang] = true
		}
	}
	for lang := range m.perRoot {
		set[lang] = true
	}
	out := make([]string, 0, len(set))
	for lang := range set {
		out = append(out, lang)
	}
	sort.Strings(out)
	return out
}

// Shutdown sends shutdown+exit to every running child and waits for
// them. Best-effort: errors are logged and accumulated but every child
// gets a Kill if shutdown stalls past ctx. Sets the shutdown flag
// first so watchdog goroutines don't try to respawn children we're
// intentionally killing.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.shutdownMu.Lock()
	if !m.shutdownCalled {
		close(m.monitorStop)
	}
	m.shutdownCalled = true
	m.shutdownMu.Unlock()
	m.removeStatus()

	m.mu.Lock()
	children := m.children
	m.children = map[childKey]*Child{}
	m.caps = map[string]json.RawMessage{}
	m.perRoot = map[string]bool{}
	m.mu.Unlock()

	var errs []error
	for k, child := range children {
		if err := child.Shutdown(ctx); err != nil {
			log.Printf("multiplex: shutdown %s: %v", k.lang, err)
			errs = append(errs, err)
			_ = child.Kill()
		}
		if err := child.Wait(); err != nil {
			// exit-after-shutdown often surfaces as non-nil; only log.
			log.Printf("multiplex: wait %s: %v", k.lang, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// uriToPath converts a file:// URI to a filesystem path. POSIX-only.
// Duplicated from internal/server to avoid creating a util package for
// two functions; revisit when a third caller wants it.
func uriToPath(rawURI string) string {
	if rawURI == "" {
		return ""
	}
	u, err := url.Parse(rawURI)
	if err != nil {
		return ""
	}
	if u.Scheme != "file" {
		return ""
	}
	return u.Path
}
