// Package mcp serves the Model Context Protocol layer of tslsmcp.
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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"

	"time"

	"github.com/iodesystems/tslsmcp/internal/bindings"
	"github.com/iodesystems/tslsmcp/internal/config"
	"github.com/iodesystems/tslsmcp/internal/jsonrpc"
	"github.com/iodesystems/tslsmcp/internal/multiplex"
	"github.com/iodesystems/tslsmcp/internal/symbols"
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

	tools     map[string]Tool
	resources map[string]Resource
}

const defaultDiagnosticWait = 1500 * time.Millisecond

// New constructs an MCP server bound to a workspace. The root is the
// directory whose files the symbol index will cover; bindings and
// schemas are applied (Tier 2 then Tier 3) once `initialize` arrives.
func New(reg *config.Registry, root string, declared []config.Binding, schemas []config.Schema) *Server {
	s := &Server{
		registry: reg,
		root:     root,
		bindings: declared,
		schemas:  schemas,
		openDocs: map[string]int32{},
	}
	s.tools = registerTools()
	s.resources = registerResources()
	s.parseCache = symbols.NewParseCache()
	return s
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
		s.reply(req, json.RawMessage("null"))
	default:
		if req.IsNotification() {
			return
		}
		s.replyError(req, errMethodNotFound, fmt.Sprintf("method not found: %s", req.Method))
	}
}

// handleInitialize builds the symbol index for s.getRoot(), applies any
// Tier-2 and Tier-3 bindings, and advertises tool capability.
func (s *Server) handleInitialize(req *jsonrpc.Message) {
	if s.getRoot() != "" {
		idx, err := symbols.Build(s.getRoot(), s.registry, symbols.WithCache(s.parseCache))
		if err != nil {
			log.Printf("mcp initialize: index build failed for %s: %v", s.getRoot(), err)
		} else {
			s.setIndex(idx)
			log.Printf("mcp initialize: indexed %d names from %s", len(idx.Names()), s.getRoot())
			if len(s.bindings) > 0 || len(s.schemas) > 0 {
				resolver := bindings.NewResolver(s.getRoot())
				if len(s.bindings) > 0 {
					n, err := resolver.Apply(idx, s.bindings)
					if err != nil {
						log.Printf("mcp initialize: some bindings failed validation: %v", err)
					}
					log.Printf("mcp initialize: applied %d declared binding site(s)", n)
				}
				if len(s.schemas) > 0 {
					n := resolver.ApplySchemas(idx, s.schemas)
					log.Printf("mcp initialize: applied %d schema-anchored site(s)", n)
				}
			}
			s.startManagerIfPresent(idx)
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
			"name":    "tslsmcp",
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
