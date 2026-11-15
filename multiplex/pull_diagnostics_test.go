package multiplex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/internal/jsonrpc"
)

func TestPullsDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		path string
		args []string
		want bool
	}{
		{"/p/node_modules/@typescript/typescript-linux-x64/lib/tsc", []string{"--lsp", "--stdio"}, true},
		{"/bin/tsgo", []string{"--lsp", "--stdio"}, true},
		{"/bin/tsc", []string{"-p", "."}, false},
		{"/bin/typescript-language-server", []string{"--stdio"}, false},
		{"/bin/gopls", nil, false},
	} {
		if got := pullsDiagnostics(tc.path, tc.args); got != tc.want {
			t.Errorf("pullsDiagnostics(%s %v) = %v, want %v", tc.path, tc.args, got, tc.want)
		}
	}
}

func docParams(uri string, v int) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"textDocument": map[string]any{"uri": uri, "version": v}})
	return b
}

func TestPullStateObserve(t *testing.T) {
	p := newPullState()
	if pl := p.observe("textDocument/didOpen", docParams("a", 1)); pl.primary != "" || len(pl.rest) != 1 || pl.rest[0] != "a" || pl.gen != 0 {
		t.Fatalf("didOpen a: %+v", pl)
	}
	p.observe("textDocument/didOpen", docParams("b", 1))
	p.observe("textDocument/didOpen", docParams("c", 1))
	pl := p.observe("textDocument/didChange", docParams("b", 2))
	// b first, alone; then the siblings, most recently touched first.
	if pl.primary != "b" || fmt.Sprint(pl.rest) != "[c a]" || pl.gen == 0 {
		t.Fatalf("didChange b: %+v, want primary b, rest [c a]", pl)
	}
	if fmt.Sprint(p.refresh().rest) != "[b c a]" {
		t.Fatalf("refresh order %v, want [b c a]", p.refresh().rest)
	}
	if !p.current(p.refresh().gen) || p.current(pl.gen) {
		t.Fatal("an older sweep is still current")
	}
	if pl := p.observe("textDocument/didClose", docParams("c", 0)); pl.primary != "" || pl.rest != nil {
		t.Fatalf("didClose pulls %+v", pl)
	}
	if pl := p.observe("textDocument/didSave", docParams("c", 0)); pl.primary != "" || pl.rest != nil {
		t.Fatalf("didSave of a closed doc pulls %+v", pl)
	}
	if pl := p.observe("workspace/didChangeWatchedFiles", json.RawMessage(`{"changes":[]}`)); pl.primary != "" || pl.rest != nil {
		t.Fatalf("watched files pulls %+v", pl)
	}
	for i := range pullSiblingMax + 10 {
		p.observe("textDocument/didOpen", docParams(fmt.Sprintf("s%d", i), 1))
	}
	if n := len(p.observe("textDocument/didSave", docParams("a", 0)).rest); n != pullSiblingMax {
		t.Fatalf("edit pulls %d siblings, want %d", n, pullSiblingMax)
	}
}

// fakePull is a pull child with no process: its diagnostic requests
// arrive on reqs and are answered by reply; its publishes arrive on pubs.
type fakePull struct {
	t    *testing.T
	c    *Child
	reqs chan *jsonrpc.Message
	pubs chan string // "uri v<version> <items>"
	// unchanged gets the URI of each PulledUnchanged.
	unchanged chan string
}

func newFakePull(t *testing.T) *fakePull {
	pr, pw := io.Pipe()
	f := &fakePull{
		t:    t,
		reqs: make(chan *jsonrpc.Message, 100),
		pubs:      make(chan string, 100),
		unchanged: make(chan string, 100),
		c: &Child{
			name:    "fake",
			in:      pw,
			pending: map[string]chan *jsonrpc.Message{},
			done:    make(chan struct{}),
			pull:    newPullState(),
		},
	}
	t.Cleanup(func() { close(f.c.done); pw.Close() })
	f.c.SetNotificationHandler(func(method string, params json.RawMessage) {
		var p struct {
			URI         string          `json:"uri"`
			Version     int             `json:"version"`
			Diagnostics json.RawMessage `json:"diagnostics"`
		}
		_ = json.Unmarshal(params, &p)
		if method == PulledUnchanged {
			f.unchanged <- p.URI
			return
		}
		f.pubs <- fmt.Sprintf("%s v%d %s", p.URI, p.Version, p.Diagnostics)
	})
	go func() {
		r := bufio.NewReader(pr)
		for {
			m, err := jsonrpc.Read(r)
			if err != nil {
				return
			}
			if m.Method == "textDocument/diagnostic" {
				f.reqs <- m
			}
		}
	}()
	return f
}

func (f *fakePull) notify(method, uri string, v int) {
	f.t.Helper()
	if err := f.c.Notify(method, map[string]any{"textDocument": map[string]any{"uri": uri, "version": v}}); err != nil {
		f.t.Fatal(err)
	}
}

// next is the next diagnostic request and the URI it asks for.
func (f *fakePull) next() (*jsonrpc.Message, string) {
	f.t.Helper()
	select {
	case m := <-f.reqs:
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(m.Params, &p)
		return m, p.TextDocument.URI
	case <-time.After(5 * time.Second):
		f.t.Fatal("no diagnostic request")
		return nil, ""
	}
}

func (f *fakePull) noRequest() {
	f.t.Helper()
	select {
	case m := <-f.reqs:
		f.t.Fatalf("unexpected request %s", m.Params)
	case <-time.After(150 * time.Millisecond):
	}
}

func (f *fakePull) reply(m *jsonrpc.Message, items string) {
	f.c.handle(&jsonrpc.Message{JSONRPC: "2.0", ID: m.ID, Result: json.RawMessage(`{"kind":"full","items":` + items + `}`)})
}

func (f *fakePull) pub() string {
	f.t.Helper()
	select {
	case p := <-f.pubs:
		return p
	case <-time.After(5 * time.Second):
		f.t.Fatal("no publish")
		return ""
	}
}

func (f *fakePull) noPub() {
	f.t.Helper()
	select {
	case p := <-f.pubs:
		f.t.Fatalf("unexpected publish %s", p)
	case <-time.After(150 * time.Millisecond):
	}
}

const (
	itemsX = `[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"message":"x"}]`
	itemsY = `[{"range":{"start":{"line":1,"character":0},"end":{"line":1,"character":1}},"message":"y"}]`
)

// A report equal to the last one published for that document and version
// is not published again; a new version is, even with equal items.
func TestPullSuppressesUnchangedReport(t *testing.T) {
	f := newFakePull(t)
	f.notify("textDocument/didOpen", "a", 1)
	m, _ := f.next()
	f.reply(m, itemsX)
	if p := f.pub(); p != "a v1 "+itemsX {
		t.Fatalf("publish %s", p)
	}
	f.notify("textDocument/didOpen", "b", 1)
	m, _ = f.next()
	f.reply(m, itemsY)
	f.pub()

	f.notify("textDocument/didSave", "a", 0) // a, then sibling b: both unchanged
	m, u := f.next()
	f.reply(m, itemsX)
	m, u2 := f.next()
	f.reply(m, itemsY)
	if u != "a" || u2 != "b" {
		t.Fatalf("pulled %s, %s; want a, b", u, u2)
	}
	f.noPub()
	// Each is still handed on as activity for the settle.
	for _, want := range []string{"a", "b"} {
		select {
		case got := <-f.unchanged:
			if got != want {
				t.Fatalf("unchanged %s, want %s", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no PulledUnchanged for %s", want)
		}
	}

	f.notify("textDocument/didChange", "a", 2)
	m, _ = f.next()
	f.reply(m, itemsX)
	if p := f.pub(); p != "a v2 "+itemsX {
		t.Fatalf("publish %s, want a at v2", p)
	}
	m, _ = f.next()
	f.reply(m, `[]`) // b's fallout cleared
	if p := f.pub(); p != "b v1 []" {
		t.Fatalf("publish %s, want b cleared", p)
	}
}

// The edited document is pulled first and alone; siblings follow once its
// report is in.
func TestPullEditedFirst(t *testing.T) {
	f := newFakePull(t)
	for _, u := range []string{"a", "b", "c"} {
		f.notify("textDocument/didOpen", u, 1)
		m, _ := f.next()
		f.reply(m, `[]`)
		f.pub()
	}
	f.notify("textDocument/didChange", "a", 2)
	m, u := f.next()
	if u != "a" {
		t.Fatalf("first pull %s, want the edited a", u)
	}
	f.noRequest()
	f.reply(m, itemsX)
	f.pub()
	got := map[string]bool{}
	for range 2 {
		m, u := f.next()
		got[u] = true
		f.reply(m, `[]`)
	}
	if !got["b"] || !got["c"] {
		t.Fatalf("siblings pulled %v", got)
	}
}

// An unchanged pulled report is activity for the settle, not a publish.
func TestStorePulledUnchangedIsActivityOnly(t *testing.T) {
	s := NewDiagnosticStore()
	s.put("a", []Diagnostic{}, nil, false)
	g, total := s.Gen("a"), s.TotalGen()
	s.activity()
	if s.Gen("a") != g || s.TotalGen() != total+1 {
		t.Fatalf("gen %d -> %d, total %d -> %d; want gen unchanged, total +1", g, s.Gen("a"), total, s.TotalGen())
	}
}

// At most pullConcurrency pulls are in flight, a didOpen replay included.
func TestPullConcurrencyBound(t *testing.T) {
	f := newFakePull(t)
	for i := range 10 {
		f.notify("textDocument/didOpen", fmt.Sprintf("d%d", i), 1)
	}
	var held []*jsonrpc.Message
	for range pullConcurrency {
		m, _ := f.next()
		held = append(held, m)
	}
	f.noRequest()
	f.reply(held[0], `[]`)
	f.next()
	f.noRequest()
}

// A report from a pull that started earlier than one already published is
// dropped.
func TestPullDropsStaleResponse(t *testing.T) {
	f := newFakePull(t)
	f.notify("textDocument/didOpen", "a", 1)
	older, _ := f.next()
	f.notify("textDocument/didSave", "a", 0)
	newer, _ := f.next()
	f.reply(newer, itemsY)
	if p := f.pub(); p != "a v1 "+itemsY {
		t.Fatalf("publish %s", p)
	}
	f.reply(older, itemsX)
	f.noPub()
}

// A pull in flight across a didClose and a reopen at the same version is
// dropped.
func TestPullDropsAcrossReopen(t *testing.T) {
	f := newFakePull(t)
	f.notify("textDocument/didOpen", "a", 1)
	before, _ := f.next()
	f.notify("textDocument/didClose", "a", 0)
	f.notify("textDocument/didOpen", "a", 1)
	after, _ := f.next()
	f.reply(before, itemsX)
	f.noPub()
	f.reply(after, itemsY)
	if p := f.pub(); p != "a v1 "+itemsY {
		t.Fatalf("publish %s", p)
	}
}
