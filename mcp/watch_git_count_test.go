package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// gitCounter puts a git on PATH that logs each call and runs the real
// one, so a test can count the git processes one event costs.
type gitCounter struct{ log string }

func newGitCounter(t *testing.T) *gitCounter {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell wrapper")
	}
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	bin := t.TempDir()
	c := &gitCounter{log: filepath.Join(bin, "calls")}
	script := "#!/bin/sh\necho \"$*\" >> '" + c.log + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return c
}

// calls returns the logged git argument lines and clears the log.
func (c *gitCounter) calls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(c.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(c.log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// newGitServer makes a git repo with a .gitignore and one Go file, and a
// built server over it.
func newGitServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range map[string]string{
		".gitignore": "generated/\n",
		"go.mod":     "module x\n\ngo 1.21\n",
		"a.go":       "package x\n\nfunc A() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	s := newIndexedServer(t, dir)
	if err := s.BuildIndex(); err != nil {
		t.Fatal(err)
	}
	return s, dir
}

// settle waits out a debounce timer of d.
func settle(d time.Duration) { time.Sleep(d + 150*time.Millisecond) }

// A Chmod-only event, on an indexed file or on .gitignore, starts no
// git process and schedules no re-read. On macOS kqueue reports one per
// attribute change on every file in a watched dir; acting on them ran
// the daemon at 200% CPU for 12 hours.
func TestWatchChmodOnlyCostsNoGit(t *testing.T) {
	c := newGitCounter(t)
	s, dir := newGitServer(t)
	c.calls(t)

	var mu sync.Mutex
	pending := map[string]*time.Timer{}
	for _, name := range []string{"a.go", ".gitignore", "go.mod"} {
		s.handleWatchEvent(nil, fsnotify.Event{Name: filepath.Join(dir, name), Op: fsnotify.Chmod}, pending, &mu)
	}
	if len(pending) != 0 {
		t.Errorf("Chmod scheduled %d refresh(es)", len(pending))
	}
	settle(gitignoreReloadDelay)
	if got := c.calls(t); len(got) != 0 {
		t.Errorf("Chmod-only events ran git %d time(s): %q", len(got), got)
	}

	// A Write still counts. The check-ignore answer is memoized, so the
	// second Write to the same file runs no git at all.
	a := filepath.Join(dir, "a.go")
	for i, wantMax := range []int{1, 0} {
		s.handleWatchEvent(nil, fsnotify.Event{Name: a, Op: fsnotify.Write}, pending, &mu)
		settle(fileWatchDebounce)
		if got := c.calls(t); len(got) > wantMax {
			t.Errorf("Write #%d ran git %d time(s), want <= %d: %q", i+1, len(got), wantMax, got)
		}
	}
}

// lsFiles counts the `git ls-files` runs (ignore-set re-resolves) in calls.
func lsFiles(calls []string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c, " ls-files ") {
			n++
		}
	}
	return n
}

// A burst of .gitignore events re-resolves the ignore set once.
func TestGitignoreBurstRunsGitOnce(t *testing.T) {
	c := newGitCounter(t)
	s, dir := newGitServer(t)
	gi := filepath.Join(dir, ".gitignore")
	var mu sync.Mutex
	pending := map[string]*time.Timer{}
	c.calls(t)
	for i := 0; i < 20; i++ {
		body := "generated/\n" + strings.Repeat("#\n", i)
		if err := os.WriteFile(gi, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		s.handleWatchEvent(nil, fsnotify.Event{Name: gi, Op: fsnotify.Write}, pending, &mu)
	}
	settle(gitignoreReloadDelay)
	if got := c.calls(t); lsFiles(got) != 1 || len(got) != 1 {
		t.Errorf("20 .gitignore writes ran git %q, want one ls-files", got)
	}
	if !s.pathIgnored(filepath.Join(dir, "generated", "x.go")) {
		t.Error("generated/ not ignored after reload")
	}
}

// A .gitignore event with unchanged content runs no git and keeps the
// check-ignore memo.
func TestGitignoreUnchangedRunsNoGit(t *testing.T) {
	c := newGitCounter(t)
	s, dir := newGitServer(t)
	gi := filepath.Join(dir, ".gitignore")
	a := filepath.Join(dir, "a.go")
	s.pathIgnored(a) // fills the memo
	set := s.getIgnores()
	var mu sync.Mutex
	pending := map[string]*time.Timer{}
	for round := 0; round < 2; round++ {
		// An editor's save that writes the same bytes. Round 0 is the
		// first event for the file: its mtime must predate the set.
		if err := os.WriteFile(gi, []byte("generated/\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if round == 0 {
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(gi, old, old); err != nil {
				t.Fatal(err)
			}
		}
		c.calls(t)
		s.handleWatchEvent(nil, fsnotify.Event{Name: gi, Op: fsnotify.Write}, pending, &mu)
		settle(gitignoreReloadDelay)
		if got := c.calls(t); len(got) != 0 {
			t.Errorf("round %d: unchanged .gitignore ran git %q", round, got)
		}
	}
	if s.getIgnores() != set {
		t.Error("ignore set replaced though nothing changed")
	}
	s.pathIgnored(a)
	if got := c.calls(t); len(got) != 0 {
		t.Errorf("check-ignore memo lost: %q", got)
	}

	// A real change still re-resolves.
	if err := os.WriteFile(gi, []byte("other/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.handleWatchEvent(nil, fsnotify.Event{Name: gi, Op: fsnotify.Write}, pending, &mu)
	settle(gitignoreReloadDelay)
	if got := c.calls(t); lsFiles(got) != 1 {
		t.Errorf("changed .gitignore ran git %q, want one ls-files", got)
	}
	if s.pathIgnored(filepath.Join(dir, "generated", "x.go")) {
		t.Error("generated/ still ignored after the change")
	}
}
