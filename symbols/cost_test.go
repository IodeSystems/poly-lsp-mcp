package symbols

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// The dun shape: many worktrees under one directory dwarf the project. The
// report must name the directory to ignore — .claude/worktrees/, not .claude/
// (no narrower) and not each worktree (none is heavy alone) — and say it
// holds other repositories.
func TestCostReportNamesTheDirectoryToIgnore(t *testing.T) {
	c := NewBuildCost()
	c.begin("/r")
	for i := 0; i < 40; i++ {
		wt := filepath.Join("/r/.claude/worktrees", "agent-"+string(rune('a'+i%26))+string(rune('a'+i/26)))
		c.nested[c.rel(wt)] = true
		// Only the first is parsed; the rest hit the cache, as on dun.
		cost := time.Millisecond
		if i == 0 {
			cost = 300 * time.Millisecond
		}
		c.file(filepath.Join(wt, "main.go"), 1000, cost, i > 0)
	}
	c.file("/r/main.go", 1000, 10*time.Millisecond, false)
	c.file("/r/cmd/x/x.go", 1000, 10*time.Millisecond, true)
	c.end()

	r := c.Report()
	if r.Files != 42 || r.Bytes != 42000 || r.Cached != 40 {
		t.Fatalf("totals: %+v", r)
	}
	if len(r.Heaviest) != 1 {
		t.Fatalf("want only .claude/worktrees/, got %+v", r.Heaviest)
	}
	h := r.Heaviest[0]
	if h.Dir != ".claude/worktrees/" || h.Files != 40 || h.NestedCheckouts != 40 || h.CostMs != 339 {
		t.Errorf("heaviest: %+v", h)
	}
	if h.Share < 0.95 || h.Share > 0.96 {
		t.Errorf("share %v, want 40/42", h.Share)
	}
	if !strings.Contains(r.Hint, ".gitignore") || !strings.Contains(r.Hint, "own .git") {
		t.Errorf("hint should name the cause and the remedy: %q", r.Hint)
	}
	if txt := r.Text(); !strings.Contains(txt, ".claude/worktrees/") || !strings.Contains(txt, "40 nested checkout(s)") {
		t.Errorf("text:\n%s", txt)
	}
}

// Shares are of bytes, so the parse cache does not decide what is heavy: a
// cached directory the same size as a parsed one ranks the same. A directory
// under the threshold is not named; nothing is listed inside a listed one.
func TestCostReportRanksByBytes(t *testing.T) {
	c := NewBuildCost()
	c.begin("/r")
	c.file("/r/src/a/a.go", 500, 90*time.Millisecond, false)
	c.file("/r/src/b/b.go", 460, time.Millisecond, true)
	c.file("/r/gen/g.go", 400, time.Millisecond, true)
	c.file("/r/tiny/t.go", 40, 9*time.Millisecond, false)
	c.end()
	var dirs []string
	for _, h := range c.Report().Heaviest {
		dirs = append(dirs, h.Dir)
	}
	if got := strings.Join(dirs, " "); got != "src/ gen/" {
		t.Errorf("heaviest = %q, want src/ (a and b inside it not repeated) then gen/", got)
	}
}

// Through a real Build: every indexed file is counted and a directory with
// its own .git is flagged, without being skipped.
func TestBuildRecordsCost(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main\n\nfunc main() {}\n")
	write("clone/.git", "gitdir: /elsewhere\n")
	write("clone/lib.go", "package lib\n\nfunc Lib() {}\n")
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	c := NewBuildCost()
	idx, err := Build(dir, reg, WithCost(c))
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Lookup("Lib")) == 0 {
		t.Error("a nested checkout is reported, not skipped")
	}
	r := c.Report()
	if r.Files != 2 {
		t.Errorf("files = %d, want 2", r.Files)
	}
	if !c.nested["clone"] {
		t.Errorf("clone/ has its own .git and should be flagged: %v", c.nested)
	}
}
