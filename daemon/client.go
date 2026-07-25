package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"

	"github.com/iodesystems/poly-lsp-mcp/mcp"
)

// Client dials a daemon over its unix socket. The URL host is irrelevant
// (the transport always dials the socket); "poly-lsp" is a placeholder.
type Client struct {
	socket string
	hc     *http.Client
}

// NewClient builds a client for the given socket path.
func NewClient(socket string) *Client {
	return &Client{
		socket: socket,
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

func (c *Client) get(path string, q url.Values, out any) error {
	u := "http://poly-lsp" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	resp, err := c.hc.Get(u)
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
	resp, err := c.hc.Post("http://poly-lsp"+path, "application/json", bytes.NewReader(buf))
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
