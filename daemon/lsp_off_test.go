package daemon

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/jsonrpc"
)

// fakeLSPArg makes the test binary a child LSP: it answers initialize and
// shutdown, and publishes each didOpen / didChange at once with its
// version and one error when the text holds BROKEN. No language server on
// PATH is needed.
const fakeLSPArg = "--fake-daemon-lsp"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == fakeLSPArg {
		runFakeLSP()
		return
	}
	os.Exit(m.Run())
}

func runFakeLSP() {
	texts := map[string]string{}
	r := bufio.NewReader(os.Stdin)
	for {
		msg, err := jsonrpc.Read(r)
		if err != nil || msg.Method == "exit" {
			return
		}
		if msg.IsResponse() {
			continue
		}
		if !msg.IsNotification() {
			resp := &jsonrpc.Message{JSONRPC: "2.0", ID: msg.ID}
			switch msg.Method {
			case "initialize":
				resp.Result = json.RawMessage(`{"capabilities":{"textDocumentSync":1}}`)
			case "shutdown":
				resp.Result = json.RawMessage(`null`)
			default:
				resp.Error = &jsonrpc.Error{Code: -32601, Message: "method not found: " + msg.Method}
			}
			if jsonrpc.Write(os.Stdout, resp) != nil {
				return
			}
			continue
		}
		var p struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Version int    `json:"version"`
				Text    string `json:"text"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		uri := p.TextDocument.URI
		switch msg.Method {
		case "textDocument/didOpen":
			texts[uri] = p.TextDocument.Text
		case "textDocument/didChange":
			if n := len(p.ContentChanges); n > 0 {
				texts[uri] = p.ContentChanges[n-1].Text
			}
		default:
			continue
		}
		diags := []any{}
		if strings.Contains(texts[uri], "BROKEN") {
			diags = append(diags, map[string]any{
				"range":    map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 1}},
				"severity": 1, "message": "broken here",
			})
		}
		raw, _ := json.Marshal(map[string]any{"uri": uri, "version": p.TextDocument.Version, "diagnostics": diags})
		if jsonrpc.Write(os.Stdout, &jsonrpc.Message{JSONRPC: "2.0", Method: "textDocument/publishDiagnostics", Params: raw}) != nil {
			return
		}
	}
}

// fakeLSP is the test binary as a trusted child LSP.
func fakeLSP(t *testing.T) *config.LSP {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	trust := true
	return &config.LSP{Cmd: exe, Args: []string{fakeLSPArg}, TrustCleanDiagnostics: &trust}
}

// serveRegistry serves r on a test socket, allowing allowRoot.
func serveRegistry(t *testing.T, allowRoot string, r *Registry) string {
	t.Helper()
	socket := testSocket(t)
	ln, err := listenUnix(socket)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := buildHandler(NewAllowList([]string{allowRoot}), r)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); r.Close() })
	c := NewClient(socket)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !c.Healthy(); {
		time.Sleep(20 * time.Millisecond)
	}
	return socket
}

// editFile replaces a whole file through the daemon as c's session.
func editFile(t *testing.T, c *Client, root, rel, text string) map[string]any {
	t.Helper()
	cur, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"node": rel, "oldText": string(cur), "newText": text})
	content, _, err := c.Call(root, "node_edit", args)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(content[0].Text), &m); err != nil {
		t.Fatalf("decode: %v (%s)", err, content[0].Text)
	}
	return m
}

// Two clients on one root, one with lsp_off go: the go child is shared
// and starts only for the client that wants it; the lsp_off client's
// edits get no diagnostics and are parse-checked only, the other's are
// validated by the child. Neither switch rebuilds the root.
func TestTwoClientsOneWithLSPOff(t *testing.T) {
	_, root := goWorkspace(t)
	cfg := config.Default()
	for i := range cfg.Languages {
		if cfg.Languages[i].Name == "go" {
			cfg.Languages[i].LSP = fakeLSP(t)
		}
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(cfg, reg, false, true)
	r.cachePath = ""
	socket := serveRegistry(t, root, r)

	off := NewClient(socket)
	off.SetValidateWait(5 * time.Second)
	if res, err := off.OpenWith(root, OpenOptions{LSPOff: []string{"go"}}); err != nil || res.Error != "" {
		t.Fatalf("open: %v %s", err, res.Error)
	}
	srv, err := r.Get(root)
	if err != nil {
		t.Fatal(err)
	}
	if st := srv.ChildStates()["go"]; st == "running" {
		t.Fatalf("the only client has go off; child %s", st)
	}
	m := editFile(t, off, root, "f0.go", "package main\n\n// BROKEN\nfunc F0() int { return 0 }\n")
	if m["rejected"] == true || m["validationSkipped"] != "lsp-off" || m["diagnosticsAvailable"] != false {
		t.Fatalf("lsp_off client's edit: %v", m)
	}
	if st := srv.ChildStates()["go"]; st == "running" {
		t.Fatalf("an lsp_off client's edit started the child")
	}

	on := NewClient(socket)
	on.SetValidateWait(5 * time.Second)
	if res, err := on.OpenWith(root, OpenOptions{}); err != nil || res.Error != "" {
		t.Fatalf("open: %v %s", err, res.Error)
	}
	if again, _ := r.Get(root); again != srv {
		t.Fatal("a client's lsp switch rebuilt the root")
	}
	if m := editFile(t, on, root, "f1.go", "package main\n\n// BROKEN\nfunc F1() int { return 1 }\n"); m["rejected"] != true {
		t.Fatalf("the client that wants go: its broken edit was not reverted: %v", m)
	}
	if st := srv.ChildStates()["go"]; st != "running" {
		t.Fatalf("child after the go client's edit: %q", st)
	}
	if m := editFile(t, on, root, "f1.go", "package main\n\nfunc F1() int { return 11 }\n"); m["validated"] != true {
		t.Fatalf("the go client's clean edit: %v", m)
	}
	// The child runs now; the lsp_off client still gets none of it.
	m = editFile(t, off, root, "f2.go", "package main\n\n// BROKEN\nfunc F2() int { return 2 }\n")
	if m["rejected"] == true || m["validationSkipped"] != "lsp-off" {
		t.Fatalf("lsp_off client with the child running: %v", m)
	}
	m = editFile(t, off, root, "f3.go", "package main\n\nfunc F3() int {\n")
	if m["rejected"] != true {
		t.Fatalf("lsp_off client: a parse-breaking edit was kept: %v", m)
	}
}

// servers.yaml is re-read on the next open: a language added there is
// served by the root rebuilt for that open, with its child LSP.
func TestServersFileChangeIsPickedUpOnNextOpen(t *testing.T) {
	root, err := resolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.fk"), []byte("fine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sp := filepath.Join(t.TempDir(), config.ServersFile)
	cfg := config.Default()
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(cfg, reg, false, true)
	r.cachePath = ""
	r.SetServersFile(sp, config.Default().Languages)
	socket := serveRegistry(t, root, r)
	c := NewClient(socket)
	c.SetValidateWait(5 * time.Second)

	if res, err := c.OpenWith(root, OpenOptions{}); err != nil || res.Error != "" {
		t.Fatalf("open: %v %s", err, res.Error)
	}
	first, _ := r.Get(root)
	if lang := r.LanguageForPath("a.fk"); lang != "" {
		t.Fatalf(".fk before servers.yaml: %q", lang)
	}

	l := fakeLSP(t)
	s := &config.Servers{Servers: []config.UserServer{{Name: "fake", Extensions: []string{"fk"}, Cmd: l.Cmd, Args: l.Args, TrustCleanDiagnostics: l.TrustCleanDiagnostics}}}
	if err := s.Save(sp); err != nil {
		t.Fatal(err)
	}
	if res, err := c.OpenWith(root, OpenOptions{}); err != nil || res.Error != "" {
		t.Fatalf("re-open: %v %s", err, res.Error)
	}
	if lang := r.LanguageForPath("a.fk"); lang != "fake" {
		t.Fatalf(".fk after servers.yaml: %q", lang)
	}
	second, _ := r.Get(root)
	if second == first {
		t.Fatal("the root was not rebuilt for the new servers.yaml")
	}
	if m := editFile(t, c, root, "a.fk", "BROKEN\n"); m["rejected"] != true {
		t.Fatalf("the new language's child did not judge the edit: %v", m)
	}

	// Removed again: the next open rebuilds without it.
	if err := os.Remove(sp); err != nil {
		t.Fatal(err)
	}
	if _, err := c.OpenWith(root, OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	if lang := r.LanguageForPath("a.fk"); lang != "" {
		t.Fatalf(".fk after servers.yaml was removed: %q", lang)
	}
	// An open with nothing changed keeps the root.
	third, _ := r.Get(root)
	if _, err := c.OpenWith(root, OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	if again, _ := r.Get(root); again != third {
		t.Fatal("an open with servers.yaml unchanged rebuilt the root")
	}
}
