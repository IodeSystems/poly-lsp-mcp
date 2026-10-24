package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// writeHookScript creates an executable hook that prints body then exits
// with the given code.
func writeHookScript(t *testing.T, body string, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("hook script is a unix shell one-liner")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s' \"$1 $2\" \"''\"\n" // args sanity, overwritten below
	_ = script
	content := "#!/bin/sh\n"
	if body != "" {
		content += "printf '%s' '" + body + "'\n"
	}
	content += "exit " + itoa(exitCode) + "\n"
	path := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	return path
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestHookExitZeroProceeds: a hook that exits 0 lets the operation through.
func TestHookExitZeroProceeds(t *testing.T) {
	h := &FileHook{Path: writeHookScript(t, "ignored", 0)}
	cancel, note := h.Invoke("read", "/tmp/whatever.go")
	if cancel {
		t.Fatalf("exit-0 hook must not cancel; got note %q", note)
	}
	if note != "" {
		t.Errorf("exit-0 hook must not carry a note; got %q", note)
	}
}

// TestHookExitTwoCancels: a hook that exits 2 cancels and returns its
// stdout as the note.
func TestHookExitTwoCancels(t *testing.T) {
	const note = "read AGENTS.md first"
	h := &FileHook{Path: writeHookScript(t, note, 2)}
	cancel, got := h.Invoke("edit", "/tmp/whatever.go")
	if !cancel {
		t.Fatal("exit-2 hook must cancel")
	}
	if got != note {
		t.Errorf("note = %q, want %q", got, note)
	}
	// The injected result is not an error and carries the marker prefix.
	c := HookBlockedResult(got)
	if len(c) != 1 || c[0].Type != "text" {
		t.Fatalf("bad content shape: %+v", c)
	}
	if !strings.HasPrefix(c[0].Text, hookNotePrefix) {
		t.Errorf("result must be prefixed %q: %q", hookNotePrefix, c[0].Text)
	}
	if !strings.Contains(c[0].Text, note) {
		t.Errorf("result must contain the note: %q", c[0].Text)
	}
}

// TestHookMissingBinaryNoop: a missing hook binary proceeds silently.
func TestHookMissingBinaryNoop(t *testing.T) {
	h := &FileHook{Path: filepath.Join(t.TempDir(), "does-not-exist")}
	cancel, note := h.Invoke("create", "/tmp/whatever.go")
	if cancel || note != "" {
		t.Fatalf("missing binary must be a no-op; got cancel=%v note=%q", cancel, note)
	}
}

// TestHookNotConfiguredNoop: no hook at all behaves as today.
func TestHookNotConfiguredNoop(t *testing.T) {
	var h *FileHook
	if h.Cancelled() {
		t.Fatal("nil hook must report Cancelled()=false")
	}
	if cancel, _ := h.Invoke("read", "/tmp/whatever.go"); cancel {
		t.Fatal("nil hook must never cancel")
	}
	h2 := &FileHook{}
	if h2.Cancelled() {
		t.Fatal("empty-path hook must report Cancelled()=false")
	}
}

// TestHookNonZeroExitProceeds: any non-2 failure (e.g. exit 1) proceeds.
func TestHookNonZeroExitProceeds(t *testing.T) {
	h := &FileHook{Path: writeHookScript(t, "boom", 1)}
	if cancel, _ := h.Invoke("read", "/tmp/whatever.go"); cancel {
		t.Fatal("exit-1 hook must not cancel")
	}
}

// FileAccess (what the daemon proxy asks before running the hook) names
// the op and path the in-process guard hands the hook, call for call.
func TestFileAccessMatchesTheGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook script is a unix shell one-liner")
	}
	dir := goWorkspace(t, "package main\n\nfunc Keep() {}\n\nfunc Other() {}\n")
	logPath := filepath.Join(t.TempDir(), "hook.log")
	hook := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho \"$1 $2\" >> '"+logPath+"'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.LoadOrDefault("nonexistent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetQuiet(true)
	if err := srv.BringUp(false); err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	srv.SetFileAccessHook(&FileHook{Path: hook})

	calls := []struct{ name, args, op string }{
		{"node_read", `{"node":"main.go"}`, "read"},
		{"node_read", `{"node":"main.go#Keep"}`, "read"},
		{"node_read", `{"node":"missing.go"}`, ""},
		{"node_read", `{"node":"main.go@3"}`, "read"},
		{"node_edit", `{"node":"main.go#Other","oldText":"func Other() {}","newText":"func Other() { }"}`, "edit"},
		{"node_edit", `{"node":"main.go@3","newText":"func Keep() { }"}`, "edit"},
		{"node_edit", `{"node":"new.go","newText":"package main\n"}`, "create"},
		{"node_edit", `{"node":"new2.go","oldText":"","newText":"package main\n"}`, "create"},
		{"node_edit", `{"node":"main.go","newText":"package main\n"}`, ""}, // exists: refused, no guard
		{"node_edit", `{"node":"missing.go","oldText":"a","newText":"b"}`, ""},
		{"node_edit", `{"rollback":true}`, ""},
	}
	seen := 0
	for _, c := range calls {
		op, abs, ok := srv.FileAccess(c.name, json.RawMessage(c.args))
		if _, _, err := srv.CallTool("", c.name, json.RawMessage(c.args), CallOptions{}); err != nil {
			t.Logf("%s %s: %v", c.name, c.args, err)
		}
		b, _ := os.ReadFile(logPath)
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(b) == 0 {
			lines = nil
		}
		var got string
		if len(lines) > seen {
			got = lines[len(lines)-1]
		}
		seen = len(lines)
		want := ""
		if ok {
			want = op + " " + abs
		}
		if op != c.op {
			t.Errorf("%s %s: op %q, want %q", c.name, c.args, op, c.op)
		}
		if got != want {
			t.Errorf("%s %s: the guard asked %q, FileAccess says %q", c.name, c.args, got, want)
		}
	}
}
