package daemon

import (
	"encoding/json"
	"log"
	"sync"
)

// The notification hub. A stdio server writes its notifications to its one
// client; a daemon-hosted root has many clients and no stream of its own,
// so every notification it sends is published here and delivered to each
// session holding the root, over that session's /session/events stream.
// The proxy writes them to its stdio client, so a client sees the same
// notifications in both modes.
//
// The bring-up's outcome — the latest poly-lsp-mcp/index and
// poly-lsp-mcp/setup notification of each root — is kept and replayed to a
// session that attaches after it was sent: the second client of a warm
// root gets the index report the first one got. Daemon-wide notices
// (poly-lsp-mcp/daemon) go to every session.

// replayed are the loggers whose latest notification a late session gets.
var replayed = map[string]bool{indexLogger: true, setupLogger: true}

const (
	indexLogger  = "poly-lsp-mcp/index"
	setupLogger  = "poly-lsp-mcp/setup"
	daemonLogger = "poly-lsp-mcp/daemon"
)

// event is one notification on its way to a session: one NDJSON line of
// /session/events.
type event struct {
	Root   string          `json:"root,omitempty"` // "" = daemon-wide
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`

	seq uint64
	key string // root + logger, for replay de-duplication; "" when not replayed
}

type subscriber struct {
	ch   chan event
	seen map[string]uint64 // key → latest seq delivered
}

type hub struct {
	mu   sync.Mutex
	seq  uint64
	last map[string]map[string]event // root → logger → latest replayed event
	subs map[string]*subscriber      // session → its stream
}

func newHub() *hub {
	return &hub{last: map[string]map[string]event{}, subs: map[string]*subscriber{}}
}

// subBuffer bounds what a slow stream may lag by; past it, events drop for
// that session (logged) rather than block the root that sent them.
const subBuffer = 256

// subscribe opens session's stream and replays the kept notifications of
// roots, the roots the session already holds. A second subscribe for the
// session (a reconnect) replaces the first. cancel reports whether this
// stream was still the session's own when it ended.
func (h *hub) subscribe(session string, roots []string) (<-chan event, func() bool) {
	s := &subscriber{ch: make(chan event, subBuffer), seen: map[string]uint64{}}
	h.mu.Lock()
	h.subs[session] = s
	for _, root := range roots {
		h.replayLocked(s, root)
	}
	h.mu.Unlock()
	return s.ch, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.subs[session] != s {
			return false
		}
		delete(h.subs, session)
		return true
	}
}

// replay sends root's kept notifications to session, if it is subscribed:
// called when a session takes a hold on a root.
func (h *hub) replay(session, root string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.subs[session]; ok {
		h.replayLocked(s, root)
	}
}

func (h *hub) replayLocked(s *subscriber, root string) {
	for _, ev := range h.last[root] {
		h.deliverLocked(s, ev)
	}
}

// publish sends a root's notification to the sessions in to, keeping it
// for replay when its logger is one a late session needs.
func (h *hub) publish(root, logger, method string, params json.RawMessage, to []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	ev := event{Root: root, Method: method, Params: params, seq: h.seq}
	if replayed[logger] {
		ev.key = root + "\x00" + logger
		if h.last[root] == nil {
			h.last[root] = map[string]event{}
		}
		h.last[root][logger] = ev
	}
	for _, sess := range to {
		if s, ok := h.subs[sess]; ok {
			h.deliverLocked(s, ev)
		}
	}
}

// broadcast sends a daemon-wide notification to every session.
func (h *hub) broadcast(method string, params json.RawMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	ev := event{Method: method, Params: params, seq: h.seq}
	for _, s := range h.subs {
		h.deliverLocked(s, ev)
	}
}

func (h *hub) deliverLocked(s *subscriber, ev event) {
	if ev.key != "" {
		if s.seen[ev.key] >= ev.seq {
			return
		}
		s.seen[ev.key] = ev.seq
	}
	select {
	case s.ch <- ev:
	default:
		log.Printf("daemon: a session's event stream is %d behind; dropped %s", subBuffer, ev.Method)
	}
}

// forget drops a root's kept notifications: it was evicted or is rebuilt,
// and what it said no longer holds.
func (h *hub) forget(root string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.last, root)
}
