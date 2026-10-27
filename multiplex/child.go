// Package multiplex spawns and talks to child LSP processes on behalf of
// the poly-lsp-mcp server. Each Child owns one subprocess plus its I/O loop;
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
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/jsonrpc"
	"github.com/iodesystems/poly-lsp-mcp/internal/lspbin"
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

	// progress holds the work-done progress tokens the child has begun
	// and not yet ended ($/progress). A child with one open is still
	// loading or indexing: sourcekit-lsp publishes a file's diagnostics
	// without its semantic errors while "SourceKit-LSP: Reloading
	// Package" is open. See Busy.
	progressMu sync.Mutex
	progress   map[string]struct{}
	idle       chan struct{} // closed while progress is empty; see Idle

	done chan struct{}
	err  atomic.Value // error

	// exec.Cmd.Wait must run exactly once; both the manager's watchdog
	// and Shutdown reap, so the result is memoized.
	waitOnce sync.Once
	waitErr  error
}

// Spawn starts the child process and begins draining its stdout/stderr.
// cwd becomes the child's working directory (gopls/pylsp/etc. discover
// project roots from cwd). On success the returned *Child is alive and
// ready for Initialize.
func Spawn(name string, lsp *config.LSP, cwd string) (*Child, error) {
	if lsp == nil {
		return nil, errors.New("multiplex: nil LSP config")
	}
	found, err := lspbin.Resolve(lsp.Cmd)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", lsp.Cmd, err)
	}
	cmd := exec.Command(found.Path, lsp.Args...)
	cmd.Dir = cwd
	switch {
	case len(lsp.Env) > 0 && found.Env != nil:
		// found.Env leads PATH with the interpreter's dir; lsp.Env, later,
		// wins on any variable it names, PATH included.
		cmd.Env = append(found.Env, lsp.Env...)
	case len(lsp.Env) > 0:
		cmd.Env = append(os.Environ(), lsp.Env...)
	case found.Env != nil:
		cmd.Env = found.Env
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
	traceChild(c.name, "<-", msg.Method, msg.Params)
	if msg.IsNotification() {
		if msg.Method == "$/progress" {
			c.trackProgress(msg.Params)
		}
		c.notifyMu.Lock()
		fn := c.notifyFn
		c.notifyMu.Unlock()
		if fn != nil {
			fn(msg.Method, msg.Params)
		}
		return
	}
	if msg.Method == "window/workDoneProgress/create" {
		// Accepted so the child reports progress (Busy).
		_ = c.send(&jsonrpc.Message{JSONRPC: "2.0", ID: msg.ID, Result: json.RawMessage("null")})
		return
	}
	// Any other server→client request: not supported; method-not-found keeps
	// the child happy without blocking it on a response we'll never give.
	_ = c.send(&jsonrpc.Message{
		JSONRPC: "2.0",
		ID:      msg.ID,
		Error:   &jsonrpc.Error{Code: -32601, Message: "client does not implement " + msg.Method},
	})
}

// trackProgress records a $/progress begin or end. Report and
// partial-result values carry no begin/end and change nothing.
func (c *Child) trackProgress(params json.RawMessage) {
	var p struct {
		Token json.RawMessage `json:"token"`
		Value struct {
			Kind string `json:"kind"`
		} `json:"value"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	token := string(p.Token)
	c.progressMu.Lock()
	defer c.progressMu.Unlock()
	switch p.Value.Kind {
	case "begin":
		if c.progress == nil {
			c.progress = map[string]struct{}{}
		}
		if len(c.progress) == 0 {
			c.idle = make(chan struct{})
		}
		c.progress[token] = struct{}{}
	case "end":
		if _, open := c.progress[token]; !open {
			return
		}
		delete(c.progress, token)
		if len(c.progress) == 0 && c.idle != nil {
			close(c.idle)
		}
	}
}

// closedChan is returned by Idle for a child with no progress open.
var closedChan = func() chan struct{} { ch := make(chan struct{}); close(ch); return ch }()

// Idle returns a channel closed once the child has no work-done progress
// open: already closed when it has none now.
func (c *Child) Idle() <-chan struct{} {
	c.progressMu.Lock()
	defer c.progressMu.Unlock()
	if len(c.progress) == 0 {
		return closedChan
	}
	return c.idle
}

// Busy reports whether the child has a work-done progress open: it is
// loading its workspace or indexing, and what it publishes now may lack
// results it will publish later. Messages are handled in order, so a
// publish's notification handler sees the progress state as of that
// publish.
func (c *Child) Busy() bool {
	c.progressMu.Lock()
	defer c.progressMu.Unlock()
	return len(c.progress) > 0
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
	traceChild(c.name, "->", method, rawParams)
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
		"processId": os.Getpid(),
		"rootUri":   rootURI,
		// workDoneProgress: the child reports loading and indexing as
		// $/progress, which Busy reads.
		"capabilities": map[string]any{"window": map[string]any{"workDoneProgress": true}},
		"clientInfo": map[string]any{
			"name":    "poly-lsp-mcp",
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
// exited, reaping it. Returns the process's exit error if any. Safe to
// call more than once and from several goroutines.
func (c *Child) Wait() error {
	<-c.done
	c.waitOnce.Do(func() { c.waitErr = c.cmd.Wait() })
	return c.waitErr
}

// Reap waits up to grace for the process to exit on its own, then kills
// it, and always reaps it. Done (stdout EOF) can fire before the process
// has exited, so killing right after Done would cut a clean exit short.
func (c *Child) Reap(grace time.Duration) error {
	exited := make(chan error, 1)
	go func() { exited <- c.Wait() }()
	select {
	case err := <-exited:
		return err
	case <-time.After(grace):
		_ = c.Kill()
		return <-exited
	}
}

// Pid returns the OS process id, or 0 if the process never started.
func (c *Child) Pid() int {
	if c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
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

// TraceEnv, set to 1, logs each notification and request a child sends
// and each notification sent to it, params cut to traceMax bytes: the
// publish/progress sequence an edit's validation saw.
const TraceEnv = "POLY_LSP_CHILD_TRACE"

const traceMax = 300

func traceChild(name, dir, method string, params json.RawMessage) {
	if os.Getenv(TraceEnv) != "1" {
		return
	}
	if len(params) > traceMax {
		params = params[:traceMax]
	}
	log.Printf("[%s trace] %s %s %s", name, dir, method, params)
}
