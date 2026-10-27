package mcp

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/jsonrpc"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// fakeDefLSPArg makes the test binary act as a child LSP that answers every
// textDocument/definition with the location in $FAKE_DEF_URI, line
// $FAKE_DEF_LINE (0-based). It needs no language server on PATH, so the
// precision pass runs on any CI host.
const fakeDefLSPArg = "--fake-def-lsp"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == fakeDefLSPArg {
		runFakeDefLSP()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == fakeDiagLSPArg {
		runFakeDiagLSP()
		return
	}
	os.Exit(m.Run())
}

func runFakeDefLSP() {
	r := bufio.NewReader(os.Stdin)
	for {
		msg, err := jsonrpc.Read(r)
		if err != nil || msg.Method == "exit" {
			return
		}
		if msg.IsNotification() {
			continue
		}
		resp := &jsonrpc.Message{JSONRPC: "2.0", ID: msg.ID}
		switch msg.Method {
		case "initialize":
			resp.Result = json.RawMessage(`{"capabilities":{"definitionProvider":true}}`)
		case "shutdown":
			resp.Result = json.RawMessage(`null`)
		case "textDocument/definition":
			line := os.Getenv("FAKE_DEF_LINE")
			resp.Result = json.RawMessage(`{"uri":` + strconvQuote(os.Getenv("FAKE_DEF_URI")) +
				`,"range":{"start":{"line":` + line + `,"character":0},"end":{"line":` + line + `,"character":0}}}`)
		default:
			resp.Error = &jsonrpc.Error{Code: -32601, Message: "method not found: " + msg.Method}
		}
		if jsonrpc.Write(os.Stdout, resp) != nil {
			return
		}
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A root reached through a symlinked parent (macOS t.TempDir() is under
// /var, a symlink to /private/var) and a child LSP that answers with the
// resolved path, as sourcekit-lsp does. The definition is inside the root;
// it must settle the edge on the local declaration, not mint an external
// stub for /private/....
func TestPrecisionMapsResolvedChildPathIntoSymlinkedRoot(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	root := filepath.Join(link, "ws")
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module collide\ngo 1.21\n")
	write("store/store.go", "package store\n\nfunc Save(v string) string {\n\treturn \"store:\" + v\n}\n")
	write("cache/cache.go", "package cache\n\nfunc Save(v string) string {\n\treturn \"cache:\" + v\n}\n")
	write("main.go", "package main\n\nimport \"collide/store\"\n\nfunc Run() string {\n\treturn store.Save(\"x\")\n}\n")

	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	for i := range cfg.Languages {
		if cfg.Languages[i].Name == "go" {
			cfg.Languages[i].LSP = &config.LSP{Cmd: exe, Args: []string{fakeDefLSPArg}, Env: []string{
				"FAKE_DEF_URI=file://" + filepath.Join(resolved, "store/store.go"),
				"FAKE_DEF_LINE=2", // func Save, 0-based
			}}
		}
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, root, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	s := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	q := query(t, s, map[string]any{"selector": `#'main.go#Run'::out.call`, "limit": 20})
	var saw bool
	for _, m := range q.Matches {
		if m.Domain == "external" {
			t.Errorf("an in-root definition became an external stub: %v (conf=%q)", m.To, m.Conf)
		}
		joined := strings.Join(m.To, ",")
		if !strings.Contains(joined, "store/store.go#Save") {
			continue
		}
		saw = true
		if strings.Contains(joined, "cache/cache.go#Save") {
			t.Errorf("the child resolves store.Save; the edge still lists cache's Save: %v (conf=%q)", m.To, m.Conf)
		}
		if m.Conf != refLSP {
			t.Errorf("conf=%q, want %q", m.Conf, refLSP)
		}
	}
	if !saw {
		t.Fatalf("no outgoing call edge to store.Save; got %+v", q.Matches)
	}
}
