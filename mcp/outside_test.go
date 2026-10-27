package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const outsideSrc = "package scratch\n\nfunc Keep() int {\n\treturn 1\n}\n\nfunc Other() {}\n"

// outsideSession serves a Go workspace with allow as the only directory
// outside the root (exclude removed from it), validation on.
func outsideSession(t *testing.T, allow, exclude []string) *mcpSession {
	t.Helper()
	s := startSessionWith(t, goWorkspace(t, jailSrc), nil, nil, func(srv *Server) {
		srv.SetPathPolicy(allow, exclude)
		srv.SetValidateEdits(true)
	})
	t.Cleanup(s.close)
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	return s
}

func decodeTool(t *testing.T, r toolResp) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Content[0].Text), &m); err != nil {
		t.Fatalf("decode %q: %v", r.Content[0].Text, err)
	}
	return m
}

// A file of ours in an allowed directory outside the root reads whole and
// by symbol, and edits by symbol and whole, keeping its mode; the address
// is the absolute path.
func TestOutsideFileReadAndEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scratch.go")
	if err := os.WriteFile(path, []byte(outsideSrc), 0o640); err != nil {
		t.Fatal(err)
	}
	s := outsideSession(t, []string{dir}, nil)

	r := s.callTool("node_read", map[string]any{"node": path})
	if r.IsError || !strings.Contains(r.Content[0].Text, "func Keep") || !strings.Contains(r.Content[0].Text, "outsideRoot") {
		t.Fatalf("whole read: isErr=%v %s", r.IsError, r.Content[0].Text)
	}
	m := decodeTool(t, s.callTool("node_read", map[string]any{"node": path + "#Keep"}))
	if m["node"] != path+"#Keep" || !strings.Contains(m["text"].(string), "return 1") || strings.Contains(m["text"].(string), "Other") {
		t.Fatalf("symbol read: %v", m)
	}

	m = decodeTool(t, s.callTool("node_read", map[string]any{"node": path + "@4"}))
	if m["node"] != path+"@4" || !strings.Contains(m["text"].(string), "return 1") {
		t.Fatalf("site read: %v", m)
	}
	m = decodeTool(t, s.callTool("node_edit", map[string]any{"node": path + "#Keep", "oldText": "return 1", "newText": "return 2"}))
	if m["validation"] != "parse-only" || m["parsesCleanly"] != true || m["node"] != path+"#Keep" {
		t.Fatalf("symbol edit: %v", m)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "return 2") || !strings.Contains(string(got), "func Other() {}") {
		t.Fatalf("after symbol edit: %q", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Errorf("mode %v after edit, want 0640", info.Mode().Perm())
	}

	// An edit that stops the file parsing is reverted (parse check).
	r = s.callTool("node_edit", map[string]any{"node": path, "oldText": "func Other() {}", "newText": "func Other( {"})
	m = decodeTool(t, r)
	if !r.IsError || m["rejected"] != true || m["reverted"] != true {
		t.Fatalf("parse-breaking edit not reverted: %v", m)
	}
	if after, _ := os.ReadFile(path); string(after) != string(got) {
		t.Fatalf("not restored: %q", after)
	}

	// Create in a directory we own.
	created := filepath.Join(dir, "new.go")
	m = decodeTool(t, s.callTool("node_edit", map[string]any{"node": created, "newText": "package scratch\n\nfunc New() {}\n"}))
	if m["created"] != true {
		t.Fatalf("create: %v", m)
	}
	if b, _ := os.ReadFile(created); !strings.Contains(string(b), "func New") {
		t.Fatalf("created file: %q", b)
	}

	// Workspace-only ops are refused.
	r = s.callTool("node_edit", map[string]any{"node": path + "#Keep", "rename": "Kept"})
	if !r.IsError || !strings.Contains(r.Content[0].Text, "workspace-only") {
		t.Errorf("rename outside the root: %s", r.Content[0].Text)
	}
	r = s.callTool("node_edit", map[string]any{"node": path, "oldText": "return 2", "newText": "return 3", "commit": false})
	if !r.IsError || !strings.Contains(r.Content[0].Text, "batch") {
		t.Errorf("commit:false outside the root: %s", r.Content[0].Text)
	}
}

func TestOutsideFileRefusals(t *testing.T) {
	dir := t.TempDir()
	excluded := filepath.Join(dir, "private")
	sub := filepath.Join(dir, "sub")
	for _, d := range []string{excluded, sub} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string) {
		if err := os.WriteFile(p, []byte(outsideSrc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(sub, "real.go"))
	write(filepath.Join(excluded, "hidden.go"))
	if err := os.Symlink(sub, filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(sub, "real.go"), filepath.Join(dir, "link.go")); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "pipe.go")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.go")
	write(elsewhere)

	s := outsideSession(t, []string{dir}, []string{excluded})
	cases := []struct {
		name, node, want string
	}{
		{"symlink dir component", filepath.Join(dir, "linkdir", "real.go"), "symlink"},
		{"symlink file", filepath.Join(dir, "link.go"), "symlink"},
		{"fifo", fifo, "not a regular file"},
		{"excluded dir", filepath.Join(excluded, "hidden.go"), "escapes the workspace root"},
		{"outside every dir", elsewhere, "escapes the workspace root"},
	}
	for _, c := range cases {
		for _, tool := range []string{"node_read", "node_edit"} {
			args := map[string]any{"node": c.node}
			if tool == "node_edit" {
				args["oldText"], args["newText"] = "return 1", "return 9"
			}
			r := s.callTool(tool, args)
			if !r.IsError || !strings.Contains(r.Content[0].Text, c.want) {
				t.Errorf("%s %s: isErr=%v %q, want an error naming %q", c.name, tool, r.IsError, r.Content[0].Text, c.want)
			}
		}
	}
	if b, _ := os.ReadFile(filepath.Join(sub, "real.go")); string(b) != outsideSrc {
		t.Error("an edit through a symlink reached its target")
	}
	// No create in a directory another user owns (/ is root's).
	if os.Geteuid() != 0 {
		s2 := outsideSession(t, []string{"/"}, nil)
		r := s2.callTool("node_edit", map[string]any{"node": "/poly-lsp-mcp-probe.go", "newText": "package x\n"})
		if !r.IsError || !strings.Contains(r.Content[0].Text, "owned by this user") {
			t.Errorf("create in a root-owned dir: %q", r.Content[0].Text)
		}
	}
}

// Only the user's own files: a file another uid owns is refused. Read
// only — the test never writes there. Skips without such a file.
func TestOutsideFileForeignOwnerRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns the candidate files")
	}
	var foreign string
	for _, p := range []string{"/etc/hostname", "/etc/hosts", "/etc/passwd"} {
		if info, err := os.Lstat(p); err == nil && info.Mode().IsRegular() && !ownedByUs(info) {
			foreign = p
			break
		}
	}
	if foreign == "" {
		t.Skip("no regular file owned by another user found")
	}
	s := outsideSession(t, []string{filepath.Dir(foreign)}, nil)
	r := s.callTool("node_read", map[string]any{"node": foreign})
	if !r.IsError || !strings.Contains(r.Content[0].Text, "owned by another user") {
		t.Fatalf("read of %s: isErr=%v %q", foreign, r.IsError, r.Content[0].Text)
	}
}

// The file-access hook sees an outside path, op and all, as it does an
// in-root one.
func TestOutsideFileHookAndFileAccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scratch.go")
	if err := os.WriteFile(path, []byte(outsideSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	hookDir := t.TempDir()
	hook := filepath.Join(hookDir, "hook.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho \"$1 $2\"; exit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var srv *Server
	s := startSessionWith(t, goWorkspace(t, jailSrc), nil, nil, func(sv *Server) {
		srv = sv
		sv.SetPathPolicy([]string{dir}, nil)
		sv.SetFileAccessHook(&FileHook{Path: hook})
	})
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	if r := s.callTool("node_read", map[string]any{"node": path}); r.Content[0].Text != "[hook] read "+path {
		t.Errorf("read: %q", r.Content[0].Text)
	}
	if r := s.callTool("node_edit", map[string]any{"node": path, "oldText": "return 1", "newText": "return 2"}); r.Content[0].Text != "[hook] edit "+path {
		t.Errorf("edit: %q", r.Content[0].Text)
	}
	if b, _ := os.ReadFile(path); string(b) != outsideSrc {
		t.Error("a cancelled outside edit was applied")
	}
	args, _ := json.Marshal(map[string]any{"node": filepath.Join(dir, "new.go"), "newText": "package x\n"})
	if op, abs, ok := srv.FileAccess("node_edit", args); !ok || op != "create" || abs != filepath.Join(dir, "new.go") {
		t.Errorf("FileAccess create = %q %q %v", op, abs, ok)
	}
	// A daemon connection whose lists leave the dir out reaches no file.
	args, _ = json.Marshal(map[string]any{"node": path})
	if _, _, ok := srv.FileAccessAs("other", "node_read", args, CallOptions{AllowPaths: []string{}}); ok {
		t.Error("FileAccessAs: a connection with no allowed dirs reached an outside file")
	}
}
