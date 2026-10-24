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
)

// protocolVersion mirrors the mcp package's advertised MCP revision. The
// proxy answers the handshake locally, so it must claim the same version
// a direct server would.
const protocolVersion = "2024-11-05"

// openWait bounds how long a tool call waits for its root's bring-up. The
// daemon bounds the bring-up by the index budget (30 s by default); this
// is the backstop for a daemon that stops answering.
const openWait = 10 * time.Minute

// proxy is one `mcp --daemon` process: the stdio MCP surface of a single
// root, served by the daemon.
type proxy struct {
	connect func() (*Client, error)
	root    string
	opts    OpenOptions

	writeMu sync.Mutex
	enc     *json.Encoder

	// connected is closed when connect returned: client, or connErr.
	connected chan struct{}
	client    *Client
	connErr   error
	// opened is closed when the root's /open returned; openErr is a
	// transport failure of it (a failed bring-up is not one: the daemon
	// answers tool calls with its post-mortem).
	opened  chan struct{}
	openErr error

	ctx    context.Context
	cancel context.CancelFunc
}

// Connect is how a proxy reaches its daemon: EnsureRunning, in production.
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
// client as a stdio server would write them. shutdown is local — the
// daemon keeps the root warm for other clients.
func RunProxy(connect Connect, root string, opts OpenOptions, in io.Reader, out io.Writer) error {
	p := &proxy{connect: connect, root: root, opts: opts, enc: json.NewEncoder(out),
		connected: make(chan struct{}), opened: make(chan struct{})}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	defer p.cancel()
	return p.serve(in)
}

// waitClient blocks until the daemon was reached, bounded.
func (p *proxy) waitClient() (*Client, error) {
	select {
	case <-p.connected:
		return p.client, p.connErr
	case <-time.After(openWait):
		return nil, fmt.Errorf("the daemon was not reached within %s", openWait)
	}
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

// start opens the root and the event stream, behind the initialize reply.
func (p *proxy) start() {
	go func() {
		defer close(p.opened)
		p.client, p.connErr = p.connect()
		close(p.connected)
		if p.connErr != nil {
			p.openErr = p.connErr
			log.Printf("proxy: daemon: %v", p.connErr)
			return
		}
		log.Printf("proxy: root %s served by daemon %s", p.root, p.client.socket)
		p.startEvents()
		go p.handshake(p.client)
		res, err := p.client.OpenWith(p.root, p.opts)
		if err != nil {
			p.openErr = err
			log.Printf("proxy: open root %s: %v", p.root, err)
			return
		}
		if res.Error != "" {
			log.Printf("proxy: root %s did not come up (tool calls answer why)", p.root)
			return
		}
		log.Printf("proxy: root %s warm (%d names)", p.root, res.Names)
	}()
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

// startEvents follows the daemon's event stream. It binds this proxy's
// lifetime to the daemon: when the proxy exits, the connection drops and
// the daemon rolls back any batch this session left open.
func (p *proxy) startEvents() {
	go func() {
		err := p.client.Events(p.ctx, func(ev Event) { p.notify(ev.Method, ev.Params) })
		if err != nil && p.ctx.Err() == nil {
			log.Printf("proxy: event stream ended: %v", err)
		}
	}()
}

// waitOpen blocks until the root's /open returned, bounded.
func (p *proxy) waitOpen() error {
	select {
	case <-p.opened:
		return p.openErr
	case <-time.After(openWait):
		return fmt.Errorf("the daemon did not finish opening %s within %s", p.root, openWait)
	}
}

func (p *proxy) serve(in io.Reader) error {
	dec := json.NewDecoder(in)
	var shutdown, started bool
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
			if !started {
				started = true
				p.start()
			}
			p.reply(&msg, map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities": map[string]any{
					"tools": map[string]any{},
					// The daemon's notifications (index, setup, daemon)
					// reach the client as notifications/message.
					"logging": map[string]any{},
				},
				"serverInfo": map[string]any{"name": "poly-lsp-mcp", "version": "0.0.0"}, // as the stdio server; BuildID hashes the binary
				// As an async stdio server: the report arrives in the
				// notification that ends the bring-up.
				"index": map[string]any{"state": "building", "notification": indexLogger},
			})
		case "notifications/initialized":
			// no-op
		case "tools/list":
			c, err := p.waitClient()
			if err != nil {
				p.replyErr(&msg, -32603, "poly-lsp: daemon: "+err.Error())
				continue
			}
			tools, err := c.Tools(p.root)
			if err != nil {
				p.replyErr(&msg, -32603, err.Error())
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
			if err := p.waitOpen(); err != nil {
				p.reply(&msg, toolError("poly-lsp: the daemon could not open the workspace: "+err.Error()))
				continue
			}
			content, isError, err := p.client.Call(p.root, params.Name, params.Arguments)
			if err != nil {
				p.replyErr(&msg, -32603, err.Error())
				continue
			}
			p.reply(&msg, map[string]any{"content": content, "isError": isError})
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
