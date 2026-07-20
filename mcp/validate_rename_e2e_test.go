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

// A workspace-wide rename that INTRODUCES an error (renaming Foo → Bar where
// Bar already exists = redeclaration) must revert EVERY file the refactor
// touched — the multi-file all-or-nothing contract of validationTxn.
func TestRefactorRenameValidateRevertsAllFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("validate rename e2e skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Foo and Bar both exist; renaming Foo→Bar redeclares Bar (a compile error).
	aPath := filepath.Join(dir, "a.go")
	bPath := filepath.Join(dir, "b.go")
	origA := "package x\n\nfunc Foo() int { return 1 }\nfunc Bar() int { return 2 }\n"
	origB := "package x\n\nfunc UseFoo() int { return Foo() }\n"
	if err := os.WriteFile(aPath, []byte(origA), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, []byte(origB), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil) // modern 3-tool surface (node_edit has rename)
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

	r := sess.callTool("node_edit", map[string]any{
		"node":   "a.go#Foo",
		"rename": "Bar",
	})
	if !r.IsError {
		t.Fatalf("colliding rename should surface as isError=true; response=%+v", r.Content)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Content[0].Text), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["rejected"] != true || m["reverted"] != true {
		t.Fatalf("expected rejected+reverted; got %+v", m)
	}
	// Every touched file must be back to its original bytes.
	if got, _ := os.ReadFile(aPath); string(got) != origA {
		t.Fatalf("a.go not restored:\n got: %q\nwant: %q", got, origA)
	}
	if got, _ := os.ReadFile(bPath); string(got) != origB {
		t.Fatalf("b.go not restored:\n got: %q\nwant: %q", got, origB)
	}
}
