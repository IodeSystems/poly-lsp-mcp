package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/iodesystems/tslsmcp/internal/config"
	"github.com/iodesystems/tslsmcp/internal/jsonrpc"
	"github.com/iodesystems/tslsmcp/internal/multiplex"
	"github.com/iodesystems/tslsmcp/internal/symbols"
)

// LSP / JSON-RPC error codes we surface explicitly. Values match the
// LSP 3.17 spec and the JSON-RPC 2.0 base spec it derives from.
const (
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternal       = -32603
	errServerNotInit  = -32002
)

// ErrExitWithoutShutdown is returned from Serve when the client sent an
// exit notification before a shutdown request. The spec requires the
// process to terminate with a non-zero exit code in this case; main.go
// surfaces that via log.Fatal.
var ErrExitWithoutShutdown = errors.New("server: exit notification received before shutdown")

// Server implements the LSP base protocol over an io.Reader/io.Writer
// pair. It owns the JSON-RPC loop, dispatches to typed handlers in
// lsp.go, holds the cross-language symbol index, and (when supplied)
// delegates per-file LSP semantics to child LSPs via *multiplex.Manager.
//
// New takes a Manager so callers can decide whether to enable multiplex
// (production main.go always does; tests opt out by passing nil to keep
// child-spawn latency out of the suite).
type Server struct {
	registry *config.Registry
	manager  *multiplex.Manager
	bindings []config.Binding
	schemas  []config.Schema

	writeMu sync.Mutex
	out     io.Writer

	stateMu     sync.Mutex
	gotInit     bool // true once we've replied to the initialize request
	initialized bool // true once the client sent the initialized notification
	shutdown    bool // true once the client requested shutdown

	indexMu sync.RWMutex
	index   *symbols.Index
}

// New constructs a Server. manager may be nil to skip child-LSP
// spawning (useful for tests that don't want gopls/tsserver startup
// latency); declared may be nil for workspaces without explicit
// cross-language bindings; schemas may be nil for workspaces with no
// Tier-3 schema-anchored bindings.
func New(reg *config.Registry, manager *multiplex.Manager, declared []config.Binding, schemas []config.Schema) *Server {
	return &Server{registry: reg, manager: manager, bindings: declared, schemas: schemas}
}

func (s *Server) Serve(in io.Reader, out io.Writer) error {
	s.out = out
	r := bufio.NewReader(in)
	for {
		msg, err := jsonrpc.Read(r)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		s.dispatch(msg)
		if msg.Method == "exit" {
			s.stateMu.Lock()
			clean := s.shutdown
			s.stateMu.Unlock()
			if !clean {
				return ErrExitWithoutShutdown
			}
			return nil
		}
	}
}

// dispatch enforces the LSP lifecycle and JSON-RPC base-protocol rules
// before forwarding to the per-method handlers below. The ordering here
// matters: jsonrpc version → initialize gate → double-init check →
// shutdown gate → method routing.
func (s *Server) dispatch(req *jsonrpc.Message) {
	// JSON-RPC 2.0 requires every message to declare jsonrpc:"2.0".
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

	// Pre-initialize: only initialize and exit are allowed. Per LSP spec
	// other requests get -32002 and other notifications are dropped.
	if !gotInit && req.Method != "initialize" && req.Method != "exit" {
		if !req.IsNotification() {
			s.replyError(req, errServerNotInit, "server not initialized")
		}
		return
	}

	// Re-initialize is an error.
	if gotInit && req.Method == "initialize" {
		s.replyError(req, errInvalidRequest, "server already initialized")
		return
	}

	// Post-shutdown: only exit is allowed. Any other request returns
	// InvalidRequest; notifications are dropped.
	if shutdown && req.Method != "exit" {
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
	case "initialized":
		s.stateMu.Lock()
		s.initialized = true
		s.stateMu.Unlock()
		log.Print("client initialized")
	case "shutdown":
		s.stateMu.Lock()
		s.shutdown = true
		s.stateMu.Unlock()
		s.reply(req, json.RawMessage("null"))
		s.stopManager()
	case "exit":
		// Loop in Serve closes us out.
	case "workspace/symbol":
		s.handleWorkspaceSymbol(req)
	case "textDocument/references":
		s.handleReferences(req)
	case "textDocument/documentSymbol":
		s.handleDocumentSymbol(req)
	case "textDocument/rename":
		s.handleRename(req)
	case "textDocument/didSave":
		s.handleDidSave(req)
	case "workspace/didChangeConfiguration", "workspace/didChangeWorkspaceFolders":
		// Workspace-scoped notifications get fanned out to every running
		// child so per-language servers can react. The parent server has
		// no state to update for these yet (single-root design).
		s.broadcastNotification(req)
	default:
		// Generic textDocument/* forwarding: anything we don't intercept
		// goes to the child LSP for the URI's language. Server-owned
		// methods (above) win.
		if strings.HasPrefix(req.Method, "textDocument/") {
			s.forwardTextDocument(req)
			return
		}
		if req.IsNotification() {
			return
		}
		s.replyError(req, errMethodNotFound, fmt.Sprintf("method not found: %s", req.Method))
	}
}

// broadcastNotification fans a workspace-scoped notification out to
// every running child via the manager. Silently no-ops when the
// manager is absent (single-server-only setups, tests).
func (s *Server) broadcastNotification(req *jsonrpc.Message) {
	if s.manager == nil {
		return
	}
	var params any
	if len(req.Params) > 0 {
		params = json.RawMessage(req.Params)
	}
	s.manager.Broadcast(req.Method, params)
}

// stopManager drains the manager (best-effort) and clears the pointer.
// Called once after shutdown reply so the exit notification can complete
// cleanly without children still running.
func (s *Server) stopManager() {
	if s.manager == nil {
		return
	}
	m := s.manager
	s.manager = nil
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = m.Shutdown(ctx)
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
	if err := jsonrpc.Write(s.out, m); err != nil {
		log.Printf("write: %v", err)
	}
}
