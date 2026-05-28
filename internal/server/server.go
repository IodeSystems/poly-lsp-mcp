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

	writeMu sync.Mutex
	out     io.Writer

	stateMu     sync.Mutex
	initialized bool
	shutdown    bool

	indexMu sync.RWMutex
	index   *symbols.Index
}

func New(reg *config.Registry, manager *multiplex.Manager) *Server {
	return &Server{registry: reg, manager: manager}
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
		s.stateMu.Lock()
		exiting := s.shutdown && msg.Method == "exit"
		s.stateMu.Unlock()
		if exiting {
			return nil
		}
	}
}

func (s *Server) dispatch(req *jsonrpc.Message) {
	switch req.Method {
	case "initialize":
		s.handleInitialize(req)
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
	case "textDocument/didSave":
		s.handleDidSave(req)
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
		s.replyError(req, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
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
		s.replyError(req, -32603, err.Error())
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
