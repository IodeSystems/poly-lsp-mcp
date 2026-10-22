package mcp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/git"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// The build walk and the file WATCHER must agree on what is ignored.
// Without that they fight: Build excludes a gitignored file, then the
// next write to it puts the file straight back, so any session that
// runs a build — or switches branches — drifts back toward indexing
// everything it was meant to skip.
func TestWatcherHonoursGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	write := func(rel, body string) {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write(".gitignore", "generated/\n")
	write("go.mod", "module x\n\ngo 1.21\n")
	write("real.go", "package x\n\nfunc RealSym() {}\n")
	run("init", "-q")
	run("add", ".")
	run("commit", "-qm", "init")

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	s := New(reg, dir, nil, nil)
	if err := s.BuildIndex(); err != nil {
		t.Fatal(err)
	}

	// A write into a gitignored directory must NOT enter the index,
	// which is what the watcher's refresh path would otherwise do.
	write("generated/dump.go", "package x\n\nfunc GeneratedSym() {}\n")
	s.watchRefreshFile(filepath.Join(dir, "generated/dump.go"))
	if got := s.getIndex().Lookup("GeneratedSym"); len(got) != 0 {
		t.Errorf("the watcher re-indexed a gitignored file: %+v", got)
	}
	if !s.pathIgnored(filepath.Join(dir, "generated/dump.go")) {
		t.Error("pathIgnored should report the generated dir as ignored")
	}
	if s.pathIgnored(filepath.Join(dir, "real.go")) {
		t.Error("a tracked file must not be reported ignored")
	}

	// A branch switch that changes .gitignore must be picked up: the set
	// is re-resolved rather than frozen at build time.
	write(".gitignore", "other/\n")
	run("add", ".gitignore")
	run("commit", "-qm", "loosen")
	s.setIgnores(git.LoadIgnores(dir))
	if s.pathIgnored(filepath.Join(dir, "generated/dump.go")) {
		t.Error("after .gitignore changed, generated/ should no longer be ignored")
	}
}

// The proactive didOpen walk must skip gitignored paths: an ignored
// directory (dun's vendored go/ toolchain, 9,355 files) and an ignored
// file pattern. Each didOpen'd file costs gopls memory.
func TestProactiveOpenSkipsGitignored(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short")
	}
	for _, bin := range []string{"git", "gopls"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not on PATH")
		}
	}
	dir := t.TempDir()
	write := func(rel, body string) {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "generated/\n*.gen.go\n")
	write("go.mod", "module x\n\ngo 1.21\n")
	write("real.go", "package x\n\nfunc RealSym() {}\n")
	write("x.gen.go", "package x\n\nfunc GenSym() {}\n")
	write("generated/dump.go", "package x\n\nfunc DumpSym() {}\n")
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	sess := &mcpSession{
		t: t, srv: srv,
		srvIn: cOut, clientR: json.NewDecoder(cIn),
		clientW: cOut, done: done,
	}
	defer sess.close()
	sess.request("initialize", map[string]any{})
	sess.notify("notifications/initialized", map[string]any{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.WaitForProactiveOpen(ctx); err != nil {
		t.Fatal(err)
	}

	srv.openDocsMu.Lock()
	defer srv.openDocsMu.Unlock()
	var got []string
	for uri := range srv.openDocs {
		got = append(got, filepath.Base(uriToPath(uri)))
	}
	if len(got) != 1 || got[0] != "real.go" {
		t.Fatalf("proactive open sent didOpen for %v, want only [real.go]", got)
	}
}
