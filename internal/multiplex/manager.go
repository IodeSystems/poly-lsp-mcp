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

	"github.com/iodesystems/tslsmcp/internal/config"
)

// Manager owns the set of running Child LSPs and routes incoming
// textDocument/* requests to the right one by file extension.
//
// Spawn policy in v0.1 is eager: Start spawns one child per language in
// the supplied list, blocks on each Initialize, and discards children
// that fail. Children that survive are kept until Shutdown.
//
// Restart-on-crash is a planned follow-up — currently a dead child stays
// in the map and Call returns errors from it.
type Manager struct {
	registry *config.Registry

	mu       sync.RWMutex
	children map[string]*Child // language name → Child
	caps     map[string]json.RawMessage
}

func NewManager(reg *config.Registry) *Manager {
	return &Manager{
		registry: reg,
		children: map[string]*Child{},
		caps:     map[string]json.RawMessage{},
	}
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
	}
	return nil
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
// gets a Kill if shutdown stalls past ctx.
func (m *Manager) Shutdown(ctx context.Context) error {
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
