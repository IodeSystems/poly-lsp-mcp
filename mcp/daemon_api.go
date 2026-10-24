package mcp

// Exported surface the daemon uses to drive a Server in-process, without
// the stdio JSON-RPC handshake. A daemon hosts one *Server per workspace
// root and calls Init once, CallTool per request, and Shutdown on
// eviction — the same machinery handleInitialize / handleToolsCall /
// the shutdown method reach over stdio, exposed as plain method calls.

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/multiplex"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// SetParseCache replaces this server's private parse cache with a shared
// one so a daemon hosting many roots parses identical file content once
// (the cache is content-keyed and its own mutex makes it safe to share).
// Call after New and before Init/Serve. In daemon mode the daemon owns
// the cache's load/save, so leave SetCachePath unset on shared servers.
func (s *Server) SetParseCache(c *symbols.ParseCache) {
	if c != nil {
		s.parseCache = c
	}
}

// Init performs the workspace bring-up handleInitialize does for a stdio
// session — build the index, then (server-lifecycle only) start the
// child-LSP manager, git prewarm, and file watcher — but callable
// directly by a daemon that never speaks the initialize handshake. A
// BuildIndex failure is returned; the manager/prewarm/watch are only
// started on success, matching the stdio path.
func (s *Server) Init() error {
	if s.getRoot() == "" {
		return errors.New("no workspace root configured")
	}
	if err := s.BuildIndex(); err != nil {
		return err
	}
	s.StartLSPs()
	s.kickFileWatch()
	return nil
}

// StartLSPs starts the child-LSP manager and the git prewarm over the
// built index. Once per server: Init calls it, and a daemon that warmed a
// root without its child LSPs (BringUp(false), a takeover) calls it on the
// root's first real request.
func (s *Server) StartLSPs() {
	s.lspOnce.Do(func() {
		s.startManagerIfPresent(s.getIndex())
		s.kickGitPrewarm()
	})
}

// BringUp is a daemon-hosted root's bring-up: what an asynchronous stdio
// initialize runs behind its reply, run in the caller's goroutine — build
// the index (bounded by SetIndexBudget), start the watcher, and, when
// startLSPs, the child LSPs. It ends with the same poly-lsp-mcp/index and
// poly-lsp-mcp/setup notifications, which reach the client through
// SetNotifier. A takeover warms with startLSPs false: a second gopls fleet
// while the old daemon still runs its own would double the memory the swap
// costs, and a child LSP is not what makes a root slow to come back.
func (s *Server) BringUp(startLSPs bool) error {
	start := time.Now()
	var err error
	if s.getRoot() == "" {
		err = errors.New("no workspace root configured")
	} else {
		err = s.BuildIndex()
	}
	if err == nil {
		if startLSPs {
			s.StartLSPs()
		}
		s.kickFileWatch()
	}
	s.setInitErr(err)
	s.notifyIndexState(err, time.Since(start))
	s.notifySetup()
	return err
}

// IndexFailure is what a tool call answers when the bring-up failed: the
// error, the post-mortem when there is one, and the remedy — the text a
// stdio server returns, for the daemon to return the same.
func (s *Server) IndexFailure(err error) string { return s.indexFailure(err) }

// SetNotifier routes this server's unsolicited notifications (index and
// setup state, conflicts, warnings) to fn instead of a stdio stream: a
// daemon has no stream per root, and fans them out to the sessions that
// hold the root. Set before the bring-up.
func (s *Server) SetNotifier(fn func(method string, params map[string]any)) {
	s.notifier = fn
}

// Shutdown tears down the child-LSP manager and file watcher and
// persists the parse cache if a cache path is configured. Mirrors the
// shutdown JSON-RPC method's teardown so a daemon can evict a warm
// Server cleanly. Safe to call more than once.
func (s *Server) Shutdown() {
	s.stopManagerIfPresent()
	s.stopFileWatch()
	s.maybeSaveCache()
}

// StopLSPs stops the root's running child LSPs, as an idle stop does:
// each starts again on the next request that needs it, and the index
// stays. A daemon calls it a grace period after a root's last client
// left. Returns how many stopped.
func (s *Server) StopLSPs() int {
	if s.manager == nil {
		return 0
	}
	return s.manager.StopAll(multiplex.StopNoClient)
}

// CallOptions carries a per-CONNECTION policy the daemon enforces at its
// boundary, because the hosted *Server is shared across clients and its own
// read-only/validate flags are process-global. Policy can only TIGHTEN: a
// client may add read-only or validation on top of the daemon baseline, never
// remove it (a read-only daemon stays read-only for everyone).
type CallOptions struct {
	ReadOnly bool // reject mutating tools for this call
	Validate bool // force revert-on-new-diagnostics for this edit
}

// CallTool invokes a registered MCP tool in-process — the exported seam
// the daemon uses in place of the stdio tools/call path. Dispatch and
// error shaping mirror handleToolsCall exactly (a handler error becomes
// an isError text result there; here the raw error is returned so the
// caller can choose the wire shape). sess is the client's session id
// (X-Poly-Session), used to isolate its edit batch from other clients on
// the same root; "" maps to the implicit local session. opts is the
// per-connection policy enforced HERE, at the boundary.
func (s *Server) CallTool(sess, name string, args json.RawMessage, opts CallOptions) (content []Content, isError bool, err error) {
	if opts.ReadOnly && IsMutatingTool(name) {
		return nil, true, fmt.Errorf("read-only session: %s is not permitted", name)
	}
	tool, ok := s.tools[name]
	if !ok {
		return nil, true, fmt.Errorf("unknown tool: %s", name)
	}
	// Same argument check the stdio dispatch runs — the daemon serves the same
	// tools, so an argument the tool does not have must be refused here too
	// rather than being silently dropped into a different answer.
	if err := checkToolArgs(&tool, args); err != nil {
		return []Content{{Type: "text", Text: err.Error()}}, true, nil
	}
	// A validate connection forces validate:true onto edits (the tool already
	// honors a per-call validate flag); non-edit tools ignore the extra field.
	if opts.Validate && IsMutatingTool(name) {
		args = withValidate(args)
	}
	return tool.Handler(s, normSession(sess), args)
}

// withValidate merges "validate":true into a tool's JSON arguments, so a
// validate connection's edits are checked even when the client didn't ask
// per-call. Malformed/empty args degrade to a bare {"validate":true}; a real
// parse error surfaces later in the handler's own unmarshal.
func withValidate(args json.RawMessage) json.RawMessage {
	m := map[string]json.RawMessage{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &m); err != nil {
			return args // let the handler report the malformed args
		}
	}
	m["validate"] = json.RawMessage("true")
	out, err := json.Marshal(m)
	if err != nil {
		return args
	}
	return out
}

// IndexedNames reports how many symbol names the current index holds
// (0 before Init or on an empty workspace) — a cheap warmth signal for
// the daemon's /open response.
func (s *Server) IndexedNames() int {
	idx := s.getIndex()
	if idx == nil {
		return 0
	}
	return len(idx.Names())
}

// ToolDescriptor is one entry of the tool catalog, the shape tools/list
// emits, so a daemon/proxy can answer tools/list without a live stdio
// server.
type ToolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Tools returns the registered tool catalog in deterministic name order
// (so LLM-side prompt caches don't churn), reflecting the current
// legacy/read-only surface.
func (s *Server) Tools() []ToolDescriptor {
	out := make([]ToolDescriptor, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, ToolDescriptor{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
