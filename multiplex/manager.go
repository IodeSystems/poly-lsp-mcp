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
type Manager struct {
	registry *config.Registry

	// Restart policy. Zero values mean: 1s initial backoff, 30s max
	// backoff, 5 attempts. Tests override before calling Start.
	RestartInitialBackoff time.Duration
	RestartMaxBackoff     time.Duration
	RestartMaxAttempts    int

	// Captured at Start so the watchdog can respawn with the same
	// shape it was originally given.
	startCwd     string
	startRootURI string

	mu       sync.RWMutex
	children map[string]*Child // language name → Child
	caps     map[string]json.RawMessage

	// diagnostics is a single store across every spawned child. Wired
	// into each child via Attach during Start / restart so callers can
	// WaitAfter(uri) for the next publishDiagnostics from whichever
	// child owns that URI.
	diagnostics *DiagnosticStore

	shutdownMu     sync.Mutex
	shutdownCalled bool
}

func NewManager(reg *config.Registry) *Manager {
	return &Manager{
		registry:    reg,
		children:    map[string]*Child{},
		caps:        map[string]json.RawMessage{},
		diagnostics: NewDiagnosticStore(),
	}
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
		m.mu.Lock()
		m.children[name] = child
		m.caps[name] = caps
		m.mu.Unlock()
		log.Printf("multiplex: %s ready", name)
		go m.watch(name, child)
	}
	return nil
}

// watch blocks on child.Done. If the child exits while the manager
// is still live, the watchdog kicks off a restart attempt with the
// configured backoff policy. Watchdogs from previous incarnations
// exit when their child is replaced — the new child gets its own.
func (m *Manager) watch(name string, child *Child) {
	<-child.Done()

	m.shutdownMu.Lock()
	stopping := m.shutdownCalled
	m.shutdownMu.Unlock()
	if stopping {
		return
	}

	// Already replaced by a previous restart pass? Drop out so the
	// most recent watchdog is the one in charge.
	m.mu.RLock()
	current := m.children[name]
	m.mu.RUnlock()
	if current != nil && current != child {
		return
	}

	log.Printf("multiplex: %s exited unexpectedly: %v", name, child.Err())
	m.restart(name)
}

// restart attempts to respawn the child for name with exponential
// backoff. Each attempt re-checks the shutdown flag so a Shutdown call
// arriving mid-restart cancels promptly. On exhaustion the language is
// dropped from the table so RouteByURI returns nil (callers fall back
// to the symbol index).
func (m *Manager) restart(name string) {
	lang := m.registry.LookupByName(name)
	if lang == nil || lang.LSP == nil {
		// Language vanished from the registry; nothing to restart.
		m.mu.Lock()
		delete(m.children, name)
		delete(m.caps, name)
		m.mu.Unlock()
		return
	}

	initial, maxBackoff, maxAttempts := m.restartPolicy()
	backoff := initial

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		time.Sleep(backoff)

		m.shutdownMu.Lock()
		stopping := m.shutdownCalled
		m.shutdownMu.Unlock()
		if stopping {
			return
		}

		log.Printf("multiplex: restart %s attempt %d/%d", name, attempt, maxAttempts)

		child, err := Spawn(name, lang.LSP, m.startCwd)
		if err != nil {
			log.Printf("multiplex: restart %s spawn failed: %v", name, err)
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}
		m.diagnostics.Attach(child)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		caps, err := child.Initialize(ctx, m.startRootURI)
		cancel()
		if err != nil {
			log.Printf("multiplex: restart %s initialize failed: %v", name, err)
			_ = child.Kill()
			_ = child.Wait()
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		m.mu.Lock()
		m.children[name] = child
		m.caps[name] = caps
		m.mu.Unlock()
		log.Printf("multiplex: %s restarted", name)
		go m.watch(name, child)
		return
	}

	log.Printf("multiplex: gave up restarting %s after %d attempts", name, maxAttempts)
	m.mu.Lock()
	delete(m.children, name)
	delete(m.caps, name)
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
func (m *Manager) RouteByURI(uri string) *Child {
	lang := m.languageForURI(uri)
	if lang == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	child := m.children[lang.Name]
	if child == nil {
		return nil
	}
	select {
	case <-child.Done():
		return nil
	default:
		return child
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

// Languages returns the names of running children, sorted. Useful for
// diagnostics and as a sanity check after Start.
func (m *Manager) Languages() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.children))
	for k := range m.children {
		out = append(out, k)
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
	m.shutdownCalled = true
	m.shutdownMu.Unlock()

	m.mu.Lock()
	children := m.children
	m.children = map[string]*Child{}
	m.caps = map[string]json.RawMessage{}
	m.mu.Unlock()

	var errs []error
	for name, child := range children {
		if err := child.Shutdown(ctx); err != nil {
			log.Printf("multiplex: shutdown %s: %v", name, err)
			errs = append(errs, err)
			_ = child.Kill()
		}
		if err := child.Wait(); err != nil {
			// exit-after-shutdown often surfaces as non-nil; only log.
			log.Printf("multiplex: wait %s: %v", name, err)
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
