package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/iodesystems/poly-lsp-mcp/internal/jsonrpc"
)

// protocolVersion mirrors the mcp package's advertised MCP revision. The
// proxy answers the handshake locally, so it must claim the same version
// a direct server would.
const protocolVersion = "2024-11-05"

// RunProxy is the thin client: it speaks the stdio MCP JSON-RPC surface
// to an editor/agent exactly as a direct `mcp.Server` would, but forwards
// tools/list and tools/call to the daemon over the socket. The workspace
// index, child LSPs, watcher, and parse cache all live once in the
// daemon, shared across every client on the same root. initialize warms
// the root (and surfaces the trust-gate rejection early); shutdown is
// local — the daemon keeps the root warm for other clients.
func RunProxy(client *Client, root string, in io.Reader, out io.Writer) error {
	enc := json.NewEncoder(out)
	dec := json.NewDecoder(in)
	send := func(m *jsonrpc.Message) {
		if err := enc.Encode(m); err != nil {
			log.Printf("proxy write: %v", err)
		}
	}
	reply := func(req *jsonrpc.Message, result any) {
		raw, err := json.Marshal(result)
		if err != nil {
			send(&jsonrpc.Message{JSONRPC: "2.0", ID: req.ID, Error: &jsonrpc.Error{Code: -32603, Message: err.Error()}})
			return
		}
		send(&jsonrpc.Message{JSONRPC: "2.0", ID: req.ID, Result: raw})
	}
	replyErr := func(req *jsonrpc.Message, code int, msg string) {
		send(&jsonrpc.Message{JSONRPC: "2.0", ID: req.ID, Error: &jsonrpc.Error{Code: code, Message: msg}})
	}

	// watchStarted guards the single long-lived /session/watch request that
	// binds this proxy's lifetime to the daemon: when the proxy exits, that
	// connection drops and the daemon rolls back any batch this session left
	// open. Started on initialize (after the daemon is confirmed reachable).
	var watchStarted bool
	startWatch := func() {
		if watchStarted {
			return
		}
		watchStarted = true
		go func() {
			if err := client.WatchSession(context.Background()); err != nil {
				log.Printf("proxy: session watch ended: %v", err)
			}
		}()
	}

	var shutdown bool
	for {
		var msg jsonrpc.Message
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				if shutdown {
					return nil
				}
				return errors.New("proxy: stream closed before shutdown")
			}
			return fmt.Errorf("proxy read: %w", err)
		}

		switch msg.Method {
		case "initialize":
			// Warm the root now so the first tool call is fast and any
			// trust-gate rejection surfaces immediately.
			if n, err := client.Open(root); err != nil {
				log.Printf("proxy: open root %s: %v", root, err)
			} else {
				log.Printf("proxy: root %s warm (%d names)", root, n)
				startWatch()
			}
			reply(&msg, map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities": map[string]any{
					"tools":     map[string]any{},
					"resources": map[string]any{},
				},
				"serverInfo": map[string]any{"name": "poly-lsp-mcp", "version": Version},
			})
		case "notifications/initialized":
			// no-op
		case "tools/list":
			tools, err := client.Tools(root)
			if err != nil {
				replyErr(&msg, -32603, err.Error())
				continue
			}
			reply(&msg, map[string]any{"tools": tools})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				replyErr(&msg, -32602, fmt.Sprintf("bad tools/call params: %v", err))
				continue
			}
			content, isError, err := client.Call(root, p.Name, p.Arguments)
			if err != nil {
				replyErr(&msg, -32603, err.Error())
				continue
			}
			reply(&msg, map[string]any{"content": content, "isError": isError})
		case "resources/list":
			reply(&msg, map[string]any{"resources": []any{}})
		case "shutdown":
			shutdown = true
			reply(&msg, json.RawMessage("null"))
		default:
			if msg.IsNotification() {
				continue
			}
			replyErr(&msg, -32601, "method not found: "+msg.Method)
		}
	}
}
