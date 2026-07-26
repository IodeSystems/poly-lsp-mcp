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
	s.startManagerIfPresent(s.getIndex())
	s.kickGitPrewarm()
	s.kickFileWatch()
	return nil
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
