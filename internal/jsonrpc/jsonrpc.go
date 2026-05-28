// Package jsonrpc implements the LSP base-protocol framing
// (Content-Length headers wrapping JSON-RPC 2.0 bodies).
//
// Both the server loop (internal/server) and the child-LSP supervisor
// (internal/multiplex) read and write messages through this package.
package jsonrpc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Message is a JSON-RPC 2.0 message. The same struct serves requests
// (Method + ID), notifications (Method, no ID), and responses (ID +
// Result/Error). Fields are RawMessage to defer decoding to handlers
// that know the method-specific shape.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// IsNotification reports whether the message lacks an id (per JSON-RPC 2.0).
func (m *Message) IsNotification() bool {
	return len(m.ID) == 0 || string(m.ID) == "null"
}

// IsResponse reports whether the message carries a Result or Error.
func (m *Message) IsResponse() bool {
	return m.Result != nil || m.Error != nil
}

// Read parses one Content-Length framed message from r.
func Read(r *bufio.Reader) (*Message, error) {
	var contentLength int
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("malformed header: %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("bad Content-Length: %w", err)
			}
			contentLength = n
		}
	}
	if contentLength <= 0 {
		return nil, fmt.Errorf("missing Content-Length")
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var m Message
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	return &m, nil
}

// Write encodes m and writes it with a Content-Length header.
func Write(w io.Writer, m *Message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}
