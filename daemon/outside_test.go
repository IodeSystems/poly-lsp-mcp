package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/mcp"
)

// Access outside the root is the CONNECTION's: the proxy resolves its
// defaults with its own $TMPDIR (the daemon's may differ: a per-user
// /var/folders on macOS) and sends the lists; the daemon's own temp dir
// opens nothing. The client's hook covers those paths too.
func TestProxyAllowPathIsPerConnection(t *testing.T) {
	root, resolved := goWorkspace(t)
	c, stop := startTestDaemon(t, resolved)
	defer stop()

	clientTmp := filepath.Join(t.TempDir(), "client-tmpdir")
	if err := os.Mkdir(clientTmp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", clientTmp)
	scratch := filepath.Join(clientTmp, "scratch.go")
	if err := os.WriteFile(scratch, []byte("package scratch\n\nfunc S() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if mcp.DefaultAllowPaths()[0] != clientTmp {
		t.Fatalf("DefaultAllowPaths = %v, want $TMPDIR first", mcp.DefaultAllowPaths())
	}

	// A connection with no lists (an older proxy) reaches nothing outside.
	bare := NewClient(c.socket)
	if _, err := bare.Open(root); err != nil {
		t.Fatal(err)
	}
	content, isErr, err := bare.Call(root, "node_read", []byte(`{"node":"`+scratch+`"}`))
	if err != nil || !isErr || !strings.Contains(content[0].Text, "escapes the workspace root") {
		t.Fatalf("a connection with no lists read %s: isErr=%v err=%v %v", scratch, isErr, err, content)
	}

	// The proxy's own lists, with its $TMPDIR, through the hook.
	hookDir := t.TempDir()
	logPath := filepath.Join(hookDir, "hook.log")
	hook := filepath.Join(hookDir, "hook.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho \"$1 $2\" >> '"+logPath+"'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.SetPathPolicy(append(mcp.DefaultAllowPaths(), "/nonexistent-allowed"), []string{"/tmp/nothing-here"})
	h := startProxyHook(t, c, root, OpenOptions{}, &mcp.FileHook{Path: hook})
	h.reply(h.request("initialize", map[string]any{}), 5*time.Second)
	call := func(name string, args map[string]any) (string, bool) {
		return callText(t, h.reply(h.request("tools/call", map[string]any{"name": name, "arguments": args}), 30*time.Second))
	}
	if text, isErr := call("node_read", map[string]any{"node": scratch + "#S"}); isErr || !strings.Contains(text, "return 1") {
		t.Fatalf("read through the proxy: isErr=%v %s", isErr, text)
	}
	if text, isErr := call("node_edit", map[string]any{"node": scratch, "oldText": "return 1", "newText": "return 2"}); isErr || !strings.Contains(text, "parse-only") {
		t.Fatalf("edit through the proxy: isErr=%v %s", isErr, text)
	}
	if b, _ := os.ReadFile(scratch); !strings.Contains(string(b), "return 2") {
		t.Fatalf("edit did not land: %q", b)
	}
	b, _ := os.ReadFile(logPath)
	want := "read " + scratch + "\nedit " + scratch
	if strings.TrimSpace(string(b)) != want {
		t.Errorf("hook calls:\n%s\nwant:\n%s", b, want)
	}

	// The same connection excluding its $TMPDIR is refused.
	c.SetPathPolicy(mcp.DefaultAllowPaths(), []string{clientTmp})
	content, isErr, err = c.Call(root, "node_read", []byte(`{"node":"`+scratch+`"}`))
	if err != nil || !isErr || !strings.Contains(content[0].Text, "escapes the workspace root") {
		t.Fatalf("excluded $TMPDIR still read: isErr=%v err=%v %v", isErr, err, content)
	}
}
