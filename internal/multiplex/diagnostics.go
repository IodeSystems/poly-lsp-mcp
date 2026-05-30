package multiplex

import (
	"context"
	"encoding/json"
	"sync"
)

// Diagnostic mirrors the LSP `Diagnostic` shape but stays in this
// package so the MCP layer can include it in tool responses without
// reaching into internal/server types. Range fields are 0-based
// (`character` is UTF-16 per LSP); the MCP tool layer translates to
// the 1-based byte form the rest of our wire shapes use.
type Diagnostic struct {
	Range    DiagnosticRange `json:"range"`
	Severity int             `json:"severity,omitempty"` // 1=Error 2=Warning 3=Info 4=Hint
	Code     any             `json:"code,omitempty"`     // int or string per spec
	Source   string          `json:"source,omitempty"`
	Message  string          `json:"message"`
}

type DiagnosticRange struct {
	Start DiagnosticPos `json:"start"`
	End   DiagnosticPos `json:"end"`
}

type DiagnosticPos struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// DiagnosticStore captures `textDocument/publishDiagnostics` from
// every attached child LSP and stores the latest snapshot per URI.
// Subscribers use WaitAfter to block until a new publish arrives — a
// generation counter keeps "wait for the next publish after I made my
// edit" from racing against an earlier publish.
//
// One store can be Attach'd to many children; subsequent publishes
// from any child overwrite the URI's entry. gopls / tsserver / pylsp
// don't overlap on URIs in practice (the registry routes by
// extension), so the last-write-wins model is correct.
type DiagnosticStore struct {
	mu     sync.Mutex
	latest map[string][]Diagnostic
	gen    map[string]uint64
	chans  map[string][]chan struct{}
}

func NewDiagnosticStore() *DiagnosticStore {
	return &DiagnosticStore{
		latest: map[string][]Diagnostic{},
		gen:    map[string]uint64{},
		chans:  map[string][]chan struct{}{},
	}
}

// Attach registers this store as c's notification handler. Any prior
// handler is replaced. Idempotent per child: attaching twice doesn't
// register two listeners.
func (s *DiagnosticStore) Attach(c *Child) {
	c.SetNotificationHandler(func(method string, params json.RawMessage) {
		if method != "textDocument/publishDiagnostics" {
			return
		}
		var p struct {
			URI         string       `json:"uri"`
			Diagnostics []Diagnostic `json:"diagnostics"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return
		}
		if p.Diagnostics == nil {
			p.Diagnostics = []Diagnostic{}
		}
		s.put(p.URI, p.Diagnostics)
	})
}

func (s *DiagnosticStore) put(uri string, diags []Diagnostic) {
	s.mu.Lock()
	s.latest[uri] = diags
	s.gen[uri]++
	waiters := s.chans[uri]
	delete(s.chans, uri)
	s.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
}

// Get returns the latest published diagnostics for uri, or nil if
// nothing has been published for that URI yet.
func (s *DiagnosticStore) Get(uri string) []Diagnostic {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest[uri]
}

// Snapshot returns a copy of the entire store as URI → []Diagnostic.
// Empty result means no child LSP has published yet — either nothing
// is open in the editor (with MCP, that's typical) OR the manager has
// no children. Callers iterate to surface workspace-wide state.
func (s *DiagnosticStore) Snapshot() map[string][]Diagnostic {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]Diagnostic, len(s.latest))
	for uri, diags := range s.latest {
		dup := make([]Diagnostic, len(diags))
		copy(dup, diags)
		out[uri] = dup
	}
	return out
}

// Gen returns the URI's current generation counter. Callers about to
// trigger a publish (didSave / didChange) capture Gen first, then pass
// it to WaitAfter — that way we only wake on publishes that arrive
// AFTER the captured point, even if a publish raced in between.
func (s *DiagnosticStore) Gen(uri string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen[uri]
}

// WaitAfter blocks until a publishDiagnostics for uri arrives with a
// generation strictly greater than `since`, or ctx is done. Returns
// the latest snapshot in either case (caller compares ctx.Err() to
// distinguish "fresh result" from "timed out").
//
// If the gen has already advanced past `since` when WaitAfter is
// called, it returns immediately with the current snapshot.
func (s *DiagnosticStore) WaitAfter(ctx context.Context, uri string, since uint64) []Diagnostic {
	s.mu.Lock()
	if s.gen[uri] > since {
		out := s.latest[uri]
		s.mu.Unlock()
		return out
	}
	ch := make(chan struct{})
	s.chans[uri] = append(s.chans[uri], ch)
	s.mu.Unlock()

	select {
	case <-ch:
	case <-ctx.Done():
		s.removeWaiter(uri, ch)
	}
	return s.Get(uri)
}

func (s *DiagnosticStore) removeWaiter(uri string, ch chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing := s.chans[uri]
	kept := existing[:0]
	for _, x := range existing {
		if x != ch {
			kept = append(kept, x)
		}
	}
	if len(kept) == 0 {
		delete(s.chans, uri)
	} else {
		s.chans[uri] = kept
	}
}
