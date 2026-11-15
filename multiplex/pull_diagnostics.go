package multiplex

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
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
//   - didChange, didSave: that document first, alone; then the other open
//     documents, most recently touched first, at most pullSiblingMax of
//     them (an edit's fallout lands in siblings, as with a push server);
//   - workspace/diagnostic/refresh (sent after file events it judged
//     relevant, e.g. a package install): every open document, most
//     recently touched first.
//
// Every pull but an edited document's own goes through a limiter of
// pullConcurrency. A newer sweep stops the sibling pulls an older one has
// not started; an edited document's own pull is never stopped.
//
// A report is published with the document version it was asked for. It
// is dropped when the document changed, closed or was reopened in the
// meantime, or when a pull of it started later has been published: a
// newer pull is on its way or has landed. A report equal to the last one
// published for that document and version is not published again: a
// publish bumps the store's generation, and the sibling rollup reads a
// bump as fallout of the edit. tsgo sends no resultId (typescript-go
// internal/ls/diagnostics.go ProvideDiagnostics: always a full report),
// so the items are compared.

// pullTimeout bounds one textDocument/diagnostic.
const pullTimeout = 30 * time.Second

// pullConcurrency bounds the pulls in flight per child, an edited
// document's own pull excepted.
const pullConcurrency = 4

// pullSiblingMax bounds the siblings one edit pulls (docs/design.md).
const pullSiblingMax = 256

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

// pullDoc is one open document of a pull child.
type pullDoc struct {
	version int
	epoch   uint64 // seq at its didOpen: a reopen is a new epoch
	touched uint64 // seq at its last didOpen, didChange or didSave

	// The last report published for it: its pull's seq, the version it
	// carried and its items. published is false before the first.
	published bool
	pubSeq    uint64
	pubVer    int
	pubItems  []byte
}

// pullState is the open documents of a pull child by URI, one sequence
// counter for epochs, touches and pulls, a sweep generation (a newer
// sweep stops an older one), and the pull limiter.
type pullState struct {
	mu   sync.Mutex
	docs map[string]*pullDoc
	seq  uint64
	gen  uint64

	sem chan struct{}
	// deliverMu spans deciding to publish a report and publishing it, so
	// reports reach the handler in the order they were accepted.
	deliverMu sync.Mutex
}

func newPullState() *pullState {
	return &pullState{docs: map[string]*pullDoc{}, sem: make(chan struct{}, pullConcurrency)}
}

// pullPlan is the pulls one event calls for: primary (an edited
// document: not limited, never stopped), then each of rest through the
// limiter. A sweep (gen != 0) stops when a newer one starts.
type pullPlan struct {
	primary string
	rest    []string
	gen     uint64
}

// observe records one notification the child was sent and returns the
// pulls it calls for. Callers keep it in the order the notifications
// were written (Child.Notify holds writeMu).
func (p *pullState) observe(method string, params json.RawMessage) pullPlan {
	var d struct {
		TextDocument struct {
			URI     string `json:"uri"`
			Version int    `json:"version"`
		} `json:"textDocument"`
	}
	switch method {
	case "textDocument/didOpen", "textDocument/didChange", "textDocument/didSave", "textDocument/didClose":
		if json.Unmarshal(params, &d) != nil || d.TextDocument.URI == "" {
			return pullPlan{}
		}
	default:
		return pullPlan{}
	}
	uri := d.TextDocument.URI
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	switch method {
	case "textDocument/didOpen":
		p.docs[uri] = &pullDoc{version: d.TextDocument.Version, epoch: p.seq, touched: p.seq}
		return pullPlan{rest: []string{uri}}
	case "textDocument/didClose":
		delete(p.docs, uri)
		return pullPlan{}
	}
	doc, ok := p.docs[uri]
	if !ok {
		return pullPlan{} // didChange or didSave of a document not open
	}
	if method == "textDocument/didChange" {
		doc.version = d.TextDocument.Version
	}
	doc.touched = p.seq
	rest := p.byTouchLocked(uri)
	if len(rest) > pullSiblingMax {
		rest = rest[:pullSiblingMax]
	}
	p.gen++
	return pullPlan{primary: uri, rest: rest, gen: p.gen}
}

// byTouchLocked is the open documents but skip, most recently touched
// first.
func (p *pullState) byTouchLocked(skip string) []string {
	out := make([]string, 0, len(p.docs))
	for u := range p.docs {
		if u != skip {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return p.docs[out[i]].touched > p.docs[out[j]].touched })
	return out
}

// refresh starts a sweep of every open document, most recently touched
// first.
func (p *pullState) refresh() pullPlan {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gen++
	return pullPlan{rest: p.byTouchLocked(""), gen: p.gen}
}

func (p *pullState) current(gen uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gen == gen
}

// pullTicket is one pull: the document's epoch and version when it
// started, and its seq.
type pullTicket struct {
	epoch   uint64
	version int
	seq     uint64
}

// start begins a pull of uri; false when it is not open.
func (p *pullState) start(uri string) (pullTicket, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	doc, ok := p.docs[uri]
	if !ok {
		return pullTicket{}, false
	}
	p.seq++
	return pullTicket{epoch: doc.epoch, version: doc.version, seq: p.seq}, true
}

// PulledUnchanged is the notification a pull child hands its handler for
// a report equal to the last one published ({"uri": ...}). The store
// counts it as activity for the settle and changes no URI's generation.
const PulledUnchanged = "$/poly-lsp/pulledUnchanged"

// pullVerdict is what becomes of one pull's report.
type pullVerdict int

const (
	pullPublish   pullVerdict = iota
	pullUnchanged             // equal to the last published: activity only
	pullDrop                  // stale
)

// accept decides what becomes of items, the report of pull t of uri, and
// records it as published unless it is dropped. The caller holds
// deliverMu until it has handed it on.
func (p *pullState) accept(uri string, t pullTicket, items []byte) pullVerdict {
	p.mu.Lock()
	defer p.mu.Unlock()
	doc, ok := p.docs[uri]
	if !ok || doc.epoch != t.epoch || doc.version != t.version {
		return pullDrop // changed, closed or reopened since: a newer pull follows
	}
	if doc.published && t.seq <= doc.pubSeq {
		return pullDrop // a pull started later has been published
	}
	same := doc.published && doc.pubVer == t.version && bytes.Equal(doc.pubItems, items)
	doc.published, doc.pubSeq, doc.pubVer, doc.pubItems = true, t.seq, t.version, items
	if same {
		return pullUnchanged
	}
	return pullPublish
}

// runPulls runs pl in the background. No-op for an empty plan.
func (c *Child) runPulls(pl pullPlan) {
	if pl.primary == "" && len(pl.rest) == 0 {
		return
	}
	go func() {
		if pl.primary != "" {
			c.pullOne(pl.primary)
		}
		for _, u := range pl.rest {
			c.pull.sem <- struct{}{}
			if pl.gen != 0 && !c.pull.current(pl.gen) {
				<-c.pull.sem
				return
			}
			go func(u string) {
				defer func() { <-c.pull.sem }()
				c.pullOne(u)
			}(u)
		}
	}()
}

// pullOne asks the child for uri's diagnostics and hands the report to
// the notification handler as a publishDiagnostics.
func (c *Child) pullOne(uri string) {
	t, ok := c.pull.start(uri)
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
		Kind  string          `json:"kind"`
		Items json.RawMessage `json:"items"`
	}
	if json.Unmarshal(raw, &rep) != nil || rep.Kind != "full" {
		return
	}
	if len(rep.Items) == 0 || string(rep.Items) == "null" {
		rep.Items = json.RawMessage("[]")
	}
	c.pull.deliverMu.Lock()
	defer c.pull.deliverMu.Unlock()
	method := "textDocument/publishDiagnostics"
	var params []byte
	switch c.pull.accept(uri, t, rep.Items) {
	case pullDrop:
		return
	case pullUnchanged:
		method = PulledUnchanged
		params, err = json.Marshal(map[string]any{"uri": uri})
	default:
		params, err = json.Marshal(map[string]any{"uri": uri, "version": t.version, "diagnostics": rep.Items})
	}
	if err != nil {
		return
	}
	traceChild(c.name, "<- (pulled)", method, params)
	c.notifyMu.Lock()
	fn := c.notifyFn
	c.notifyMu.Unlock()
	if fn != nil {
		fn(method, params)
	}
}
