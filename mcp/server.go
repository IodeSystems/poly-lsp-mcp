// Package mcp serves the Model Context Protocol layer of poly-lsp-mcp.
// Unlike the LSP layer (which exists to talk to editors), MCP exposes
// the same cross-language symbol/bindings/schemas machinery to LLM
// agents through a small set of typed tools.
//
// Transport: newline-delimited JSON-RPC 2.0 over stdio. We share the
// jsonrpc.Message struct with the LSP layer for the on-the-wire shape
// but skip the LSP-style Content-Length framing — MCP servers stream
// one JSON value per line.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"

	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/bindings"
	"github.com/iodesystems/poly-lsp-mcp/internal/jsonrpc"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// protocolVersion is the date-coded MCP protocol revision we advertise.
const protocolVersion = "2024-11-05"

// JSON-RPC / MCP error codes we surface explicitly.
const (
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternal       = -32603
	errServerNotInit  = -32002
)

// ErrExitWithoutShutdown matches the LSP-side sentinel: returned from
// Serve when the client stream closes without an orderly shutdown.
var ErrExitWithoutShutdown = errors.New("mcp: stream closed before shutdown")

// Server holds the per-session state for one MCP connection: the
// workspace registry/bindings/schemas the symbol index will be built
// from, lifecycle flags, and the tool table populated by registerTools.
type Server struct {
	registry *config.Registry
	bindings []config.Binding
	schemas  []config.Schema

	rootMu sync.RWMutex
	root   string

	writeMu sync.Mutex
	enc     *json.Encoder

	stateMu     sync.Mutex
	gotInit     bool
	initialized bool
	shutdown    bool

	indexMu sync.RWMutex
	index   *symbols.Index

	// classCount tallies symbols per class (func/type/…) for the query-cost
	// estimator's a-priori bare-class figure. It needs the SYMBOL TREE (the
	// index has names, not classes), so it is a one-time full-symbol walk
	// memoized against the index generation — recomputed only when the
	// index actually changes. See classCounts.
	statsMu    sync.Mutex
	statsGen   uint64
	statsClass map[string]int

	// derivRoots maps a symbol name → the @derived sources registered for it
	// (gat operation / sqlc column). node_refactor consults this to detect when
	// a rename touches a derivation graph and must resolve a mode (the variance
	// model) rather than acting blindly. Rebuilt on every index build.
	derivMu    sync.RWMutex
	derivRoots map[string][]bindings.DerivRoot

	// parseCache is shared across all Build calls on this server so
	// refresh() and refresh(workspace_root=…) only re-parse files
	// whose bytes actually changed. See symbols.ParseCache.
	parseCache *symbols.ParseCache

	// cachePath is where parseCache is persisted between runs. Empty
	// disables persistence (the cache lives only as long as the
	// process). Production main.go sets this; tests typically don't.
	cachePath string

	// manager owns child LSPs. Optional: nil means "index-only", in
	// which case node_edit/node_delete/node_refactor return
	// diagnosticsAvailable=false (no compiler talking to us). When
	// present, edits route didOpen/didChange/didSave through the
	// matching child and we wait briefly for publishDiagnostics.
	manager *multiplex.Manager

	// openDocs tracks which (URI, version) we've informed the child
	// about. First write to a URI sends didOpen; subsequent writes
	// send didChange. The version monotonically increases per LSP
	// spec.
	openDocsMu sync.Mutex
	openDocs   map[string]int32

	// diagnosticWait is the per-edit deadline for publishDiagnostics.
	// 0 means use the default (1500ms). Tests set a smaller value to
	// stay fast.
	diagnosticWait time.Duration

	// proactiveOpen controls whether handleInitialize, after spawning
	// the manager, walks the workspace and sends didOpen for every
	// file with a child LSP. This is how the poly-lsp-mcp://diagnostics
	// resource becomes useful before any edits — gopls (and most LSPs)
	// only publish after didOpen / didSave. Default true.
	proactiveOpen bool

	// legacyTools / readOnly are remembered so SetLegacyTools and SetReadOnly
	// compose in either call order: each re-registers the base surface, then
	// re-applies the read-only filter.
	legacyTools bool
	readOnly    bool
	// validateEdits: when true, node_edit reverts any write that introduces a
	// new error (revert-on-new-diagnostics). Set by --validate. See validate.go.
	validateEdits bool

	// queryWorkBudget caps one node_query evaluation's WORK (nodes
	// visited + edges crossed + sites/lines scanned). 0 = the default.
	// See engine.spend — tripping it is loud, never silent.
	queryWorkBudget int

	// lspResolveCap bounds child-LSP round-trips per query (see
	// precision.go). 0 = defaultLSPResolveCap.
	lspResolveCap int

	// proactiveOpenDoneMu guards proactiveOpenDone. The channel is
	// (re)created at the start of each proactive walk and closed when
	// it finishes. WaitForProactiveOpen reads it under the lock so a
	// caller that races with initialize doesn't see a nil channel.
	proactiveOpenDoneMu sync.Mutex
	proactiveOpenDone   chan struct{}

	// gitPrewarm controls whether handleInitialize walks ancestor
	// branches in git and seeds the parse cache from their contents.
	// Lets later branch-switches re-use parses for unchanged files
	// without having to visit those branches first. Default true;
	// no-op when not in a git repo or git binary missing.
	gitPrewarm bool

	// gitPrewarmDoneMu guards gitPrewarmDone — same dance as
	// proactiveOpenDone.
	gitPrewarmDoneMu sync.Mutex
	gitPrewarmDone   chan struct{}

	// fileWatch controls the post-initialize filesystem watcher that
	// keeps the symbol index in sync with on-disk changes made outside
	// the tool's own edits (git checkout / mv, another editor). Default
	// true; no-op if the watcher fails to start.
	fileWatch bool

	watcherMu sync.Mutex
	watcher   io.Closer

	// fileWatchDoneMu guards fileWatchDone — same dance as
	// gitPrewarmDone; closed when the watcher goroutine exits.
	fileWatchDoneMu sync.Mutex
	fileWatchDone   chan struct{}

	tools     map[string]Tool
	resources map[string]Resource
}

const defaultDiagnosticWait = 1500 * time.Millisecond

// New constructs an MCP server bound to a workspace. The root is the
// directory whose files the symbol index will cover; bindings and
// schemas are applied (Tier 2 then Tier 3) once `initialize` arrives.
func New(reg *config.Registry, root string, declared []config.Binding, schemas []config.Schema) *Server {
	s := &Server{
		registry:      reg,
		root:          root,
		bindings:      declared,
		schemas:       schemas,
		openDocs:      map[string]int32{},
		proactiveOpen: true,
		gitPrewarm:    true,
		fileWatch:     true,
	}
	s.SetLegacyTools(false)
	s.parseCache = symbols.NewParseCache()
	return s
}

// SetLegacyTools swaps the tool/resource surface. false (the default
// New() leaves it in) is the modern 3-tool surface (node_query,
// node_read, node_edit) with no resources — tool-definition JSON is
// re-sent every turn by the MCP protocol, so the trimmed surface is
// the dominant lever on prompt-token cost. true restores today's
// 9-tool surface (structure, node_references, node_read, node_edit,
// node_delete, node_refactor, search, node_rename_file, node_query)
// plus its 3 resources (workspace/bindings/diagnostics). Call before
// Serve.
func (s *Server) SetLegacyTools(enabled bool) {
	s.legacyTools = enabled
	if enabled {
		s.tools = registerLegacyTools()
		s.resources = registerResources()
	} else {
		s.tools = registerModernTools()
		s.resources = map[string]Resource{}
	}
	s.applyReadOnly()
}

// SetReadOnly removes every mutating tool from the surface, leaving
// navigation and reading. Call before Serve; composes with
// SetLegacyTools in either order.
//
// This is enforcement, not a hint: a tool the model cannot see is a tool it
// cannot call, which is stronger than any instruction not to. For exploration,
// review, or an agent pointed at a repo you don't want written to, that removes
// the whole class of "it edited something" outcomes.
//
// It's also cheaper. node_edit is ~370 of the surface's ~995 tokens — the
// refactor fields (rename/params/return/resolution) are most of its schema —
// so read-only is a ~37% smaller surface for a job that never needed them.
// SetValidateEdits toggles revert-on-new-diagnostics for every mutating edit.
func (s *Server) SetValidateEdits(enabled bool) { s.validateEdits = enabled }

func (s *Server) SetReadOnly(enabled bool) {
	s.readOnly = enabled
	s.SetLegacyTools(s.legacyTools) // re-register, then re-filter
}

// applyReadOnly strips mutating tools from whatever surface is registered. It
// filters by NAME rather than a flag on Tool: the legacy and modern surfaces
// share handlers, and a name list makes "what can write" auditable in one line.
func (s *Server) applyReadOnly() {
	if !s.readOnly {
		return
	}
	for _, name := range []string{
		"node_edit", "node_delete", "node_refactor", "node_rename_file",
	} {
		delete(s.tools, name)
	}
}

// SetProactiveOpen toggles the post-initialize workspace walk that
// sends didOpen to every child LSP for every indexed file. Default
// true — that's what makes poly-lsp-mcp://diagnostics useful before
// any edits. Disable in tests where you want to control timing
// precisely, or in workspaces too large to afford the up-front cost.
func (s *Server) SetProactiveOpen(enabled bool) {
	s.proactiveOpen = enabled
}

// WaitForProactiveOpen blocks until the most recent proactive-open
// walk finishes, or ctx is done. Returns ctx.Err() on timeout, nil
// otherwise. Safe to call before initialize: if no walk has started,
// returns immediately with nil. Primarily for tests that need to
// observe post-open state deterministically.
func (s *Server) WaitForProactiveOpen(ctx context.Context) error {
	s.proactiveOpenDoneMu.Lock()
	ch := s.proactiveOpenDone
	s.proactiveOpenDoneMu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetQueryWorkBudget overrides the per-query work cap (nodes visited +
// edges crossed + sites/lines scanned; default 200000). A tripped
// budget returns PARTIAL results loudly flagged with the repair recipe
// — never an error, never a silent cut. Mainly for tests and for
// monstrous workspaces in either direction.
func (s *Server) SetQueryWorkBudget(n int) {
	s.queryWorkBudget = n
}

// SetLSPResolveCap bounds how many child-LSP round-trips ONE query may
// spend settling ambiguous reference edges (default 200; see
// precision.go). 0 restores the default. Past the cap the remaining
// edges stay lexical and the result says so — same contract as the work
// budget: partial, flagged, never silent. Mainly for tests.
func (s *Server) SetLSPResolveCap(n int) {
	s.lspResolveCap = n
}

// SetGitPrewarm toggles the post-initialize ancestor-branch walk
// that seeds the parse cache from each ancestor's contents. Default
// true. Disable when working outside a git repo (the walk no-ops
// anyway), in CI workloads that don't benefit from branch reuse, or
// in monstrously huge stacks where the up-front cost outweighs the
// later switch-time saving.
func (s *Server) SetGitPrewarm(enabled bool) {
	s.gitPrewarm = enabled
}

// SetFileWatch toggles the post-initialize filesystem watcher that
// keeps the symbol index in sync with on-disk changes made outside the
// tool's own edits (git checkout / mv, another editor). Default true.
// Disable in tests that want deterministic index state, or in huge
// trees where the watch-descriptor cost isn't worth it.
func (s *Server) SetFileWatch(enabled bool) {
	s.fileWatch = enabled
}

// WaitForGitPrewarm blocks until the most recent ancestor-branch
// prewarm finishes, or ctx is done. Returns nil if no walk was
// kicked (git unavailable, prewarm disabled, etc.). Used by tests.
func (s *Server) WaitForGitPrewarm(ctx context.Context) error {
	s.gitPrewarmDoneMu.Lock()
	ch := s.gitPrewarmDone
	s.gitPrewarmDoneMu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetManager attaches a multiplex.Manager so node_edit / node_delete /
// node_refactor can route didOpen/didChange/didSave to child LSPs and
// pick up publishDiagnostics. Call once after New, before Serve.
// Manager.Start is invoked from handleInitialize so child spawn waits
// until we know which languages the workspace actually has.
func (s *Server) SetManager(mgr *multiplex.Manager) {
	s.manager = mgr
}

// SetDiagnosticWait overrides the per-edit deadline for waiting on
// publishDiagnostics. Tests use this to keep fast; main.go leaves it
// at the default (1500ms).
func (s *Server) SetDiagnosticWait(d time.Duration) {
	s.diagnosticWait = d
}

func (s *Server) diagWaitDuration() time.Duration {
	if s.diagnosticWait > 0 {
		return s.diagnosticWait
	}
	return defaultDiagnosticWait
}

// SetCachePath configures persistence: on Serve start the cache loads
// from path if it exists; on Serve return (clean or otherwise) the
// current cache is written back atomically via a temp file. Empty
// path disables persistence (the default — tests get an in-memory-
// only cache). main.go sets this for production runs.
func (s *Server) SetCachePath(path string) {
	s.cachePath = path
}

// Serve reads framed JSON-RPC messages from in and writes responses to
// out until the stream closes or shutdown is observed.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	s.enc = json.NewEncoder(out)
	dec := json.NewDecoder(in)
	s.maybeLoadCache()
	defer s.maybeSaveCache()
	defer s.stopFileWatch()
	for {
		var msg jsonrpc.Message
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				s.stateMu.Lock()
				clean := s.shutdown
				s.stateMu.Unlock()
				if clean {
					return nil
				}
				return ErrExitWithoutShutdown
			}
			return fmt.Errorf("read: %w", err)
		}
		s.dispatch(&msg)
	}
}

// dispatch enforces the MCP lifecycle (only `initialize` allowed before
// it has been processed; nothing but `shutdown` allowed after a prior
// shutdown) and then routes to per-method handlers.
func (s *Server) dispatch(req *jsonrpc.Message) {
	if req.JSONRPC != "2.0" {
		if req.IsNotification() {
			return
		}
		s.replyError(req, errInvalidRequest, `jsonrpc field must be "2.0"`)
		return
	}

	s.stateMu.Lock()
	gotInit := s.gotInit
	shutdown := s.shutdown
	s.stateMu.Unlock()

	if !gotInit && req.Method != "initialize" {
		if !req.IsNotification() {
			s.replyError(req, errServerNotInit, "server not initialized")
		}
		return
	}
	if gotInit && req.Method == "initialize" {
		s.replyError(req, errInvalidRequest, "server already initialized")
		return
	}
	if shutdown && req.Method != "shutdown" {
		if !req.IsNotification() {
			s.replyError(req, errInvalidRequest, "server is shutting down")
		}
		return
	}

	switch req.Method {
	case "initialize":
		s.handleInitialize(req)
		s.stateMu.Lock()
		s.gotInit = true
		s.stateMu.Unlock()
	case "notifications/initialized":
		s.stateMu.Lock()
		s.initialized = true
		s.stateMu.Unlock()
	case "tools/list":
		s.handleToolsList(req)
	case "tools/call":
		s.handleToolsCall(req)
	case "resources/list":
		s.handleResourcesList(req)
	case "resources/read":
		s.handleResourcesRead(req)
	case "shutdown":
		s.stateMu.Lock()
		s.shutdown = true
		s.stateMu.Unlock()
		s.stopManagerIfPresent()
		s.stopFileWatch()
		s.reply(req, json.RawMessage("null"))
	default:
		if req.IsNotification() {
			return
		}
		s.replyError(req, errMethodNotFound, fmt.Sprintf("method not found: %s", req.Method))
	}
}

// BuildIndex builds the symbol index for the workspace root and runs
// the binding passes over it: Tier-2 declared, Tier-3 schema-anchored,
// and the Tier-3 @derived/sqlc auto-bindings that feed the
// derivation-root registry the variance model reads.
//
// Every entry point that queries the workspace must come through here.
// Reference edges (::in/::out) resolve through s.index, so a caller
// that builds the tree WITHOUT this gets a tree that answers
// containment queries normally and silently reports no references at
// all — the failure looks like an empty result, not an error.
func (s *Server) BuildIndex() error {
	root := s.getRoot()
	if root == "" {
		return errors.New("no workspace root configured")
	}
	idx, err := symbols.Build(root, s.registry, symbols.WithCache(s.parseCache))
	if err != nil {
		return fmt.Errorf("index build failed for %s: %w", root, err)
	}
	s.setIndex(idx)
	log.Printf("index: indexed %d names from %s", len(idx.Names()), root)

	resolver := bindings.NewResolver(root)
	if len(s.bindings) > 0 {
		n, err := resolver.Apply(idx, s.bindings)
		if err != nil {
			log.Printf("index: some bindings failed validation: %v", err)
		}
		log.Printf("index: applied %d declared binding site(s)", n)
	}
	if len(s.schemas) > 0 {
		n := resolver.ApplySchemas(idx, s.schemas)
		log.Printf("index: applied %d schema-anchored site(s)", n)
	}
	// Tier-3 auto: gat @derived(operationId) + sqlc derived:"table.column"
	// edges → declared source bindings, and the derivation-root registry the
	// variance model reads.
	var derivRoots []bindings.DerivRoot
	if roots := resolver.ApplyDerived(idx); len(roots) > 0 {
		derivRoots = append(derivRoots, roots...)
		log.Printf("index: applied %d @derived source binding(s)", len(roots))
	}
	if roots := resolver.ApplyDerivedSQL(idx); len(roots) > 0 {
		derivRoots = append(derivRoots, roots...)
		log.Printf("index: applied %d sqlc @derived column binding(s)", len(roots))
	}
	s.setDerivRoots(derivRoots)
	return nil
}

// handleInitialize builds the symbol index for s.getRoot(), applies any
// Tier-2 and Tier-3 bindings, and advertises tool capability.
func (s *Server) handleInitialize(req *jsonrpc.Message) {
	if s.getRoot() != "" {
		if err := s.BuildIndex(); err != nil {
			log.Printf("mcp initialize: %v", err)
		} else {
			// Server-lifecycle only: a one-shot query needs none of
			// these, so they stay out of BuildIndex.
			s.startManagerIfPresent(s.getIndex())
			s.kickGitPrewarm()
			s.kickFileWatch()
		}
	} else {
		log.Print("mcp initialize: no workspace root configured; tools will return errors")
	}

	s.reply(req, map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"tools":     map[string]any{},
			"resources": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "poly-lsp-mcp",
			"version": "0.0.0",
		},
	})
}

// handleToolsList returns the registered tool catalog. Order is
// deterministic by name so LLM-side prompt caches don't churn.
func (s *Server) handleToolsList(req *jsonrpc.Message) {
	type listEntry struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	out := make([]listEntry, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, listEntry{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	// Sort alphabetically.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Name > out[j].Name; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	s.reply(req, map[string]any{"tools": out})
}

// handleResourcesList returns the registered resource catalog. Output
// is sorted by URI so prompt caches don't churn across runs.
func (s *Server) handleResourcesList(req *jsonrpc.Message) {
	type listEntry struct {
		URI         string `json:"uri"`
		Name        string `json:"name"`
		Description string `json:"description"`
		MimeType    string `json:"mimeType,omitempty"`
	}
	out := make([]listEntry, 0, len(s.resources))
	for _, r := range s.resources {
		out = append(out, listEntry{
			URI:         r.URI,
			Name:        r.Name,
			Description: r.Description,
			MimeType:    r.MimeType,
		})
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].URI > out[j].URI; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	s.reply(req, map[string]any{"resources": out})
}

// handleResourcesRead returns the content of a single resource by URI.
// Unknown URIs surface as JSON-RPC -32602 InvalidParams; read errors
// from the resource itself bubble up as -32603 Internal so MCP clients
// distinguish "you asked for X that doesn't exist" from "X failed to
// produce content".
func (s *Server) handleResourcesRead(req *jsonrpc.Message) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.replyError(req, errInvalidParams, fmt.Sprintf("bad resources/read params: %v", err))
		return
	}
	res, ok := s.resources[p.URI]
	if !ok {
		s.replyError(req, errInvalidParams, fmt.Sprintf("unknown resource: %s", p.URI))
		return
	}
	text, err := res.Read(s)
	if err != nil {
		s.replyError(req, errInternal, err.Error())
		return
	}
	s.reply(req, map[string]any{
		"contents": []resourceContent{{
			URI:      res.URI,
			MimeType: res.MimeType,
			Text:     text,
		}},
	})
}

// handleToolsCall dispatches to the requested tool's handler. Tool
// errors come back as `{isError: true}` content rather than JSON-RPC
// errors — that's the MCP convention so the LLM agent sees the message
// as tool output, not transport failure.
func (s *Server) handleToolsCall(req *jsonrpc.Message) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.replyError(req, errInvalidParams, fmt.Sprintf("bad tools/call params: %v", err))
		return
	}
	tool, ok := s.tools[p.Name]
	if !ok {
		s.replyError(req, errInvalidParams, fmt.Sprintf("unknown tool: %s", p.Name))
		return
	}
	content, isError, err := tool.Handler(s, p.Arguments)
	if err != nil {
		content = []Content{{Type: "text", Text: err.Error()}}
		isError = true
	}
	s.reply(req, map[string]any{
		"content": content,
		"isError": isError,
	})
}

func (s *Server) setIndex(idx *symbols.Index) {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	s.index = idx
}

func (s *Server) getIndex() *symbols.Index {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	return s.index
}

func (s *Server) setDerivRoots(roots []bindings.DerivRoot) {
	m := make(map[string][]bindings.DerivRoot, len(roots))
	for _, r := range roots {
		m[r.Name] = append(m[r.Name], r)
	}
	s.derivMu.Lock()
	s.derivRoots = m
	s.derivMu.Unlock()
}

// getDerivRoots returns the @derived sources registered for name (nil if name is
// not a derivation root). More than one root = fan-in (ambiguous source).
func (s *Server) getDerivRoots(name string) []bindings.DerivRoot {
	s.derivMu.RLock()
	defer s.derivMu.RUnlock()
	return s.derivRoots[name]
}

// getRoot returns the current workspace root.
func (s *Server) getRoot() string {
	s.rootMu.RLock()
	defer s.rootMu.RUnlock()
	return s.root
}

func (s *Server) reply(req *jsonrpc.Message, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		s.replyError(req, errInternal, err.Error())
		return
	}
	s.send(&jsonrpc.Message{JSONRPC: "2.0", ID: req.ID, Result: raw})
}

func (s *Server) replyError(req *jsonrpc.Message, code int, msg string) {
	s.send(&jsonrpc.Message{JSONRPC: "2.0", ID: req.ID, Error: &jsonrpc.Error{Code: code, Message: msg}})
}

func (s *Server) send(m *jsonrpc.Message) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.enc.Encode(m); err != nil {
		log.Printf("write: %v", err)
	}
}

// maybeLoadCache pulls a previously-saved cache off disk when
// persistence is configured. Missing files are silently OK (first
// run). Version mismatches and decode errors are logged then ignored
// — the in-memory cache stays empty and rebuilds from scratch, which
// is always correct.
func (s *Server) maybeLoadCache() {
	if s.cachePath == "" {
		return
	}
	f, err := os.Open(s.cachePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("mcp: open cache %s: %v", s.cachePath, err)
		}
		return
	}
	defer f.Close()
	if err := s.parseCache.Load(f); err != nil {
		log.Printf("mcp: load cache %s: %v (continuing with empty cache)", s.cachePath, err)
		return
	}
	log.Printf("mcp: loaded %d entries from %s", s.parseCache.Len(), s.cachePath)
}

// maybeSaveCache writes the current cache back to disk when
// persistence is configured. Uses temp-file + Rename so a crashed
// process doesn't leave a half-written cache. Errors are logged but
// not surfaced — failing to save a cache is never a reason to fail
// the rest of the shutdown.
func (s *Server) maybeSaveCache() {
	if s.cachePath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.cachePath), 0o755); err != nil {
		log.Printf("mcp: mkdir cache dir: %v", err)
		return
	}
	tmp := s.cachePath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		log.Printf("mcp: create cache tmp: %v", err)
		return
	}
	if err := s.parseCache.Save(f); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		log.Printf("mcp: save cache: %v", err)
		return
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		log.Printf("mcp: close cache tmp: %v", err)
		return
	}
	if err := os.Rename(tmp, s.cachePath); err != nil {
		_ = os.Remove(tmp)
		log.Printf("mcp: rename cache: %v", err)
		return
	}
	log.Printf("mcp: saved %d entries to %s", s.parseCache.Len(), s.cachePath)
}
