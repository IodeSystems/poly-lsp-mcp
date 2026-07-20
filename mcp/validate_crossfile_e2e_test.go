package mcp

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// The edit that motivated cross-file validation: a single node_edit to config.go
// that renames a type its SIBLING server.go depends on. config.go itself still
// compiles; server.go breaks (undefined: Config). Single-file validation would
// let this land — workspace-wide validation must REVERT it.
func TestNodeEditValidateCrossFileRevert(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-file validate e2e skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.go")
	// config.go defines the type but does NOT use it internally, so renaming the
	// declaration breaks ONLY the sibling — isolating the cross-file case.
	cfgOrig := "package x\n\ntype Config struct {\n\tPort int\n}\n"
	if err := os.WriteFile(cfgPath, []byte(cfgOrig), 0o644); err != nil {
		t.Fatal(err)
	}
	srvOrig := "package x\n\nfunc Addr(c Config) int {\n\treturn c.Port\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "server.go"), []byte(srvOrig), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetLegacyTools(true)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetValidateEdits(true)
	srv.SetDiagnosticWait(8 * time.Second)

	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	sess := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
	defer sess.close()

	sess.request("initialize", map[string]any{})
	sess.notify("notifications/initialized", map[string]any{})

	// Rename the type in config.go only — compiles here, breaks server.go.
	broken := "package x\n\ntype Renamed struct {\n\tPort int\n}\n"
	r := sess.callTool("node_edit", map[string]any{
		"file": "config.go", "startLine": 1, "startCol": 1, "endLine": 6, "endCol": 1,
		"newText": broken,
	})
	if !r.IsError {
		t.Fatalf("cross-file break should surface as isError=true; response=%+v", r.Content)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Content[0].Text), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["rejected"] != true || m["reverted"] != true {
		t.Fatalf("cross-file break not rejected+reverted; got %+v", m)
	}
	if got, _ := os.ReadFile(cfgPath); string(got) != cfgOrig {
		t.Fatalf("config.go not restored:\n got: %q\nwant: %q", got, cfgOrig)
	}
}
