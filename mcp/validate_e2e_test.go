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

// With --validate (SetValidateEdits), an edit that introduces a NEW error must
// be REVERTED (file unchanged, response rejected), while a valid edit lands.
// This is the revert-on-new-diagnostics contract the edit-safety benchmark
// measures. gopls is the real target; skipped when it's absent / under -short.
func TestNodeEditValidateReverts(t *testing.T) {
	if testing.Short() {
		t.Skip("validate e2e skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}

	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.go")
	orig := "package main\n\nfunc main() {\n\tx := 1\n\t_ = x\n}\n"
	if err := os.WriteFile(mainPath, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetLegacyTools(true)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetValidateEdits(true) // the flag under test
	srv.SetDiagnosticWait(8 * time.Second)

	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	sess := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
	defer sess.close()

	sess.request("initialize", map[string]any{})
	sess.notify("notifications/initialized", map[string]any{})

	decode := func(r toolResp) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(r.Content[0].Text), &m); err != nil {
			t.Fatalf("decode edit response: %v", err)
		}
		return m
	}

	// 1) A breaking edit (undeclared function) must be REVERTED. A rejection
	// is a tool-level failure, so isError=true is CORRECT here — the model must
	// see the edit did not apply.
	broken := "package main\n\nfunc main() {\n\tdoesNotExist()\n}\n"
	r := sess.callTool("node_edit", map[string]any{
		"file": "main.go", "startLine": 1, "startCol": 1, "endLine": 7, "endCol": 1,
		"newText": broken,
	})
	if !r.IsError {
		t.Fatalf("rejected edit should surface as isError=true; response=%+v", r.Content)
	}
	m := decode(r)
	if m["rejected"] != true {
		t.Fatalf("breaking edit was NOT rejected; response=%+v", m)
	}
	if m["reverted"] != true {
		t.Fatalf("breaking edit rejected but not reverted; response=%+v", m)
	}
	if got, _ := os.ReadFile(mainPath); string(got) != orig {
		t.Fatalf("file not restored after revert:\n got: %q\nwant: %q", got, orig)
	}

	// 2) A valid edit must LAND (not rejected), file changed.
	good := "package main\n\nfunc main() {\n\tprintln(\"ok\")\n}\n"
	r = sess.callTool("node_edit", map[string]any{
		"file": "main.go", "startLine": 1, "startCol": 1, "endLine": 7, "endCol": 1,
		"newText": good,
	})
	if r.IsError {
		t.Fatalf("valid node_edit transport error: %+v", r.Content)
	}
	m = decode(r)
	if m["rejected"] == true {
		t.Fatalf("valid edit was wrongly rejected; response=%+v", m)
	}
	if got, _ := os.ReadFile(mainPath); string(got) != good {
		t.Fatalf("valid edit did not land:\n got: %q\nwant: %q", got, good)
	}
}

// goplsValidateSession starts a validating server on a one-file Go module
// holding orig in main.go, gopls as its child. Skipped without gopls or
// under -short.
func goplsValidateSession(t *testing.T, orig string) (*mcpSession, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("validate e2e skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetValidateEdits(true)
	srv.SetDiagnosticWait(8 * time.Second)
	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	sess := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
	t.Cleanup(sess.close)
	sess.request("initialize", map[string]any{})
	sess.notify("notifications/initialized", map[string]any{})
	return sess, mainPath
}

// A broken edit sent again right after its revert is reverted again. The
// revert waits for gopls's diagnostics of the reverted text, so the
// retry's error baseline no longer holds the reverted error (before, 5 of
// 5 retries were kept and reported validated:true).
func TestNodeEditValidateRepeatBrokenEdit(t *testing.T) {
	orig := "package main\n\nfunc main() {\n\tx := 1\n\t_ = x\n}\n"
	sess, mainPath := goplsValidateSession(t, orig)
	broken := "package main\n\nfunc main() {\n\tdoesNotExist()\n}\n"
	for i := 0; i < 5; i++ {
		m := editMain(t, sess, mainPath, broken)
		if m["rejected"] != true || m["reverted"] != true {
			t.Fatalf("broken edit %d not reverted: %v", i+1, m)
		}
		if got, _ := os.ReadFile(mainPath); string(got) != orig {
			t.Fatalf("broken edit %d: file not restored: %q", i+1, got)
		}
	}
	good := "package main\n\nfunc main() {\n\tprintln(\"ok\")\n}\n"
	if m := editMain(t, sess, mainPath, good); m["validated"] != true {
		t.Fatalf("valid edit after the reverts not validated: %v", m)
	}
}
