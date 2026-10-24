package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/internal/jsonrpc"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
)

// protocolVersion mirrors the mcp package's advertised MCP revision. The
// proxy answers the handshake locally, so it must claim the same version
// a direct server would.
const protocolVersion = "2024-11-05"

// Waits, variables so tests can shorten them.
var (
	// openWait bounds how long a tool call waits for its root's bring-up.
	// The daemon bounds the bring-up by the index budget (30 s by
	// default); this is the backstop for a daemon that stops answering.
	openWait = 10 * time.Minute
	// reconnectWait bounds how long a lost connection is retried before
	// the proxy gives up on the daemon.
	reconnectWait = 60 * time.Second
)

// proxy is one `mcp --daemon` process: the stdio MCP surface of a single
// root, served by the daemon.
type proxy struct {
	connect Connect
	root    string
	opts    OpenOptions

	writeMu sync.Mutex
	enc     *json.Encoder

	mu  sync.Mutex
	cur *link

	ctx    context.Context
	cancel context.CancelFunc
}

// link is one connection to a daemon: a session, its event stream, its
// open of the root. A daemon upgrade ends it (the old daemon closes the
// stream), and the proxy makes a new one to whichever daemon serves the
// socket now.
type link struct {
	// connected is closed when client (or err) is set.
	connected chan struct{}
	client    *Client
	err       error
	// opened is closed when the root's /open returned; openErr is a
	// transport failure of it (a failed bring-up is not one: the daemon
	// answers tool calls with its post-mortem).
	opened  chan struct{}
	openErr error

	lost sync.Once
}

// Connect is how a proxy reaches its daemon: EnsureRunning, in production.
// Each call returns a new Client: a new session.
type Connect func() (*Client, error)

// RunProxy is the thin client: it speaks the stdio MCP JSON-RPC surface
// to an editor/agent exactly as a direct `mcp.Server` would, but forwards
// tools/list and tools/call to the daemon over the socket. The workspace
// index, child LSPs, watcher, and parse cache all live once in the
// daemon, shared across every client on the same root. initialize replies
// at once, as an async stdio server does: reaching the daemon (starting it
// if need be) and opening the root with the client's index options run
// behind it, and a call waits for what it needs. The daemon's
// notifications for the root (index, setup, conflicts) are written to the
// client as a stdio server would write them. When the daemon goes away —
// an upgrade hands over to a new one — the proxy reconnects, and a
// read-only call cut by it is sent again once. shutdown is local — the
// daemon keeps the root warm for other clients.
func RunProxy(connect Connect, root string, opts OpenOptions, in io.Reader, out io.Writer) error {
	p := &proxy{connect: connect, root: root, opts: opts, enc: json.NewEncoder(out)}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	defer p.cancel()
	return p.serve(in)
}

func (p *proxy) send(m *jsonrpc.Message) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := p.enc.Encode(m); err != nil {
		log.Printf("proxy write: %v", err)
	}
}

func (p *proxy) reply(req *jsonrpc.Message, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		p.replyErr(req, -32603, err.Error())
		return
	}
	p.send(&jsonrpc.Message{JSONRPC: "2.0", ID: req.ID, Result: raw})
}

func (p *proxy) replyErr(req *jsonrpc.Message, code int, msg string) {
	p.send(&jsonrpc.Message{JSONRPC: "2.0", ID: req.ID, Error: &jsonrpc.Error{Code: code, Message: msg}})
}

// notify writes a notification to the client.
func (p *proxy) notify(method string, params json.RawMessage) {
	p.send(&jsonrpc.Message{JSONRPC: "2.0", Method: method, Params: params})
}

func (p *proxy) current() *link {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur
}

// relink starts a new link and makes it current. reconnect: the daemon
// was there and went away, so connecting is retried (bounded) instead of
// failing at once.
func (p *proxy) relink(reconnect bool) *link {
	l := &link{connected: make(chan struct{}), opened: make(chan struct{})}
	p.mu.Lock()
	p.cur = l
	p.mu.Unlock()
	go p.establish(l, reconnect)
	return l
}

// lose marks l as gone and, if it is still current, starts the next one.
// Once per link: the event stream ending and a call failing on it are the
// same loss.
func (p *proxy) lose(l *link) {
	l.lost.Do(func() {
		if p.ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		current := p.cur == l
		p.mu.Unlock()
		if current {
			log.Printf("proxy: lost the daemon; reconnecting")
			p.relink(true)
		}
	})
}

// establish reaches the daemon, follows its events, says hello and opens
// the root, on l.
func (p *proxy) establish(l *link, reconnect bool) {
	defer close(l.opened)
	deadline := time.Now().Add(reconnectWait)
	backoff := 50 * time.Millisecond
	for {
		c, err := p.connect()
		if err == nil {
			l.client = c
			break
		}
		if !reconnect || time.Now().After(deadline) || p.ctx.Err() != nil {
			l.err, l.openErr = err, err
			close(l.connected)
			log.Printf("proxy: daemon: %v", err)
			return
		}
		time.Sleep(backoff)
		backoff = min(2*backoff, time.Second)
	}
	close(l.connected)
	log.Printf("proxy: root %s served by daemon %s", p.root, l.client.socket)
	p.followEvents(l)
	go p.handshake(l.client)
	res, err := l.client.OpenWith(p.root, p.opts)
	if err != nil {
		l.openErr = err
		log.Printf("proxy: open root %s: %v", p.root, err)
		if isTransport(err) {
			p.lose(l)
		}
		return
	}
	if res.Error != "" {
		log.Printf("proxy: root %s did not come up (tool calls answer why)", p.root)
		return
	}
	log.Printf("proxy: root %s warm (%d names)", p.root, res.Names)
}

// asked holds the build pairs this process asked an upgrade for: once per
// pair, never on every reconnect.
var (
	askedMu sync.Mutex
	asked   = map[string]bool{}
)

func once(key string) bool {
	askedMu.Lock()
	defer askedMu.Unlock()
	if asked[key] {
		return false
	}
	asked[key] = true
	return true
}

// handshake compares this proxy's build with the daemon's and, when this
// proxy's binary is the newer build in place, asks the daemon to move to
// it: dun's buildDecision. It runs beside the open; a call is served by
// the daemon as it is meanwhile.
func (p *proxy) handshake(c *Client) {
	hi, err := c.Hello()
	if err != nil {
		log.Printf("proxy: daemon handshake: %v (a daemon from before build identities: poly-lsp-mcp daemon --restart)", err)
		return
	}
	mine := BuildID()
	if hi.Build == mine {
		return
	}
	exe, _ := selfExe()
	onDisk := mine
	if h, err := HashFile(exe); err != nil || h != ExeHash() {
		onDisk = ProbeBuild(exe)
	}
	switch buildDecision(mine, hi.Build, onDisk) {
	case proxyIsStale:
		log.Printf("proxy: this binary was replaced by the daemon's build %s; restart this client to run it", hi.Build)
	case buildsDiffer:
		if once("differ " + mine + " " + hi.Build) {
			log.Printf("proxy: three builds: this proxy %s, its file now %s, the daemon %s; not asking for an upgrade", mine, onDisk, hi.Build)
		}
	case upgradeDaemon:
		if !once(hi.Build + " → " + mine) {
			return
		}
		if VCSTime() != "" && hi.VCSTime != "" && VCSTime() < hi.VCSTime {
			log.Printf("proxy: the daemon's build %s is a newer commit than this proxy's %s; not asking it to move back", hi.Build, mine)
			return
		}
		a, err := c.RequestUpgrade(UpgradeRequest{Exe: exe, Build: mine, Why: "a client of build " + mine + " connected", Time: VCSTime()})
		switch {
		case err != nil:
			log.Printf("proxy: upgrade request: %v", err)
		case a.Accepted:
			log.Printf("proxy: the daemon (%s) is upgrading to this build (%s)", hi.Build, mine)
		default:
			log.Printf("proxy: the daemon (%s) will not upgrade to %s: %s", hi.Build, mine, a.Reason)
		}
	}
}

// followEvents follows the daemon's event stream on l. It binds this
// session's lifetime to the daemon: when the proxy exits, the connection
// drops and the daemon rolls back any batch this session left open. When
// the daemon ends it (a hand-over) or the connection drops, the link is
// lost and the proxy reconnects.
func (p *proxy) followEvents(l *link) {
	go func() {
		err := l.client.Events(p.ctx, func(ev Event) { p.notify(ev.Method, ev.Params) })
		if p.ctx.Err() != nil {
			return
		}
		if err != nil && !isTransport(err) {
			// The daemon refused the stream (one too old to have it):
			// nothing a reconnect would change.
			log.Printf("proxy: event stream: %v", err)
			return
		}
		p.lose(l)
	}()
}

// ready waits, bounded, for l to have opened the root.
func (l *link) ready(d time.Duration) error {
	select {
	case <-l.opened:
		return l.openErr
	case <-time.After(d):
		return fmt.Errorf("the daemon did not answer within %s", d)
	}
}

// client waits, bounded, for l to have reached the daemon.
func (l *link) reached(d time.Duration) (*Client, error) {
	select {
	case <-l.connected:
		return l.client, l.err
	case <-time.After(d):
		return nil, fmt.Errorf("the daemon was not reached within %s", d)
	}
}

// editCut is a mutating call's answer when the connection to the daemon
// dropped while it was in flight. It is never sent again blindly: it may
// have been applied.
const editCut = "poly-lsp: the daemon was upgrading (or restarting) while this %s was in flight, and the connection dropped. " +
	"It may or may not have been applied: re-read the node, then retry. (%v)"

// call runs a tool on the current link. A transport failure loses the
// link; a read-only call is then sent once more on the next one, which
// waits (bounded) for the daemon that serves the socket now.
func (p *proxy) call(name string, args json.RawMessage) map[string]any {
	l := p.current()
	if err := l.ready(openWait); err != nil {
		return toolError("poly-lsp: the daemon could not open the workspace: " + err.Error())
	}
	content, isError, err := l.client.Call(p.root, name, args)
	if err != nil && isTransport(err) {
		p.lose(l)
		if mcp.IsMutatingTool(name) {
			return toolError(fmt.Sprintf(editCut, name, err))
		}
		next := p.current()
		if rerr := next.ready(reconnectWait + openWait); rerr != nil {
			return toolError("poly-lsp: lost the daemon and could not reconnect: " + rerr.Error())
		}
		content, isError, err = next.client.Call(p.root, name, args)
	}
	if err != nil {
		return toolError("poly-lsp: daemon: " + err.Error())
	}
	return map[string]any{"content": content, "isError": isError}
}

// tools lists the catalog, reconnecting once like a read-only call.
func (p *proxy) tools() ([]mcp.ToolDescriptor, error) {
	l := p.current()
	c, err := l.reached(openWait)
	if err != nil {
		return nil, err
	}
	tools, err := c.Tools(p.root)
	if err != nil && isTransport(err) {
		p.lose(l)
		if c, err = p.current().reached(reconnectWait + openWait); err != nil {
			return nil, err
		}
		tools, err = c.Tools(p.root)
	}
	return tools, err
}

func (p *proxy) serve(in io.Reader) error {
	dec := json.NewDecoder(in)
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
			if p.current() == nil {
				p.relink(false)
			}
			p.reply(&msg, map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities": map[string]any{
					"tools": map[string]any{},
					// The daemon's notifications (index, setup, daemon)
					// reach the client as notifications/message.
					"logging": map[string]any{},
				},
				// The stdio server's; BuildID would hash the binary first.
				"serverInfo": map[string]any{"name": "poly-lsp-mcp", "version": "0.0.0"},
				// As an async stdio server: the report arrives in the
				// notification that ends the bring-up.
				"index": map[string]any{"state": "building", "notification": indexLogger},
			})
		case "notifications/initialized":
			// no-op
		case "tools/list":
			if p.current() == nil {
				p.replyErr(&msg, -32002, "server not initialized")
				continue
			}
			tools, err := p.tools()
			if err != nil {
				p.replyErr(&msg, -32603, "poly-lsp: daemon: "+err.Error())
				continue
			}
			p.reply(&msg, map[string]any{"tools": tools})
		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(msg.Params, &params); err != nil {
				p.replyErr(&msg, -32602, fmt.Sprintf("bad tools/call params: %v", err))
				continue
			}
			if p.current() == nil {
				p.replyErr(&msg, -32002, "server not initialized")
				continue
			}
			p.reply(&msg, p.call(params.Name, params.Arguments))
		case "resources/list":
			p.reply(&msg, map[string]any{"resources": []any{}})
		case "shutdown":
			shutdown = true
			p.reply(&msg, json.RawMessage("null"))
		default:
			if msg.IsNotification() {
				continue
			}
			p.replyErr(&msg, -32601, "method not found: "+msg.Method)
		}
	}
}

// toolError is a tools/call result the agent reads as tool output.
func toolError(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": true}
}
