package mcp

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// Validation is ON by default as of 2026-08-04. A safety net that must be
// asked for is a safety net nobody has: dun spawns this server with no flags,
// so every agent edit ran unvalidated — and node_edit's `return` op will
// happily leave a signature the body no longer satisfies.
//
// These pin the three behaviours the default depends on. The flag plumbing
// itself lives in main.go; what matters here is that the SERVER reverts when
// told to, lands when not, and never blocks a workspace it cannot validate.
func TestValidateRevertsABreakingEdit(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}
	dir := t.TempDir()
	src := "package main\n\ntype Svc struct{ N int }\n\nfunc (s Svc) Run(a int) error { return nil }\n"
	path := filepath.Join(dir, "svc.go")
	for name, body := range map[string]string{"svc.go": src, "go.mod": "module x\n\ngo 1.21\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
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
	defer sess.close()
	sess.request("initialize", map[string]any{})
	sess.notify("notifications/initialized", map[string]any{})

	// `return` rebuilds the SIGNATURE and not the body, so this leaves a
	// method whose body no longer satisfies it — the shape that motivated
	// making validation the default.
	r := sess.callTool("node_edit", map[string]any{"node": "svc.go#Svc.Run", "return": "(int, error)"})
	if !r.IsError {
		t.Error("an edit that breaks the build should be refused under validation")
	}
	got, _ := os.ReadFile(path)
	if string(got) != src {
		t.Errorf("the file must be reverted byte for byte:\n%s", got)
	}
}

// Without validation the same edit LANDS — that is what --no-validate buys,
// and why it exists for when validation itself misfires.
func TestWithoutValidateTheBreakingEditLands(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	s.srv.SetValidateEdits(false)

	path := filepath.Join(dir, "svc.go")
	src := "package main\n\ntype Svc struct{ N int }\n\nfunc (s Svc) Run(a int) error { return nil }\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := s.callTool("node_edit", map[string]any{"node": "svc.go#Svc.Run", "return": "(int, error)"}); r.IsError {
		t.Fatalf("without validation the edit should land: %s", r.Content[0].Text)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "(int, error)") {
		t.Errorf("the edit should have landed:\n%s", got)
	}
}

// The property that makes the default SAFE: a workspace whose language has no
// server must stay editable. Validation degrades to apply-and-flag rather than
// refusing what it cannot prove.
func TestValidateDoesNotBlockAnUnvalidatableWorkspace(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()
	s.srv.SetValidateEdits(true)

	path := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(path, []byte("# Title\n\nbody text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := s.callTool("node_edit", map[string]any{
		"node": "notes.md", "oldText": "body text", "newText": "new body",
	})
	if r.IsError {
		t.Fatalf("markdown has no language server; the edit must still apply: %s", r.Content[0].Text)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "new body") {
		t.Errorf("the edit should have applied:\n%s", got)
	}
}
