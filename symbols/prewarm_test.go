package symbols_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/git"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

func newPrewarmRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@e")
	run("config", "user.name", "t")
	run("config", "commit.gpgSign", "false")
	return dir
}

func commit(t *testing.T, dir, msg string, files map[string]string) {
	t.Helper()
	for path, body := range files {
		full := filepath.Join(dir, path)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-q", "-m", msg},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestPrewarmFromBranchSeedsCache(t *testing.T) {
	dir := newPrewarmRepo(t)
	commit(t, dir, "init", map[string]string{
		"main.go":  "package main\n\nfunc Hello() {}\n",
		"util.go":  "package main\n\nfunc Helper() {}\n",
		"README.md": "hi\n",
	})
	repo, err := git.FromCWD(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	cache := symbols.NewParseCache()

	fresh, err := symbols.PrewarmFromBranch(repo, "main", reg, cache)
	if err != nil {
		t.Fatal(err)
	}
	if fresh < 3 {
		t.Errorf("fresh parses = %d, want >= 3 (main.go + util.go + README.md)", fresh)
	}
	// Re-run: same content → all hits.
	again, err := symbols.PrewarmFromBranch(repo, "main", reg, cache)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("rerun parsed %d files; expected all cache hits", again)
	}
}

func TestPrewarmFromBranchDeduplicatesIdenticalContent(t *testing.T) {
	dir := newPrewarmRepo(t)
	// Two files with byte-identical content → cache should see them
	// as the same entry (content-addressed).
	commit(t, dir, "init", map[string]string{
		"a.go": "package main\n\nfunc Same() {}\n",
		"b.go": "package main\n\nfunc Same() {}\n",
	})
	repo, _ := git.FromCWD(dir)
	reg, _ := config.Default().Build()
	cache := symbols.NewParseCache()

	fresh, err := symbols.PrewarmFromBranch(repo, "main", reg, cache)
	if err != nil {
		t.Fatal(err)
	}
	if fresh != 1 {
		t.Errorf("fresh = %d, want 1 (identical content should hit once)", fresh)
	}
	if got := cache.Len(); got != 1 {
		t.Errorf("cache.Len = %d, want 1", got)
	}
}

func TestPrewarmFromBranchEnablesAncestorReuse(t *testing.T) {
	// The Phase 3 win: parsing main on demand BEFORE you check it
	// out means switching to it later is free.
	dir := newPrewarmRepo(t)
	commit(t, dir, "init", map[string]string{
		"shared.go": "package main\n\nfunc Shared() {}\n",
	})

	// Branch off and add a file only in feature; shared.go stays
	// identical on both branches.
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("checkout", "-q", "-b", "feature")
	commit(t, dir, "feature work", map[string]string{
		"feature.go": "package main\n\nfunc Feature() {}\n",
	})

	repo, _ := git.FromCWD(dir)
	reg, _ := config.Default().Build()
	cache := symbols.NewParseCache()

	// Prewarm main (ancestor) while sitting on feature.
	mainFresh, _ := symbols.PrewarmFromBranch(repo, "main", reg, cache)
	if mainFresh != 1 {
		t.Errorf("main fresh = %d, want 1 (shared.go)", mainFresh)
	}

	// Now prewarm feature. shared.go should hit (same content), only
	// feature.go is new.
	featureFresh, _ := symbols.PrewarmFromBranch(repo, "feature", reg, cache)
	if featureFresh != 1 {
		t.Errorf("feature fresh = %d, want 1 (only feature.go is new)", featureFresh)
	}
}
