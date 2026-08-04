package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Each test sets up a fresh git repo in a tempdir, populates it,
// and exercises the Repo wrapper. Skipped when git is missing.

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "config", "user.email", "test@example.com")
	mustGit(t, dir, "config", "user.name", "test")
	mustGit(t, dir, "config", "commit.gpgSign", "false")
	return dir
}

func writeAndCommit(t *testing.T, dir string, files map[string]string, msg string) {
	t.Helper()
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-q", "-m", msg)
}

func TestFromCWDInRepo(t *testing.T) {
	dir := newRepo(t)
	writeAndCommit(t, dir, map[string]string{"README.md": "hi\n"}, "init")

	repo, err := FromCWD(dir)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Dir != dir {
		// Toplevel on macOS may go through /private/var symlink;
		// realpath both sides for the equality check.
		got, _ := filepath.EvalSymlinks(repo.Dir)
		want, _ := filepath.EvalSymlinks(dir)
		if got != want {
			t.Errorf("Dir = %q, want %q", repo.Dir, dir)
		}
	}
}

func TestFromCWDNotInRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	_, err := FromCWD(dir)
	if !errors.Is(err, ErrNotInRepo) {
		t.Errorf("err = %v, want ErrNotInRepo", err)
	}
}

func TestCurrentBranch(t *testing.T) {
	dir := newRepo(t)
	writeAndCommit(t, dir, map[string]string{"a.txt": "a"}, "init")

	repo, _ := FromCWD(dir)
	br, err := repo.CurrentBranch()
	if err != nil {
		t.Fatal(err)
	}
	if br != "main" {
		t.Errorf("CurrentBranch = %q, want main", br)
	}
}

func TestUpstreamAndAncestorChain(t *testing.T) {
	dir := newRepo(t)
	writeAndCommit(t, dir, map[string]string{"a.txt": "a\n"}, "init")

	// Build a 3-deep stack on top of main.
	mustGit(t, dir, "checkout", "-q", "-b", "feature/a")
	writeAndCommit(t, dir, map[string]string{"a2.txt": "a2\n"}, "feature/a work")
	mustGit(t, dir, "checkout", "-q", "-b", "feature/b")
	writeAndCommit(t, dir, map[string]string{"b.txt": "b\n"}, "feature/b work")
	mustGit(t, dir, "checkout", "-q", "-b", "feature/c")
	writeAndCommit(t, dir, map[string]string{"c.txt": "c\n"}, "feature/c work")

	// Configure each branch's upstream to point at the previous.
	mustGit(t, dir, "branch", "--set-upstream-to=main", "feature/a")
	mustGit(t, dir, "branch", "--set-upstream-to=feature/a", "feature/b")
	mustGit(t, dir, "branch", "--set-upstream-to=feature/b", "feature/c")

	repo, _ := FromCWD(dir)
	chain, err := repo.AncestorChain("feature/c", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"feature/b", "feature/a", "main"}
	if len(chain) != len(want) {
		t.Fatalf("chain = %v, want %v", chain, want)
	}
	for i := range want {
		if chain[i] != want[i] {
			t.Errorf("chain[%d] = %q, want %q", i, chain[i], want[i])
		}
	}

	// Branch with no upstream returns empty chain, no error.
	mustGit(t, dir, "checkout", "-q", "-b", "orphan")
	if got, err := repo.AncestorChain("orphan", 10); err != nil || len(got) != 0 {
		t.Errorf("orphan chain = %v, err=%v", got, err)
	}
}

func TestListFilesAndFileAt(t *testing.T) {
	dir := newRepo(t)
	writeAndCommit(t, dir, map[string]string{
		"a.txt":       "a",
		"sub/b.txt":   "b",
		"sub/c.txt":   "c",
		".gitignore":  "ignored.txt\n",
	}, "init")
	// Add an ignored file on disk; it should NOT show up in ls-tree.
	if err := os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo, _ := FromCWD(dir)
	files, err := repo.ListFiles("main")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		".gitignore": true,
		"a.txt":      true,
		"sub/b.txt":  true,
		"sub/c.txt":  true,
	}
	if len(files) != len(want) {
		t.Errorf("files = %v, want %v entries", files, len(want))
	}
	for _, f := range files {
		if !want[f] {
			t.Errorf("unexpected file %q in ls-tree", f)
		}
	}

	got, err := repo.FileAt("main", "sub/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "b" {
		t.Errorf("FileAt = %q, want b", got)
	}

	if _, err := repo.FileAt("main", "no-such.txt"); err == nil {
		t.Error("expected error for missing path")
	}
}

func TestUpstreamWithRemotePrefix(t *testing.T) {
	dir := newRepo(t)
	writeAndCommit(t, dir, map[string]string{"a.txt": "a"}, "init")

	// Set up a real "origin" remote pointing back at ourselves and
	// push main, so refs/remotes/origin/main becomes a valid
	// tracking ref git will accept as an upstream target.
	mustGit(t, dir, "remote", "add", "origin", dir)
	mustGit(t, dir, "fetch", "-q", "origin")

	mustGit(t, dir, "checkout", "-q", "-b", "feature")
	mustGit(t, dir, "branch", "--set-upstream-to=origin/main", "feature")

	repo, _ := FromCWD(dir)
	up, err := repo.UpstreamBranch("feature")
	if err != nil {
		t.Fatal(err)
	}
	// The remote prefix `origin/` should be stripped because a
	// local `main` exists.
	if up != "main" {
		t.Errorf("UpstreamBranch = %q, want main (stripped origin/)", up)
	}
}

// UnmergedPaths is git's own answer to "which files are mid-merge", which is
// exact where a scan for "<<<<<<<" is not: a marker inside a string literal,
// or a document about merge conflicts, is not an unmerged path.
func TestUnmergedPaths(t *testing.T) {
	dir := newRepo(t)
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("base\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-qm", "base")

	r := &Repo{Dir: dir}
	if got := r.UnmergedPaths(); len(got) != 0 {
		t.Fatalf("clean repo reports %v", got)
	}

	mustGit(t, dir, "checkout", "-qb", "feature")
	write("theirs\n")
	mustGit(t, dir, "commit", "-qam", "feature")
	mustGit(t, dir, "checkout", "-q", "main")
	write("mine\n")
	mustGit(t, dir, "commit", "-qam", "mine")
	// Expected to fail — that is the point.
	cmd := exec.Command("git", "merge", "feature")
	cmd.Dir = dir
	_ = cmd.Run()

	got := r.UnmergedPaths()
	if len(got) != 1 || got[0] != "f.txt" {
		t.Fatalf("UnmergedPaths = %v, want [f.txt]", got)
	}

	// A literal marker in tracked, merged content is NOT an unmerged path.
	mustGit(t, dir, "merge", "--abort")
	write("a line with <<<<<<< in it\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-qm", "markers as content")
	if got := r.UnmergedPaths(); len(got) != 0 {
		t.Errorf("a marker in committed content reported as unmerged: %v", got)
	}
}
