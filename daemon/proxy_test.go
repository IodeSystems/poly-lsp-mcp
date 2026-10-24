package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// proxyHarness drives RunProxy over pipes as an MCP client would.
type proxyHarness struct {
	t    *testing.T
	in   io.WriteCloser
	mu   sync.Mutex
	msgs []map[string]any
	cond *sync.Cond
	id   int
}

func startProxy(t *testing.T, c *Client, root string, opts OpenOptions) *proxyHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- RunProxy(func() (*Client, error) { return c, nil }, root, opts, inR, outW)
		_ = outW.Close()
	}()
	h := newHarness(t, inW, outR)
	t.Cleanup(func() {
		h.request("shutdown", nil)
		_ = inW.Close()
		<-done
	})
	return h
}

// newHarness reads the proxy's stdout (out) and writes its stdin (in).
func newHarness(t *testing.T, in io.WriteCloser, out io.Reader) *proxyHarness {
	h := &proxyHarness{t: t, in: in}
	h.cond = sync.NewCond(&h.mu)
	outR := out
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				h.mu.Lock()
				h.msgs = append(h.msgs, m)
				h.cond.Broadcast()
				h.mu.Unlock()
			}
		}
	}()
	return h
}

// request sends a request and returns its id.
func (h *proxyHarness) request(method string, params any) int {
	h.id++
	m := map[string]any{"jsonrpc": "2.0", "id": h.id, "method": method}
	if params != nil {
		m["params"] = params
	}
	b, _ := json.Marshal(m)
	_, _ = h.in.Write(append(b, '\n'))
	return h.id
}

// await waits for the first message matching ok.
func (h *proxyHarness) await(what string, d time.Duration, ok func(map[string]any) bool) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(d)
	go func() { time.Sleep(d); h.mu.Lock(); h.cond.Broadcast(); h.mu.Unlock() }()
	h.mu.Lock()
	defer h.mu.Unlock()
	for {
		for _, m := range h.msgs {
			if ok(m) {
				return m
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("no %s within %s; got %v", what, d, h.msgs)
		}
		h.cond.Wait()
	}
}

func (h *proxyHarness) reply(id int, d time.Duration) map[string]any {
	h.t.Helper()
	return h.await(fmt.Sprintf("reply %d", id), d, func(m map[string]any) bool {
		v, ok := m["id"].(float64)
		return ok && int(v) == id
	})
}

// logged waits for a notifications/message from logger whose data
// satisfies ok.
func (h *proxyHarness) logged(logger string, d time.Duration, ok func(data map[string]any) bool) map[string]any {
	h.t.Helper()
	m := h.await("notification "+logger, d, func(m map[string]any) bool {
		if m["method"] != "notifications/message" {
			return false
		}
		p, _ := m["params"].(map[string]any)
		data, _ := p["data"].(map[string]any)
		return p["logger"] == logger && (ok == nil || ok(data))
	})
	p := m["params"].(map[string]any)
	data, _ := p["data"].(map[string]any)
	return data
}

func callText(t *testing.T, m map[string]any) (string, bool) {
	t.Helper()
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("not a result: %v", m)
	}
	content, _ := res["content"].([]any)
	var b strings.Builder
	for _, c := range content {
		b.WriteString(c.(map[string]any)["text"].(string))
	}
	isErr, _ := res["isError"].(bool)
	return b.String(), isErr
}

func goWorkspace(t *testing.T) (root, resolved string) {
	t.Helper()
	root = t.TempDir()
	for i := 0; i < 5; i++ {
		src := fmt.Sprintf("package main\n\nfunc F%d() int { return %d }\n", i, i)
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%d.go", i)), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vendor", "v.go"), []byte("package v\n\nfunc Vendored() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, _ = resolvePath(root)
	return root, resolved
}

// The proxy is the stdio server, through the daemon: initialize answers at
// once with the index building, the bring-up's index and setup
// notifications reach the client, and a tool call waits for the root. The
// client's --exclude reaches the root's build and its leftOut report.
func TestProxyParityWithTheStdioServer(t *testing.T) {
	root, resolved := goWorkspace(t)
	c, stop := startTestDaemon(t, resolved)
	defer stop()

	h := startProxy(t, c, root, OpenOptions{Excludes: []string{"vendor"}})
	start := time.Now()
	init := h.reply(h.request("initialize", map[string]any{}), 5*time.Second)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("initialize took %s", took)
	}
	res := init["result"].(map[string]any)
	if idx, _ := res["index"].(map[string]any); idx["state"] != "building" {
		t.Errorf("initialize index = %v, want building", res["index"])
	}
	if caps := res["capabilities"].(map[string]any); caps["logging"] == nil {
		t.Errorf("initialize does not advertise logging: %v", caps)
	}

	data := h.logged(indexLogger, 30*time.Second, nil)
	if data["state"] != "ready" {
		t.Fatalf("index notification = %v", data)
	}
	rep, _ := data["index"].(map[string]any)
	b, _ := json.Marshal(rep["leftOut"])
	if !strings.Contains(string(b), `"vendor"`) || !strings.Contains(string(b), "excluded") {
		t.Errorf("leftOut does not name the excluded vendor/: %s", b)
	}
	h.logged(setupLogger, 10*time.Second, nil)

	call := h.reply(h.request("tools/call", map[string]any{"name": "node_query", "arguments": map[string]any{"selector": "#F3"}}), 30*time.Second)
	text, isErr := callText(t, call)
	if isErr || !strings.Contains(text, "F3") {
		t.Fatalf("node_query: isErr=%v %q", isErr, text)
	}
	call = h.reply(h.request("tools/call", map[string]any{"name": "node_query", "arguments": map[string]any{"selector": "#Vendored"}}), 30*time.Second)
	if text, _ := callText(t, call); strings.Contains(text, "vendor/v.go") {
		t.Errorf("an excluded dir was indexed: %q", text)
	}

	// A second client of the warm root gets the same report, replayed.
	h2 := startProxy(t, c, root, OpenOptions{})
	h2.reply(h2.request("initialize", map[string]any{}), 5*time.Second)
	if data := h2.logged(indexLogger, 10*time.Second, nil); data["state"] != "ready" {
		t.Fatalf("second client's index notification = %v", data)
	}
}

// A root over its index budget fails the way a stdio server's does: the
// notification says failed with the remedy, and a tool call answers with
// the post-mortem. A new client with another budget builds it again.
func TestProxyIndexBudgetFailureAndRetry(t *testing.T) {
	root, resolved := goWorkspace(t)
	c, stop := startTestDaemon(t, resolved)
	defer stop()

	h := startProxy(t, c, root, OpenOptions{Budget: "1ops"})
	h.reply(h.request("initialize", map[string]any{}), 5*time.Second)
	data := h.logged(indexLogger, 30*time.Second, nil)
	if data["state"] != "failed" || !strings.Contains(fmt.Sprint(data["remedy"]), "--index-budget") {
		t.Fatalf("index notification = %v", data)
	}
	text, isErr := callText(t, h.reply(h.request("tools/call", map[string]any{"name": "node_query", "arguments": map[string]any{"selector": "#F1"}}), 30*time.Second))
	if !isErr || !strings.Contains(text, "remedy:") {
		t.Fatalf("call on a failed root: isErr=%v %q", isErr, text)
	}

	c2 := NewClient(c.socket)
	h2 := startProxy(t, c2, root, OpenOptions{Budget: "0"})
	h2.reply(h2.request("initialize", map[string]any{}), 5*time.Second)
	if data := h2.logged(indexLogger, 30*time.Second, func(d map[string]any) bool { return d["state"] == "ready" }); data == nil {
		t.Fatal("no ready notification after the retry")
	}
	text, isErr = callText(t, h2.reply(h2.request("tools/call", map[string]any{"name": "node_query", "arguments": map[string]any{"selector": "#F1"}}), 30*time.Second))
	if isErr || !strings.Contains(text, "F1") {
		t.Fatalf("call after the retry: isErr=%v %q", isErr, text)
	}
}

// A directory of clones is refused through the daemon as by a stdio
// server, and the client's --index-anyway, carried in its /open, gets it
// indexed — the daemon honours the per-client index flags.
func TestProxyIndexAnywayReachesTheDaemon(t *testing.T) {
	dir := t.TempDir()
	for i := range 3 {
		for name, body := range map[string]string{".git/HEAD": "ref: refs/heads/main\n", "main.go": fmt.Sprintf("package main\n\nfunc Repo%d() {}\n", i)} {
			p := filepath.Join(dir, fmt.Sprintf("repo%d", i), name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	resolved, _ := resolvePath(dir)
	c, stop := startTestDaemon(t, resolved)
	defer stop()

	h := startProxy(t, c, dir, OpenOptions{})
	h.reply(h.request("initialize", map[string]any{}), 5*time.Second)
	if data := h.logged(indexLogger, 30*time.Second, nil); data["state"] != "failed" || !strings.Contains(fmt.Sprint(data["remedy"]), "--index-anyway") {
		t.Fatalf("index notification = %v", data)
	}

	h2 := startProxy(t, NewClient(c.socket), dir, OpenOptions{IndexAnyway: true})
	h2.reply(h2.request("initialize", map[string]any{}), 5*time.Second)
	h2.logged(indexLogger, 30*time.Second, func(d map[string]any) bool { return d["state"] == "ready" })
	text, isErr := callText(t, h2.reply(h2.request("tools/call", map[string]any{"name": "node_query", "arguments": map[string]any{"selector": "#Repo1"}}), 30*time.Second))
	if isErr || !strings.Contains(text, "Repo1") {
		t.Fatalf("--index-anyway root: isErr=%v %q", isErr, text)
	}
}

// tools/list does not wait for the bring-up: the catalog does not depend
// on the index.
func TestDaemonToolsDoNotBuildTheRoot(t *testing.T) {
	root, resolved := goWorkspace(t)
	c, stop := startTestDaemon(t, resolved)
	defer stop()
	tools, err := c.Tools(root)
	if err != nil || len(tools) != 3 {
		t.Fatalf("tools = %d, %v", len(tools), err)
	}
	h, _ := c.health()
	if len(h.Roots) != 0 {
		t.Fatalf("tools/list built %v", h.Roots)
	}
}
