package multiplex

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TypeScript 7's native LSP pushes no diagnostics for documents: it
// publishes only tsconfig (program) diagnostics and serves the rest by
// pull, textDocument/diagnostic (typescript-go internal/project
// session.go publishProgramDiagnostics; measured with 7.0.2, no
// publishDiagnostics for any .ts file). The diagnostics store and every
// edit's wait read publishes, so for such a child this file pulls and
// hands each report to the notification handler as a publish:
//
//   - didOpen: the opened document;
//   - didChange, didSave: that document, then every other open one (an
//     edit's fallout lands in siblings, as with a push server);
//   - workspace/diagnostic/refresh (sent after file events it judged
//     relevant, e.g. a package install): every open document.
//
// A report is published with the document version it was asked for, and
// dropped when the document changed or closed in the meantime: a newer
// pull is on its way.

// pullTimeout bounds one textDocument/diagnostic.
const pullTimeout = 30 * time.Second

// pullsDiagnostics reports whether the child at path with args is a
// pull-only server: TypeScript 7's exe (tsc, or tsgo from
// @typescript/native-preview) run with --lsp.
func pullsDiagnostics(path string, args []string) bool {
	base := strings.TrimSuffix(filepath.Base(path), ".exe")
	if base != "tsc" && base != "tsgo" {
		return false
	}
	for _, a := range args {
		if a == "--lsp" {
			return true
		}
	}
	return false
}

// pullState is the open documents of a pull child, by URI, with their
// versions, and a sweep generation: a newer sweep stops an older one.
type pullState struct {
	mu   sync.Mutex
	open map[string]int
	gen  uint64
}

// observe records one notification the child was sent and returns the
// URIs to pull, the first one first, and whether the rest are a sweep.
func (p *pullState) observe(method string, params json.RawMessage) (uris []string, sweep bool) {
	var d struct {
		TextDocument struct {
			URI     string `json:"uri"`
			Version int    `json:"version"`
		} `json:"textDocument"`
	}
	switch method {
	case "textDocument/didOpen", "textDocument/didChange", "textDocument/didSave", "textDocument/didClose":
		if json.Unmarshal(params, &d) != nil || d.TextDocument.URI == "" {
			return nil, false
		}
	default:
		return nil, false
	}
	uri := d.TextDocument.URI
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open == nil {
		p.open = map[string]int{}
	}
	switch method {
	case "textDocument/didOpen":
		p.open[uri] = d.TextDocument.Version
		return []string{uri}, false
	case "textDocument/didChange":
		p.open[uri] = d.TextDocument.Version
	case "textDocument/didClose":
		delete(p.open, uri)
		return nil, false
	}
	if _, ok := p.open[uri]; !ok {
		return nil, false // didSave of a document not open
	}
	return append([]string{uri}, p.othersLocked(uri)...), true
}

func (p *pullState) othersLocked(skip string) []string {
	out := make([]string, 0, len(p.open))
	for u := range p.open {
		if u != skip {
			out = append(out, u)
		}
	}
	return out
}

// all is every open document.
func (p *pullState) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.othersLocked("")
}

// version is the open document's version; false when it is not open.
func (p *pullState) version(uri string) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.open[uri]
	return v, ok
}

// nextGen starts a sweep and returns its generation.
func (p *pullState) nextGen() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gen++
	return p.gen
}

func (p *pullState) current(gen uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gen == gen
}

// afterNotify runs the pulls a notification calls for, in the
// background. No-op for a push child.
func (c *Child) afterNotify(method string, params json.RawMessage) {
	if c.pull == nil {
		return
	}
	uris, sweep := c.pull.observe(method, params)
	if len(uris) == 0 {
		return
	}
	if !sweep {
		go c.pullOne(uris[0])
		return
	}
	go c.pullSweep(c.pull.nextGen(), uris)
}

// pullSweep pulls uris in order, stopping when a newer sweep starts.
func (c *Child) pullSweep(gen uint64, uris []string) {
	for _, u := range uris {
		if !c.pull.current(gen) {
			return
		}
		c.pullOne(u)
	}
}

// pullOne asks the child for uri's diagnostics and hands the report to
// the notification handler as a publishDiagnostics.
func (c *Child) pullOne(uri string) {
	v, ok := c.pull.version(uri)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pullTimeout)
	defer cancel()
	raw, err := c.Call(ctx, "textDocument/diagnostic", map[string]any{"textDocument": map[string]any{"uri": uri}})
	if err != nil {
		return
	}
	var rep struct {
		Kind  string            `json:"kind"`
		Items []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(raw, &rep) != nil || rep.Kind != "full" {
		return
	}
	if now, ok := c.pull.version(uri); !ok || now != v {
		return
	}
	if rep.Items == nil {
		rep.Items = []json.RawMessage{}
	}
	params, err := json.Marshal(map[string]any{"uri": uri, "version": v, "diagnostics": rep.Items})
	if err != nil {
		return
	}
	traceChild(c.name, "<- (pulled)", "textDocument/publishDiagnostics", params)
	c.notifyMu.Lock()
	fn := c.notifyFn
	c.notifyMu.Unlock()
	if fn != nil {
		fn("textDocument/publishDiagnostics", params)
	}
}
