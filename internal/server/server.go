package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/iodesystems/tslsmcp/internal/config"
	"github.com/iodesystems/tslsmcp/internal/jsonrpc"
	"github.com/iodesystems/tslsmcp/internal/symbols"
)

// Server implements the LSP base protocol over an io.Reader/io.Writer
// pair. It owns the JSON-RPC loop, dispatches to typed handlers in
// lsp.go, and holds the cross-language symbol index.
//
// Capabilities currently advertised: workspace/symbol,
// textDocument/references, textDocument/didSave. The multiplex layer
// (internal/multiplex) will fan in child-LSP capabilities on top once
// that lands.
type Server struct {
	registry *config.Registry

	writeMu sync.Mutex
	out     io.Writer

	stateMu     sync.Mutex
	initialized bool
	shutdown    bool

	indexMu sync.RWMutex
	index   *symbols.Index
}

func New(reg *config.Registry) *Server {
	return &Server{registry: reg}
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
	case "exit":
		// Loop in Serve closes us out.
	case "workspace/symbol":
		s.handleWorkspaceSymbol(req)
	case "textDocument/references":
		s.handleReferences(req)
	case "textDocument/didSave":
		s.handleDidSave(req)
	default:
		if req.IsNotification() {
			return
		}
		s.replyError(req, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
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
