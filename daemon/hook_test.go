package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/mcp"
)

// The proxy runs the client's file-access hook, the contract of
// mcp/hook.go: exit 2 cancels the call and its stdout is the answer; any
// other exit lets the call through, its stdout ignored. The hook gets the
// op and absolute path the in-process guard would give it, and runs with
// the proxy's env.
func TestProxyRunsTheFileAccessHook(t *testing.T) {
	root, resolved := goWorkspace(t)
	c, stop := startTestDaemon(t, resolved)
	defer stop()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "hook.log")
	hook := filepath.Join(dir, "hook.sh")
	script := `#!/bin/sh
echo "$1 $2 $HOOK_ENV" >> '` + logPath + `'
case "$(basename "$2")" in
f1.go) echo "read AGENTS.md first"; exit 2;;
f2.go) exit 2;;
f3.go) echo "stdout of a hook that lets the call through"; exit 0;;
esac
exit 0
`
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOOK_ENV", "client-env")

	h := startProxyHook(t, c, root, OpenOptions{}, &mcp.FileHook{Path: hook})
	h.reply(h.request("initialize", map[string]any{}), 5*time.Second)
	call := func(name string, args map[string]any) (string, bool) {
		return callText(t, h.reply(h.request("tools/call", map[string]any{"name": name, "arguments": args}), 30*time.Second))
	}

	// exit 2 with stdout: cancelled, the note is the answer (not an error).
	text, isErr := call("node_read", map[string]any{"node": "f1.go"})
	if isErr || text != "[hook] read AGENTS.md first" {
		t.Errorf("f1.go: isErr=%v %q, want the hook's note", isErr, text)
	}
	// exit 2, no stdout: cancelled with the default note.
	if text, _ := call("node_read", map[string]any{"node": "f2.go"}); text != "[hook] operation cancelled by host hook" {
		t.Errorf("f2.go: %q", text)
	}
	// exit 0: through, stdout ignored.
	if text, isErr := call("node_read", map[string]any{"node": "f3.go"}); isErr || !strings.Contains(text, "func F3") || strings.Contains(text, "[hook]") {
		t.Errorf("f3.go: isErr=%v %q", isErr, text)
	}
	// An edit by symbol address is guarded on its file, and a cancelled
	// edit does not reach the daemon.
	text, _ = call("node_edit", map[string]any{"node": "f1.go#F1", "oldText": "return 1", "newText": "return 11"})
	if text != "[hook] read AGENTS.md first" {
		t.Errorf("edit f1.go#F1: %q", text)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "f1.go")); strings.Contains(string(b), "return 11") {
		t.Error("a cancelled edit was applied")
	}
	// A create names the create op.
	if _, isErr := call("node_edit", map[string]any{"node": "new.go", "newText": "package main\n"}); isErr {
		t.Error("create failed")
	}
	// node_query touches no file through the guard.
	call("node_query", map[string]any{"selector": "#F4"})

	b, _ := os.ReadFile(logPath)
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	want := []string{
		"read " + filepath.Join(resolved, "f1.go") + " client-env",
		"read " + filepath.Join(resolved, "f2.go") + " client-env",
		"read " + filepath.Join(resolved, "f3.go") + " client-env",
		"edit " + filepath.Join(resolved, "f1.go") + " client-env",
		"create " + filepath.Join(resolved, "new.go") + " client-env",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("hook calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
