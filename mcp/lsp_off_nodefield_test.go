package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// The `node` field of node_read / node_edit takes a full selector, and that
// selector must settle with the CALLING client's child-LSP switch. The
// fixture has TWO same-named declarations, so the switch is observable in
// the answer itself: with the child on, the LSP picks one far end and the
// selector matches exactly one node; with it off, the edge is lexical with
// two candidates and the selector is ambiguous — the pre-switch bug
// answered the node field with the daemon's defaults, so a client that
// turned go off still got the LSP-picked node.

// selectorNodeFixture writes a workspace whose call edge is ambiguous
// without an LSP: Run calls Save, and Save is declared in two files. want
// is the file the fake LSP points at (the on answer).
func selectorNodeFixture(t *testing.T, dir, want string) {
	t.Helper()
	write := func(rel, src string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module collide\ngo 1.21\n")
	write("a.go", "package main\n\n// STORE-ANCHOR\nfunc Save(v string) string {\n\treturn \"store:\" + v\n}\n")
	write("b.go", "package main\n\n// OTHER-ANCHOR\nfunc Save(v string) string {\n\treturn \"other:\" + v\n}\n")
	write("main.go", "package main\n\nfunc Run() string {\n\treturn Save(\"x\")\n}\n")
}

// fakeDefRegistry is the default config with go's child set to the test
// binary's fake definition LSP, pointing every answer at want.
func fakeDefRegistry(t *testing.T, want string) *config.Registry {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	for i := range cfg.Languages {
		if cfg.Languages[i].Name == "go" {
			cfg.Languages[i].LSP = &config.LSP{Cmd: exe, Args: []string{fakeDefLSPArg}, Env: []string{
				"FAKE_DEF_URI=file://" + want, "FAKE_DEF_LINE=2",
			}}
		}
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// (a) A daemon session: node_read's node field settles with the session's
// own switch, not the server's. The off client's ambiguous selector must
// error, the on client's must resolve to the LSP-picked file.
func TestNodeFieldSettlesWithTheSessionSwitch(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "a.go")
	selectorNodeFixture(t, dir, want)

	reg := fakeDefRegistry(t, want)
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetSessionLSP("off", LSPPrefs{Off: []string{"go"}})
	srv.SetSessionLSP("on", LSPPrefs{})
	if err := srv.BringUp(true); err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	args, _ := json.Marshal(map[string]any{"node": `#'main.go#Run'::out.call`})
	// Off client: the edge is lexical, both Save candidates stay, the
	// selector is ambiguous — returned as (nil, true, err).
	_, _, err := srv.CallTool("off", "node_read", args, CallOptions{})
	if err == nil {
		t.Fatal("off client: the ambiguous selector resolved with the daemon's defaults")
	}
	if !strings.Contains(err.Error(), "a.go#Save") || !strings.Contains(err.Error(), "b.go#Save") {
		t.Fatalf("off client: the ambiguity does not name both candidates: %v", err)
	}

	// On client: the LSP picks one far end, the selector matches exactly
	// one node, and node_read returns that file's content.
	content, isError, err := srv.CallTool("on", "node_read", args, CallOptions{})
	if err != nil {
		t.Fatalf("node_read on: %v", err)
	}
	if isError {
		t.Fatalf("on client: the LSP-settled selector errored: %s", content[0].Text)
	}
	if !strings.Contains(content[0].Text, "STORE-ANCHOR") {
		t.Fatalf("on client: did not read the LSP-picked file:\n%s", content[0].Text)
	}
}

// (c) The stdio server: --lsp-off (SetLSPPrefs, the local session) applies
// to the node field exactly as it does to node_query — the pinning pair on
// top of the existing node_query coverage in lsp_off_test.go.
func TestLSPOffStdioNodeField(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "a.go")
	selectorNodeFixture(t, dir, want)

	reg := fakeDefRegistry(t, want)
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetLSPPrefs(LSPPrefs{Off: []string{"go"}})
	if err := srv.BringUp(true); err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	args, _ := json.Marshal(map[string]any{"node": `#'main.go#Run'::out.call`})
	// --lsp-off is the local session's switch: the edge is lexical, the
	// selector is ambiguous, and node_read returns the ambiguity as err.
	_, _, err := srv.CallTool("", "node_read", args, CallOptions{})
	if err == nil {
		t.Fatal("node_read with --lsp-off: the ambiguous selector resolved")
	}
	if !strings.Contains(err.Error(), "a.go#Save") || !strings.Contains(err.Error(), "b.go#Save") {
		t.Fatalf("node_read with --lsp-off: the ambiguity does not name both candidates: %v", err)
	}
}
