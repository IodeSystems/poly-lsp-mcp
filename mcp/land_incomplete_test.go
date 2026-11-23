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

// landStart serves a one-file Go module through real gopls with validation on.
func landStart(t *testing.T) (*mcpSession, string, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("gopls e2e skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package x\n\nfunc F() int { return 1 }\n"
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, _ := config.Default().Build()
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetValidateEdits(true)
	srv.SetDiagnosticWait(8 * time.Second)
	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	s := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	return s, path, src
}

// An edit whose only new error is the incomplete class (declared and not used)
// lands: the file changes, the errors are listed with file:line, and the
// staging tip shows once, then again only after stagingHintRepeat tool calls.
func TestIncompleteEditLandsWithErrorsAndOnceHint(t *testing.T) {
	s, path, _ := landStart(t)
	defer s.close()

	r := s.callTool("node_edit", map[string]any{
		"node": "a.go#F", "oldText": "return 1", "newText": "x := 1\n\treturn 1",
	})
	if r.IsError {
		t.Fatalf("an incomplete edit must land, not error; got %+v", r.Content)
	}
	m := txnDecode(t, r)
	t.Logf("landed result: %s", r.Content[0].Text)
	if m["rejected"] == true || m["incomplete"] != true || m["validated"] != false {
		t.Fatalf("want landed incomplete, not rejected/validated; got %+v", m)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "x := 1") {
		t.Fatalf("the edit must be on disk; got %q", b)
	}
	errs, _ := m["newErrors"].([]any)
	if len(errs) == 0 || !strings.Contains(errs[0].(string), "a.go:") || !strings.Contains(errs[0].(string), "declared and not used") {
		t.Fatalf("newErrors must list file:line and message; got %+v", m["newErrors"])
	}
	if tip, _ := m["tip"].(string); !strings.Contains(tip, "commit:false") {
		t.Fatalf("first occurrence must carry the staging tip; got %+v", m)
	}

	// Second occurrence: still lands, no tip.
	m = txnDecode(t, s.callTool("node_edit", map[string]any{
		"node": "a.go#F", "oldText": "return 1", "newText": "y := 2\n\treturn 1",
	}))
	if m["incomplete"] != true || m["tip"] != nil {
		t.Fatalf("second occurrence must land with NO tip; got %+v", m)
	}

	// Below the threshold: still no tip. At it: the tip returns.
	m = txnDecode(t, s.callTool("node_edit", map[string]any{
		"node": "a.go#F", "oldText": "return 1", "newText": "z := 3\n\treturn 1",
	}))
	if m["tip"] != nil {
		t.Fatalf("tip must not repeat before %d calls; got %+v", stagingHintRepeat, m)
	}
	for i := 0; i < stagingHintRepeat; i++ {
		s.srv.noteToolCall(localSession)
	}
	m = txnDecode(t, s.callTool("node_edit", map[string]any{
		"node": "a.go#F", "oldText": "return 1", "newText": "w := 4\n\treturn 1",
	}))
	if m["tip"] == nil {
		t.Fatalf("tip must reappear after %d tool calls; got %+v", stagingHintRepeat, m)
	}
}

// An edit that introduces any OTHER error is still rejected and reverted.
func TestNonIncompleteErrorStillRejected(t *testing.T) {
	s, path, src := landStart(t)
	defer s.close()

	r := s.callTool("node_edit", map[string]any{
		"node": "a.go#F", "oldText": "return 1", "newText": `return "s"`,
	})
	m := txnDecode(t, r)
	if !r.IsError || m["rejected"] != true || m["reverted"] != true {
		t.Fatalf("a type error must still be rejected; got %+v", m)
	}
	if b, _ := os.ReadFile(path); string(b) != src {
		t.Fatal("rejected edit must revert the file")
	}
}

// The hint clock is per session: one session's shown-hint does not silence
// another's.
func TestStagingHintPerSession(t *testing.T) {
	s := &Server{}
	if !s.stagingHintDue("a") || s.stagingHintDue("a") {
		t.Fatal("session a: shown once, then not")
	}
	if !s.stagingHintDue("b") {
		t.Fatal("session b has its own clock")
	}
	for i := 0; i < stagingHintRepeat-1; i++ {
		s.noteToolCall("a")
	}
	if s.stagingHintDue("a") {
		t.Fatal("one call short of the threshold must not show")
	}
	s.noteToolCall("a")
	if !s.stagingHintDue("a") {
		t.Fatal("at the threshold the hint returns")
	}
}
