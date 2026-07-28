package symbols

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// newGitRepo lays out a workspace with three Go files: one committed,
// one created but never `git add`-ed, and one under a gitignored
// directory.
func newGitRepo(t *testing.T) string {
	t.Helper()
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
	write(".gitignore", "generated/\n*.gen.go\n")
	write("go.mod", "module x\n\ngo 1.21\n")
	write("tracked.go", "package x\n\nfunc TrackedSym() {}\n")
	write("generated/dump.go", "package x\n\nfunc IgnoredDirSym() {}\n")
	write("thing.gen.go", "package x\n\nfunc IgnoredFileSym() {}\n")
	run("init", "-q")
	run("add", ".gitignore", "go.mod", "tracked.go")
	run("commit", "-qm", "init")
	// Created AFTER the commit and never staged: untracked, NOT ignored.
	write("brandnew.go", "package x\n\nfunc BrandNewSym() {}\n")
	return dir
}

func sitesFor(t *testing.T, dir string, opts ...BuildOption) map[string]int {
	t.Helper()
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Build(dir, reg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, n := range []string{"TrackedSym", "BrandNewSym", "IgnoredDirSym", "IgnoredFileSym"} {
		out[n] = len(idx.Lookup(n))
	}
	return out
}

// The safety property this whole design turns on: IGNORED is filtered,
// UNTRACKED is not. A file an agent just created and has not `git
// add`-ed must stay indexed, or the tool goes blind to its own work
// mid-task.
func TestBuildSkipsGitignoredButKeepsBrandNewFiles(t *testing.T) {
	got := sitesFor(t, newGitRepo(t))
	if got["TrackedSym"] == 0 {
		t.Error("a tracked file must be indexed")
	}
	if got["BrandNewSym"] == 0 {
		t.Error("an untracked-but-NOT-ignored file must still be indexed — " +
			"filtering on `git ls-files` would make an agent's own new work invisible")
	}
	if got["IgnoredDirSym"] != 0 {
		t.Error("a file under a gitignored directory must be skipped")
	}
	if got["IgnoredFileSym"] != 0 {
		t.Error("a file matching a gitignore pattern must be skipped")
	}
}

func TestWithoutGitignoreRestoresTheOldWalk(t *testing.T) {
	got := sitesFor(t, newGitRepo(t), WithoutGitignore())
	for _, n := range []string{"TrackedSym", "BrandNewSym", "IgnoredDirSym", "IgnoredFileSym"} {
		if got[n] == 0 {
			t.Errorf("WithoutGitignore should index everything; %s missing", n)
		}
	}
}

// A workspace that is not a git repository must index exactly as before
// — loadIgnoreSet returns nil and the walk is unfiltered.
func TestBuildUnfilteredOutsideAGitRepo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"),
		[]byte("package x\n\nfunc LooseSym() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Build(dir, reg)
	if err != nil {
		t.Fatalf("Build outside a repo must not fail: %v", err)
	}
	if len(idx.Lookup("LooseSym")) == 0 {
		t.Error("a non-git workspace must index unfiltered")
	}
}
