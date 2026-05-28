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
)

// Server implements the LSP base protocol over an io.Reader/io.Writer pair.
// It is intentionally minimal: it owns the JSON-RPC loop and dispatches to
// handler methods. Capabilities are deliberately empty until the multiplex
// (internal/multiplex) and tree-sitter (internal/treesitter) layers come
// online — see plan/plan.md.
type Server struct {
	registry *config.Registry

	writeMu sync.Mutex
	out     io.Writer

	stateMu     sync.Mutex
	initialized bool
	shutdown    bool
}

func New(reg *config.Registry) *Server {
	return &Server{registry: reg}
}

func (s *Server) Serve(in io.Reader, out io.Writer) error {
	s.out = out
	r := bufio.NewReader(in)
	for {
		msg, err := readMessage(r)
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

func (s *Server) dispatch(req *Message) {
	switch req.Method {
	case "initialize":
		s.handleInitialize(req)
	case "initialized":
		// Notification: no response.
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
		// Handled by Serve loop.
	default:
		if req.IsNotification() {
			return
		}
		s.replyError(req, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func (s *Server) handleInitialize(req *Message) {
	// Empty capabilities for now. Multiplex layer will populate this from
	// the union of child-LSP capabilities.
	result := map[string]any{
		"capabilities": map[string]any{},
		"serverInfo": map[string]any{
			"name":    "tslsmcp",
			"version": "0.0.0",
		},
	}
	s.reply(req, result)
}

func (s *Server) reply(req *Message, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		s.replyError(req, -32603, err.Error())
		return
	}
	s.send(&Message{JSONRPC: "2.0", ID: req.ID, Result: raw})
}

func (s *Server) replyError(req *Message, code int, msg string) {
	s.send(&Message{JSONRPC: "2.0", ID: req.ID, Error: &Error{Code: code, Message: msg}})
}

func (s *Server) send(m *Message) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := writeMessage(s.out, m); err != nil {
		log.Printf("write: %v", err)
	}
}
