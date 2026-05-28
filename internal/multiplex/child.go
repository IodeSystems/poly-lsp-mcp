// Package multiplex spawns and talks to child LSP processes on behalf of
// the tslsmcp server. Each Child owns one subprocess plus its I/O loop;
// the Manager (forthcoming) holds the map of language → Child and routes
// textDocument/* requests by URI.
//
// This file implements the per-child supervisor: process lifecycle, the
// JSON-RPC client over internal/jsonrpc, and the LSP initialize handshake.
package multiplex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/iodesystems/tslsmcp/internal/config"
	"github.com/iodesystems/tslsmcp/internal/jsonrpc"
)

// Child supervises a single child LSP process. After Spawn returns
// successfully the process is running, the read loop is active, and the
// caller may issue Call / Notify / Initialize / Shutdown. Done is closed
// when the read loop terminates (EOF or fatal decode error); Err returns
// the cause if non-clean.
type Child struct {
	name string // display name (the language registry's Name)
	cmd  *exec.Cmd
	in   io.WriteCloser

	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]chan *jsonrpc.Message
	nextID    int64

	notifyMu sync.Mutex
	notifyFn func(method string, params json.RawMessage)

	done chan struct{}
	err  atomic.Value // error
}

// Spawn starts the child process and begins draining its stdout/stderr.
// cwd becomes the child's working directory (gopls/pylsp/etc. discover
// project roots from cwd). On success the returned *Child is alive and
// ready for Initialize.
func Spawn(name string, lsp *config.LSP, cwd string) (*Child, error) {
	if lsp == nil {
		return nil, errors.New("multiplex: nil LSP config")
	}
	cmd := exec.Command(lsp.Cmd, lsp.Args...)
	cmd.Dir = cwd
	if len(lsp.Env) > 0 {
		cmd.Env = append(os.Environ(), lsp.Env...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", lsp.Cmd, err)
	}

	c := &Child{
		name:    name,
		cmd:     cmd,
		in:      stdin,
		pending: make(map[string]chan *jsonrpc.Message),
		done:    make(chan struct{}),
	}

	go c.readLoop(bufio.NewReader(stdout))
	go c.drainStderr(stderr)

	return c, nil
}

func (c *Child) drainStderr(r io.Reader) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		log.Printf("[%s] %s", c.name, s.Text())
	}
}

// readLoop reads framed messages until EOF or a fatal decode error.
// On exit, c.done is closed; outstanding Calls notice via <-c.done.
// Pending channels are not closed (they're buffered(1) and may still
// receive a late response without harm); leaving them open avoids a
// send-to-closed-chan race with the handle() goroutine.
func (c *Child) readLoop(r *bufio.Reader) {
	defer close(c.done)
	for {
		msg, err := jsonrpc.Read(r)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.err.Store(err)
			}
			return
		}
		c.handle(msg)
	}
}

func (c *Child) handle(msg *jsonrpc.Message) {
	if msg.IsResponse() {
		c.pendingMu.Lock()
		ch, ok := c.pending[string(msg.ID)]
		if ok {
			delete(c.pending, string(msg.ID))
		}
		c.pendingMu.Unlock()
		if ok {
			ch <- msg // buffered(1), non-blocking
		}
		return
	}
	if msg.Method == "" {
		return
	}
	if msg.IsNotification() {
		c.notifyMu.Lock()
		fn := c.notifyFn
		c.notifyMu.Unlock()
		if fn != nil {
			fn(msg.Method, msg.Params)
		}
		return
	}
	// Server→client request. Not supported yet; method-not-found keeps
	// the child happy without blocking it on a response we'll never give.
	_ = c.send(&jsonrpc.Message{
		JSONRPC: "2.0",
		ID:      msg.ID,
		Error:   &jsonrpc.Error{Code: -32601, Message: "client does not implement " + msg.Method},
	})
}

// SetNotificationHandler registers a callback for server-initiated
// notifications (publishDiagnostics, etc.). Call once during setup.
func (c *Child) SetNotificationHandler(fn func(method string, params json.RawMessage)) {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	c.notifyFn = fn
}

func (c *Child) send(m *jsonrpc.Message) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return jsonrpc.Write(c.in, m)
}

// Call sends a request and blocks until the response arrives, ctx is
// canceled, or the child exits. params=nil omits the params field on
// the wire (some LSPs reject explicit null for no-params methods like
// shutdown).
func (c *Child) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	rawID := json.RawMessage(strconv.AppendInt(nil, id, 10))

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal %s params: %w", method, err)
		}
		rawParams = b
	}

	ch := make(chan *jsonrpc.Message, 1)
	c.pendingMu.Lock()
	c.pending[string(rawID)] = ch
	c.pendingMu.Unlock()

	if err := c.send(&jsonrpc.Message{
		JSONRPC: "2.0",
		ID:      rawID,
		Method:  method,
		Params:  rawParams,
	}); err != nil {
		c.removePending(string(rawID))
		return nil, fmt.Errorf("write %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		c.removePending(string(rawID))
		return nil, ctx.Err()
	case <-c.done:
		c.removePending(string(rawID))
		if err := c.Err(); err != nil {
			return nil, fmt.Errorf("child %s exited: %w", c.name, err)
		}
		return nil, fmt.Errorf("child %s exited", c.name)
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

func (c *Child) removePending(id string) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	delete(c.pending, id)
}

// Notify sends a one-way notification. params=nil omits params on the wire.
func (c *Child) Notify(method string, params any) error {
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshal %s params: %w", method, err)
		}
		rawParams = b
	}
	return c.send(&jsonrpc.Message{
		JSONRPC: "2.0",
		Method:  method,
		Params:  rawParams,
	})
}

// Initialize performs the LSP handshake (initialize request + initialized
// notification) and returns the raw capabilities object reported by the
// child. The capabilities are kept as json.RawMessage so the multiplex
// layer can union them across children without losing fields.
func (c *Child) Initialize(ctx context.Context, rootURI string) (json.RawMessage, error) {
	params := map[string]any{
		"processId":    os.Getpid(),
		"rootUri":      rootURI,
		"capabilities": map[string]any{},
		"clientInfo": map[string]any{
			"name":    "tslsmcp",
			"version": "0.0.0",
		},
	}
	raw, err := c.Call(ctx, "initialize", params)
	if err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	var resp struct {
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode initialize response: %w", err)
	}
	if err := c.Notify("initialized", map[string]any{}); err != nil {
		return nil, fmt.Errorf("send initialized: %w", err)
	}
	return resp.Capabilities, nil
}

// Shutdown sends shutdown + exit. On error the caller should still call
// Kill + Wait to reap the process.
func (c *Child) Shutdown(ctx context.Context) error {
	if _, err := c.Call(ctx, "shutdown", nil); err != nil {
		return err
	}
	return c.Notify("exit", nil)
}

// Wait blocks until the read loop has terminated and the process has
// exited. Returns the process's exit error if any.
func (c *Child) Wait() error {
	<-c.done
	return c.cmd.Wait()
}

// Done returns a channel that is closed when the read loop terminates.
func (c *Child) Done() <-chan struct{} { return c.done }

// Err returns the last non-EOF read-loop error, or nil for clean exit
// or while the loop is still running.
func (c *Child) Err() error {
	if e, ok := c.err.Load().(error); ok {
		return e
	}
	return nil
}

// Kill forcibly terminates the child process. Safe to call multiple
// times; the second call returns the underlying os error and is
// usually ignored.
func (c *Child) Kill() error {
	if c.cmd.Process == nil {
		return nil
	}
	return c.cmd.Process.Kill()
}
