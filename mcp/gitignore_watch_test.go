package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/git"
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
