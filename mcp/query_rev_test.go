package mcp

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRun runs a git command in dir and returns trimmed stdout, failing the
// test on a nonzero exit.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

// newGitRepo initializes a git repo in a temp dir, commits `files`, and
// returns the dir and the commit sha.
func newGitRepo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "t@t")
	gitRun(t, dir, "config", "user.name", "t")
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "one")
	return dir, gitRun(t, dir, "rev-parse", "HEAD")
}

// TestQueryRevReadsCommitWithoutCheckout is the F28 promise: given a git
// repository, a query using --rev at a commit reads the node tree from that
// commit, and no working-tree checkout of that commit is required.
//
// The setup makes the working tree DIFFER from the rev: commit 1 has two
// funcs (A and B), then the working tree (and commit 2) deletes B. A query
// with no rev sees only A (the working tree); the same query with rev at
// commit 1 sees A AND B — the tree came from the commit, not the checkout.
func TestQueryRevReadsCommitWithoutCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir, rev := newGitRepo(t, map[string]string{
		"app.go": "package p\n\nfunc A() {}\n\nfunc B() {}\n",
	})
	// Now delete B from the working tree and commit it, so the working
	// tree no longer matches `rev`.
	app := filepath.Join(dir, "app.go")
	if err := os.WriteFile(app, []byte("package p\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "two")

	// Working-tree query (no rev): B is gone.
	s := startSession(t, dir)
	defer s.close()
	s.request("initialize", map[string]any{})
	e, err := s.srv.buildTreeFor("func")
	if err != nil {
		t.Fatal(err)
	}
	list, _ := parseModernSelector("func")
	work := e.evaluate(list)
	if _, ok := funcNames(work)["B"]; ok {
		t.Fatal("working-tree query must NOT see B (it was deleted)")
	}

	// Same query with rev at commit 1: B is back.
	s.srv.SetQueryRev(rev)
	e2, err := s.srv.buildTreeFor("func")
	if err != nil {
		t.Fatal(err)
	}
	revRows := e2.evaluate(list)
	names := funcNames(revRows)
	if _, ok := names["A"]; !ok {
		t.Fatalf("--rev query missing A; got %v", names)
	}
	if _, ok := names["B"]; !ok {
		t.Fatalf("--rev query must read B from commit %s (working tree has no B); got %v", rev, names)
	}
}

// funcNames returns the set of func leaf names among the matched nodes.
func funcNames(rows []*treeNode) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		if r.class == "func" {
			out[r.leaf] = true
		}
	}
	return out
}
