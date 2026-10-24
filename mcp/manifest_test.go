package mcp

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// In a git repository the index takes git's file set, and a worktree of the
// repo under .claude/worktrees/ — dun's 48 — is left out from the start,
// with its reason in the report. node_query skips it too, unless a selector
// names a path inside it.
func TestWorktreeLeftOutByGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := writeTree(t, map[string]string{"app.go": "package p\n\nfunc App() {}\n"})
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-qm", "init")
	git("worktree", "add", "-q", filepath.Join(dir, ".claude", "worktrees", "a1"))

	s := startSession(t, dir)
	defer s.close()
	s.request("initialize", map[string]any{})

	rep := s.srv.IndexCost()
	if rep.Files != 1 {
		t.Errorf("indexed %d files, want 1 (the worktree's copy left out)", rep.Files)
	}
	found := false
	for _, l := range rep.LeftOut {
		if l.Dir == ".claude/worktrees/a1" && l.Reason == "worktree of this repo" {
			found = true
		}
	}
	if !found {
		t.Errorf("left out should name the worktree: %+v", rep.LeftOut)
	}
	if !strings.Contains(rep.Text(), "worktree of this repo") {
		t.Errorf("text report should say so:\n%s", rep.Text())
	}

	text := func(sel string) string {
		r := s.callTool("node_query", map[string]any{"selector": sel, "limit": 50})
		return r.Content[0].Text
	}
	if got := text("#App"); strings.Contains(got, ".claude/worktrees") {
		t.Errorf("a broad query reached the worktree:\n%s", got)
	}
	if got := text("file[path=.claude/worktrees/a1/app.go] > *"); !strings.Contains(got, "App") {
		t.Errorf("naming a path inside the worktree reaches it:\n%s", got)
	}
}
