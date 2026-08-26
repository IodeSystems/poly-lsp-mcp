package mcp

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	c := hookBlockedResult(got)
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
