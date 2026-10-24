package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Healthy reports whether the daemon answers /health.
func (c *Client) Healthy() bool { return probeHealth(c.hc) }

// Open warms a root on the daemon and returns the indexed-name count.
func (c *Client) Open(root string) (int, error) {
	var out struct {
		Root  string `json:"root"`
		Names int    `json:"names"`
	}
	if err := c.postJSON("/open", map[string]string{"root": root}, &out); err != nil {
		return 0, err
	}
	return out.Names, nil
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
		return err
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
		return err
	}
	return decode(resp, out)
}

func decode(resp *http.Response, out any) error {
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}
