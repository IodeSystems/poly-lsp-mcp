package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"

	"github.com/iodesystems/poly-lsp-mcp/mcp"
)

// sessionHeader carries the client's session id so the daemon isolates its
// edit batch from other clients on the same root, and so /session/watch can
// bind the session's lifetime. readOnlyHeader / validateHeader carry the
// per-connection policy the daemon enforces at its boundary.
const (
	sessionHeader  = "X-Poly-Session"
	readOnlyHeader = "X-Poly-Read-Only"
	validateHeader = "X-Poly-Validate"
)

// Client dials a daemon over its unix socket. The URL host is irrelevant
// (the transport always dials the socket); "poly-lsp" is a placeholder.
// One Client per proxy process = one session; the id is minted once and
// sent on every request.
type Client struct {
	socket   string
	session  string
	readOnly bool
	validate bool
	hc       *http.Client
}

// SetPolicy declares this client's per-connection policy (from the proxy's
// --read-only / --validate flags). Sent as headers on every request; the
// daemon enforces it at its boundary and can only tighten, never loosen, its
// own baseline.
func (c *Client) SetPolicy(readOnly, validate bool) {
	c.readOnly = readOnly
	c.validate = validate
}

// setHeaders stamps the session + policy headers on a request.
func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set(sessionHeader, c.session)
	if c.readOnly {
		req.Header.Set(readOnlyHeader, "true")
	}
	if c.validate {
		req.Header.Set(validateHeader, "true")
	}
}

// NewClient builds a client for the given socket path with a fresh random
// session id.
func NewClient(socket string) *Client {
	return &Client{
		socket:  socket,
		session: newSessionID(),
		hc: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
		},
	}
}

// retarget is c dialing socket instead, the same session and policy. c
// itself when the socket is unchanged.
func (c *Client) retarget(socket string) *Client {
	if socket == c.socket {
		return c
	}
	n := NewClient(socket)
	n.session, n.readOnly, n.validate = c.session, c.readOnly, c.validate
	return n
}

// newSessionID returns a random hex token. crypto/rand failure is fatal to
// randomness, so fall back to a fixed marker rather than an empty id (which
// would collide with the daemon's implicit local session).
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "session-randfail"
	}
	return hex.EncodeToString(b[:])
}

// Session is the client's session id.
func (c *Client) Session() string { return c.session }

// WatchSession opens the long-lived /session/watch request and blocks until
// the daemon closes it. The proxy runs it in a goroutine for its whole
// lifetime; when the proxy process exits, the connection drops and the
// daemon auto-rolls-back this session's staged batch. Returns the daemon's
// error, if any (a lost daemon is not fatal to the proxy — the batch, if
// any, is already gone with the daemon).
func (c *Client) WatchSession(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://poly-lsp/session/watch", nil)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return &transportError{err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Healthy reports whether the daemon answers /health.
func (c *Client) Healthy() bool { return probeHealth(c.hc) }

// Health is the /health answer.
type Health struct {
	OK       bool     `json:"ok"`
	Build    string   `json:"build"`
	Protocol int      `json:"protocol"`
	PID      int      `json:"pid"`
	Roots    []string `json:"roots"`
	Warm     bool     `json:"warm"`
}

// takeover asks a warm takeover daemon to take the main socket.
func (c *Client) takeover(socket string) error {
	return c.postJSON("/takeover", map[string]string{"socket": socket}, nil)
}

// Hello is the version handshake: it says this client's build and returns
// the daemon's.
func (c *Client) Hello() (Hello, error) {
	exe, _ := selfExe()
	body := map[string]any{"build": BuildID(), "protocol": Protocol, "exe": exe, "vcsTime": VCSTime()}
	var out Hello
	err := c.postJSON("/hello", body, &out)
	return out, err
}

// RequestUpgrade asks the daemon to move to another binary.
func (c *Client) RequestUpgrade(req UpgradeRequest) (UpgradeAnswer, error) {
	var out UpgradeAnswer
	err := c.postJSON("/upgrade", req, &out)
	return out, err
}

func (c *Client) health() (Health, error) {
	var h Health
	err := c.get("/health", nil, &h)
	return h, err
}

// Open warms a root on the daemon and returns the indexed-name count.
func (c *Client) Open(root string) (int, error) {
	res, err := c.OpenWith(root, OpenOptions{})
	if err != nil {
		return 0, err
	}
	if res.Error != "" {
		return 0, fmt.Errorf("daemon: %s", res.Error)
	}
	return res.Names, nil
}

// OpenResult is the /open answer: the canonical root, its indexed-name
// count, and the failed bring-up's post-mortem ("" when it is up).
type OpenResult struct {
	Root  string `json:"root"`
	Names int    `json:"names"`
	Error string `json:"error"`
}

// OpenWith opens a root with the client's index options, holding it for
// this session. It returns when the root's bring-up has ended.
func (c *Client) OpenWith(root string, opts OpenOptions) (OpenResult, error) {
	body := struct {
		Root string `json:"root"`
		OpenOptions
	}{root, opts}
	var out OpenResult
	err := c.postJSON("/open", body, &out)
	return out, err
}

// Events holds /session/events open and calls fn with each notification
// until ctx ends or the daemon closes the stream. Like WatchSession, it
// binds the session's lifetime: when it drops, the daemon rolls back the
// session's batches and releases its holds.
func (c *Client) Events(ctx context.Context, fn func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://poly-lsp/session/events", nil)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return &transportError{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daemon: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			if err == io.EOF {
				return nil
			}
			return &transportError{err}
		}
		fn(ev)
	}
}

// transportError is a failure to reach the daemon or to read its answer,
// as opposed to an answer: the proxy reconnects on one, and retries a
// read-only call.
type transportError struct{ err error }

func (e *transportError) Error() string { return "daemon connection: " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

func isTransport(err error) bool {
	var te *transportError
	return errors.As(err, &te)
}

// Event is one notification from /session/events.
type Event struct {
	Root   string          `json:"root,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Tools returns the tool catalog for a root.
func (c *Client) Tools(root string) ([]mcp.ToolDescriptor, error) {
	var out struct {
		Tools []mcp.ToolDescriptor `json:"tools"`
	}
	if err := c.get("/tools", url.Values{"root": {root}}, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// Call invokes an MCP tool against a root and returns the tool result.
func (c *Client) Call(root, name string, arguments json.RawMessage) (content []mcp.Content, isError bool, err error) {
	if arguments == nil {
		arguments = json.RawMessage("{}")
	}
	var out struct {
		Content []mcp.Content `json:"content"`
		IsError bool          `json:"isError"`
	}
	body := map[string]any{"root": root, "name": name, "arguments": arguments}
	if err := c.postJSON("/call", body, &out); err != nil {
		return nil, false, err
	}
	return out.Content, out.IsError, nil
}

// FileAccess is the /access answer: the op and absolute path a read or
// edit call would hand the file-access hook; OK false when it would reach
// no hook.
type FileAccess struct {
	Op   string `json:"op,omitempty"`
	Path string `json:"path,omitempty"`
	OK   bool   `json:"ok"`
}

// Access asks which file a node_read/node_edit call would touch.
func (c *Client) Access(root, name string, arguments json.RawMessage) (FileAccess, error) {
	if arguments == nil {
		arguments = json.RawMessage("{}")
	}
	var out FileAccess
	err := c.postJSON("/access", map[string]any{"root": root, "name": name, "arguments": arguments}, &out)
	return out, err
}

// FileSymbols fragments file content into structural atoms on the daemon
// — the read-only surface a non-MCP consumer (raglit) hits to avoid a
// cgo tree-sitter build or a fork per file. language may be "" if path
// carries a known extension.
func (c *Client) FileSymbols(content, language, path string) (string, []FileSymbol, error) {
	var out struct {
		Language string       `json:"language"`
		Symbols  []FileSymbol `json:"symbols"`
	}
	body := map[string]string{"content": content, "language": language, "path": path}
	if err := c.postJSON("/filesymbols", body, &out); err != nil {
		return "", nil, err
	}
	return out.Language, out.Symbols, nil
}

// QueryParams is one /query request against a hosted root.
type QueryParams struct {
	Root     string
	Selector string
	Limit    int
	Offset   int
	Budget   string
	Include  []string // gitignore-style patterns opening left-out dirs
	JSON     bool
}

// Query evaluates a selector against a root on the daemon and returns the
// answer text — exactly what QueryText (or QueryJSON with JSON: true)
// writes. The daemon answer is LSP-resolved: the hosted root carries its
// own child-LSP manager, so edges settle with conf the way node_query does.
// A selector error comes back as the answer (the prose), not as an error;
// a genuine tool failure is an error.
func (c *Client) Query(p QueryParams) (string, error) {
	var out struct {
		Output string `json:"output"`
		Root   string `json:"root"`
	}
	body := map[string]any{
		"root":     p.Root,
		"selector": p.Selector,
		"limit":    p.Limit,
		"offset":   p.Offset,
		"budget":   p.Budget,
		"include":  p.Include,
		"json":     p.JSON,
	}
	if err := c.postJSON("/query", body, &out); err != nil {
		return "", err
	}
	return out.Output, nil
}

func (c *Client) get(path string, q url.Values, out any) error {
	u := "http://poly-lsp" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return &transportError{err}
	}
	return decode(resp, out)
}

func (c *Client) postJSON(path string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, "http://poly-lsp"+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setHeaders(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return &transportError{err}
	}
	return decode(resp, out)
}

func decode(resp *http.Response, out any) error {
	defer resp.Body.Close()
	b, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return &transportError{rerr}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}
