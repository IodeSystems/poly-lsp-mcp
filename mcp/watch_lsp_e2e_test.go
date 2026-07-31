package mcp

import (
	"context"
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

// TestOutOfBandRewriteDoesNotStrandTheChildLSP is the dogfood bug, verbatim.
//
// A dun session ran `git rebase origin/main` through its exec tool. The rebase
// rewrote harness.go on disk — changing systemFor from one parameter to three
// — and rewrote the matching call site in servers_runtime.go. `go build`
// passed. Every subsequent node_edit on servers_runtime.go reported
// "too many arguments in call to systemFor: have 3, want 1" for the next
// twenty minutes, with diagnosticsTimedOut:false throughout.
//
// The cause was not the store and not the wait: proactive open had didOpen'd
// harness.go at its pre-rebase content, per LSP that overlay is authoritative
// for an open document, and nothing told the child otherwise — the file
// watcher fed the symbol index and stopped there. So the child kept
// type-checking the CURRENT call site against a signature that had not existed
// on disk for twenty minutes, and said so with complete confidence. That is
// worse than a missing diagnostic: node_read and node_query showed the correct
// three-parameter signature the whole time, so the two halves of the tool
// disagreed and the agent believed the confident half.
//
// The shape below reproduces it exactly: open the workspace, rewrite both
// files OUT OF BAND (os.WriteFile, the way git does), then edit through the
// tool and demand the diagnostics describe what is actually on disk.
func TestOutOfBandRewriteDoesNotStrandTheChildLSP(t *testing.T) {
	if testing.Short() {
		t.Skip("gopls e2e skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}

	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\n\ngo 1.21\n")
	// Pre-rebase state: one parameter, one argument.
	write("harness.go", "package x\n\nfunc systemFor(tools []string) string {\n\treturn tools[0]\n}\n")
	write("runtime.go", "package x\n\nfunc rebuild() string {\n\treturn systemFor(nil)\n}\n")

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetDiagnosticWait(15 * time.Second)

	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()

	sess := &mcpSession{
		t: t, srv: srv, srvIn: cOut,
		clientR: json.NewDecoder(cIn), clientW: cOut, done: done,
	}
	defer sess.close()

	sess.request("initialize", map[string]any{})
	sess.notify("notifications/initialized", map[string]any{})

	// Proactive open pins BOTH files as overlays at the one-parameter
	// version. This is the state the bug needs.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.WaitForProactiveOpen(ctx); err != nil {
		t.Fatalf("proactive open: %v", err)
	}

	// The rebase. Neither write goes through the tool.
	write("harness.go", "package x\n\nfunc systemFor(tools []string, exec string, wt int) string {\n\t_, _ = exec, wt\n\treturn tools[0]\n}\n")
	write("runtime.go", "package x\n\nfunc rebuild() string {\n\treturn systemFor(nil, \"\", 0)\n}\n")

	// Let the watcher's debounce fire and the notification land.
	pushed := func(name string) bool {
		abs := filepath.Join(dir, name)
		data, err := os.ReadFile(abs)
		if err != nil {
			return false
		}
		return srv.sentContentMatches(pathToURI(abs), data)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !pushed("harness.go") {
		time.Sleep(50 * time.Millisecond)
	}
	if !pushed("harness.go") {
		t.Fatal("watcher never pushed the out-of-band harness.go to the child LSP")
	}

	// Now edit runtime.go through the tool, the way the session did.
	// Appending a comment changes nothing semantically: any error here is
	// the child answering from a version of harness.go that no longer exists.
	r := sess.callTool("node_edit", map[string]any{
		"node":    "runtime.go#rebuild",
		"oldText": "func rebuild() string {",
		"newText": "// rebuild builds.\nfunc rebuild() string {",
	})
	if r.IsError {
		t.Fatalf("node_edit errored: %+v", r.Content)
	}
	var payload struct {
		DiagnosticsAvailable bool `json:"diagnosticsAvailable"`
		DiagnosticsTimedOut  bool `json:"diagnosticsTimedOut"`
		Diagnostics          []struct {
			File    string `json:"file"`
			Message string `json:"message"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal([]byte(r.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode: %v (%s)", err, r.Content[0].Text)
	}
	if !payload.DiagnosticsAvailable {
		t.Fatalf("diagnosticsAvailable=false; payload=%+v", payload)
	}
	for _, d := range payload.Diagnostics {
		if strings.Contains(d.Message, "too many arguments") ||
			strings.Contains(d.Message, "not enough arguments") {
			t.Fatalf("stale signature from before the out-of-band rewrite: %s: %s\n"+
				"(timedOut=%v) — the child is still holding the pre-rewrite overlay",
				d.File, d.Message, payload.DiagnosticsTimedOut)
		}
	}
}

// TestSelfWritesAreNotEchoedToTheChild guards the other half: fsnotify fires on
// the tool's own writes too, ~200ms after each edit. Re-pushing content the
// child already has costs a round-trip per edit and dribbles extra publishes
// into the next call's settle window, so the hash guard must drop them.
func TestSelfWritesAreNotEchoedToTheChild(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	body := []byte("package x\n\nfunc A() {}\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	uri := pathToURI(path)

	// Nothing pushed yet: an out-of-band change is genuinely new.
	if srv.sentContentMatches(uri, body) {
		t.Error("a URI that was never pushed must not read as already-sent")
	}
	// No manager: notifyChildOfExternalChange must be a no-op rather than a
	// panic when the workspace has no LSP at all (markdown/yaml workspaces).
	srv.notifyChildOfExternalChange(path, body)

	// After a notification records the content, the same bytes are dropped —
	// this is what makes the ~200ms fsnotify echo of our own write free — and
	// different bytes still get through.
	srv.recordSent(uri, body)
	if !srv.sentContentMatches(uri, body) {
		t.Error("identical content must be recognised as already-sent")
	}
	if srv.sentContentMatches(uri, []byte("package x\n")) {
		t.Error("changed content must not be treated as already-sent")
	}
}
