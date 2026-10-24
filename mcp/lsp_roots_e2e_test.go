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

// --lsp-roots auto with real gopls: two modules and no go.mod at the
// workspace root. An edit in b/ starts one gopls rooted at b/, nothing
// else, and validation works through it.
func TestLSPRootsAutoValidatesThroughTheRootsChild(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}
	dir := writeTree(t, map[string]string{
		"a/go.mod": "module a\n\ngo 1.21\n", "a/main.go": "package main\n\nfunc main() {}\n",
		"b/go.mod": "module b\n\ngo 1.21\n", "b/main.go": "package main\n\nfunc main() {}\n",
	})
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetLegacyTools(true)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetLSPRoots(true, 8)
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

	if st := srv.manager.Status(); len(st) != 0 {
		t.Fatalf("a child started before first use: %+v", st)
	}
	orig, _ := os.ReadFile(filepath.Join(dir, "b", "main.go"))
	r := sess.callTool("node_edit", map[string]any{
		"file": "b/main.go", "startLine": 1, "startCol": 1, "endLine": 4, "endCol": 1,
		"newText": "package main\n\nfunc main() {\n\tdoesNotExist()\n}\n",
	})
	var m map[string]any
	if len(r.Content) == 0 || json.Unmarshal([]byte(r.Content[0].Text), &m) != nil || m["rejected"] != true {
		t.Fatalf("a breaking edit in b/ should be rejected by b's gopls: %+v", r.Content)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b", "main.go")); string(got) != string(orig) {
		t.Fatalf("b/main.go not restored: %q", got)
	}
	st := srv.manager.Status()
	if len(st) != 1 || st[0].Language != "go" || st[0].Root != filepath.Join(dir, "b") {
		t.Fatalf("want one go child rooted at b/: %+v", st)
	}
}
