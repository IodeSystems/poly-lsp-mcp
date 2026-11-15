package multiplex

import (
	"encoding/json"
	"log"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// This file makes poly-lsp-mcp the file watcher of its children. Every
// child is told the client supports dynamic registration of
// workspace/didChangeWatchedFiles (Initialize). A child that registers
// watchers gets the matching events from the server's own fsnotify watcher
// (mcp/file_watch.go) through Manager.NotifyFileEvents, instead of
// creating inotify watches of its own. docs/design.md has the
// measurements.

// FileChangeType is LSP's FileChangeType.
type FileChangeType int

const (
	FileCreated FileChangeType = 1
	FileChanged FileChangeType = 2
	FileDeleted FileChangeType = 3
)

// FileEvent is one file change on disk, by absolute path.
type FileEvent struct {
	Path string
	Type FileChangeType
}

const methodWatchedFiles = "workspace/didChangeWatchedFiles"

// fileWatcher is one compiled LSP FileSystemWatcher.
type fileWatcher struct {
	// base is the RelativePattern's base path; "" for a plain string
	// pattern, which matches against the whole path.
	base string
	// alts are the pattern's brace expansions, each split on "/".
	alts [][]string
	// kind is the WatchKind bitmask: 1 create, 2 change, 4 delete.
	kind int
}

// watchKindBit maps a FileChangeType to its WatchKind bit.
func watchKindBit(t FileChangeType) int {
	switch t {
	case FileCreated:
		return 1
	case FileChanged:
		return 2
	case FileDeleted:
		return 4
	}
	return 0
}

func (w fileWatcher) matches(p string, t FileChangeType) bool {
	if w.kind&watchKindBit(t) == 0 {
		return false
	}
	p = filepath.ToSlash(p)
	if w.base != "" {
		base := strings.TrimSuffix(filepath.ToSlash(w.base), "/")
		if p == base || !strings.HasPrefix(p, base+"/") {
			return false
		}
		p = p[len(base)+1:]
	} else {
		p = strings.TrimPrefix(p, "/")
	}
	segs := strings.Split(p, "/")
	for _, alt := range w.alts {
		if matchGlobSegs(alt, segs) {
			return true
		}
	}
	return false
}

// compileWatcher compiles one FileSystemWatcher. Its globPattern is a
// string or a RelativePattern {baseUri, pattern}, where baseUri is a URI
// or a WorkspaceFolder {uri, name}. kind defaults to 7 (all).
func compileWatcher(raw json.RawMessage) (fileWatcher, bool) {
	var fw struct {
		GlobPattern json.RawMessage `json:"globPattern"`
		Kind        *int            `json:"kind"`
	}
	if json.Unmarshal(raw, &fw) != nil {
		return fileWatcher{}, false
	}
	w := fileWatcher{kind: 7}
	if fw.Kind != nil {
		w.kind = *fw.Kind
	}
	var pattern string
	if json.Unmarshal(fw.GlobPattern, &pattern) != nil {
		var rel struct {
			BaseURI json.RawMessage `json:"baseUri"`
			Pattern string          `json:"pattern"`
		}
		if json.Unmarshal(fw.GlobPattern, &rel) != nil {
			return fileWatcher{}, false
		}
		var baseURI string
		if json.Unmarshal(rel.BaseURI, &baseURI) != nil {
			var folder struct {
				URI string `json:"uri"`
			}
			if json.Unmarshal(rel.BaseURI, &folder) != nil {
				return fileWatcher{}, false
			}
			baseURI = folder.URI
		}
		w.base = fileURIPath(baseURI)
		if w.base == "" {
			return fileWatcher{}, false
		}
		pattern = rel.Pattern
	}
	if pattern == "" {
		return fileWatcher{}, false
	}
	for _, alt := range expandBraces(pattern) {
		w.alts = append(w.alts, strings.Split(strings.TrimPrefix(alt, "/"), "/"))
	}
	return w, true
}

// fileURIPath is the path of a file:// URI, "" for any other.
func fileURIPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return u.Path
}

// expandBraces expands every {a,b} group (nested groups included) into
// the plain patterns it stands for. An unclosed brace is literal.
func expandBraces(p string) []string {
	open := strings.IndexByte(p, '{')
	if open < 0 {
		return []string{p}
	}
	depth, close := 0, -1
	var commas []int
	for i := open; i < len(p) && close < 0; i++ {
		switch p[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				close = i
			}
		case ',':
			if depth == 1 {
				commas = append(commas, i)
			}
		}
	}
	if close < 0 {
		return []string{p}
	}
	prefix, suffix := p[:open], p[close+1:]
	var parts []string
	start := open + 1
	for _, c := range commas {
		parts = append(parts, p[start:c])
		start = c + 1
	}
	parts = append(parts, p[start:close])
	var out []string
	for _, part := range parts {
		out = append(out, expandBraces(prefix+part+suffix)...)
	}
	return out
}

// matchGlobSegs matches path segments against pattern segments: "**"
// matches zero or more segments, any other segment is a path.Match
// pattern (`*`, `?`, `[...]` within one segment).
func matchGlobSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for len(pat) > 1 && pat[1] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(segs); i++ {
				if matchGlobSegs(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], segs[0]); err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

// watchRegistry is one child's registered file watchers, by registration
// id.
type watchRegistry struct {
	mu   sync.Mutex
	byID map[string][]fileWatcher
}

func (r *watchRegistry) set(id string, ws []fileWatcher) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byID == nil {
		r.byID = map[string][]fileWatcher{}
	}
	r.byID[id] = ws
}

func (r *watchRegistry) drop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
}

func (r *watchRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, ws := range r.byID {
		n += len(ws)
	}
	return n
}

func (r *watchRegistry) wants(p string, t FileChangeType) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ws := range r.byID {
		for _, w := range ws {
			if w.matches(p, t) {
				return true
			}
		}
	}
	return false
}

// methodDidChangeConfiguration's registration is accepted and does
// nothing: this client never changes a child's configuration, so it
// sends no didChangeConfiguration. TypeScript 7 registers it in its
// `initialized` handler and, refused, returns before finishing that
// handler (typescript-go internal/lsp/server.go handleInitialized).
const methodDidChangeConfiguration = "workspace/didChangeConfiguration"

// handleRegistration answers client/registerCapability and
// client/unregisterCapability. didChangeWatchedFiles is supported and
// didChangeConfiguration accepted as a no-op; a request that names any
// other method is refused whole, as every registration was before this
// client supported one. It returns the error message, "" on success.
func (c *Child) handleRegistration(method string, params json.RawMessage) string {
	if method == "client/unregisterCapability" {
		// The LSP spec spells the field "unregisterations".
		var p struct {
			Unregisterations []struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			} `json:"unregisterations"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return "bad params: " + err.Error()
		}
		for _, u := range p.Unregisterations {
			if u.Method != methodWatchedFiles && u.Method != methodDidChangeConfiguration {
				return "client does not implement unregistration of " + u.Method
			}
		}
		for _, u := range p.Unregisterations {
			if u.Method == methodWatchedFiles {
				c.watches.drop(u.ID)
			}
		}
		return ""
	}
	var p struct {
		Registrations []struct {
			ID              string `json:"id"`
			Method          string `json:"method"`
			RegisterOptions struct {
				Watchers []json.RawMessage `json:"watchers"`
			} `json:"registerOptions"`
		} `json:"registrations"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "bad params: " + err.Error()
	}
	for _, r := range p.Registrations {
		if r.Method != methodWatchedFiles && r.Method != methodDidChangeConfiguration {
			return "client does not implement registration of " + r.Method
		}
	}
	for _, r := range p.Registrations {
		if r.Method != methodWatchedFiles {
			continue
		}
		var ws []fileWatcher
		for _, raw := range r.RegisterOptions.Watchers {
			if w, ok := compileWatcher(raw); ok {
				ws = append(ws, w)
			} else {
				log.Printf("[%s] file watcher not understood, ignored: %s", c.name, raw)
			}
		}
		c.watches.set(r.ID, ws)
	}
	return ""
}

// WantsFileEvent reports whether one of the child's registered watchers
// matches path for this kind of change.
func (c *Child) WantsFileEvent(path string, t FileChangeType) bool {
	return c.watches.wants(path, t)
}

// FileWatchers is the number of file watchers the child has registered.
func (c *Child) FileWatchers() int { return c.watches.count() }

// NotifyFileEvents sends each running child the events its registered
// watchers match, as one workspace/didChangeWatchedFiles. A child with no
// match gets nothing. A stopped child gets nothing either: it reads the
// disk afresh when it starts.
func (m *Manager) NotifyFileEvents(events []FileEvent) {
	if len(events) == 0 {
		return
	}
	m.mu.RLock()
	children := make([]*Child, 0, len(m.children))
	for _, c := range m.children {
		children = append(children, c)
	}
	m.mu.RUnlock()
	for _, c := range children {
		if !alive(c) {
			continue
		}
		var changes []map[string]any
		for _, ev := range events {
			if c.WantsFileEvent(ev.Path, ev.Type) {
				changes = append(changes, map[string]any{"uri": "file://" + filepath.ToSlash(ev.Path), "type": int(ev.Type)})
			}
		}
		if len(changes) == 0 {
			continue
		}
		if err := c.Notify(methodWatchedFiles, map[string]any{"changes": changes}); err != nil {
			log.Printf("multiplex: %s to %s: %v", methodWatchedFiles, c.name, err)
		}
	}
}
