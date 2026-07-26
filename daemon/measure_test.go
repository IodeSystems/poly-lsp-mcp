//go:build measure

package daemon

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// TestMeasureWorktreeOpen quantifies step-6's cost/benefit: how much a
// worktree open actually costs, split into the index build (what a true COW
// overlay would optimize) vs gopls warmup (which a COW cannot touch — gopls
// binds per module root). Run with:
//   go test ./daemon/ -tags measure -run TestMeasureWorktreeOpen -v -count=1
func TestMeasureWorktreeOpen(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.LoadOrDefault("nonexistent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// 1) Cold parent build (empty cache) — a from-scratch index.
	c1 := symbols.NewParseCache()
	t0 := time.Now()
	idx, err := symbols.Build(repoRoot, reg, symbols.WithCache(c1))
	if err != nil {
		t.Fatal(err)
	}
	coldParent := time.Since(t0)
	names, cacheLen := len(idx.Names()), c1.Len()

	// 2) A git worktree of the repo (detached at HEAD).
	wt := filepath.Join(t.TempDir(), "wt")
	run("-C", repoRoot, "worktree", "add", "--detach", wt, "HEAD")
	defer run("-C", repoRoot, "worktree", "remove", "--force", wt)

	// 3) Warm worktree build reusing the parent's cache — the residual a COW
	//    would attack (walk + read + hash + binding passes; parse is cached).
	t0 = time.Now()
	if _, err := symbols.Build(wt, reg, symbols.WithCache(c1)); err != nil {
		t.Fatal(err)
	}
	warmWorktree := time.Since(t0)

	// 4) Cold worktree build with a FRESH cache — the cost WITHOUT sharing.
	c2 := symbols.NewParseCache()
	t0 = time.Now()
	if _, err := symbols.Build(wt, reg, symbols.WithCache(c2)); err != nil {
		t.Fatal(err)
	}
	coldWorktree := time.Since(t0)

	// 5) gopls warmup LOWER BOUND: spawn + LSP initialize (Manager.Start
	//    blocks through the child's initialize). Real warmth (first correct
	//    cross-file definition) is longer; this only strengthens the point.
	var goplsStart, goplsWarm time.Duration
	if _, err := exec.LookPath("gopls"); err == nil {
		mgr := multiplex.NewManager(reg)
		t0 = time.Now()
		if err := mgr.Start(context.Background(), wt, "file://"+wt, []string{"go"}); err != nil {
			t.Logf("gopls start error: %v", err)
		}
		goplsStart = time.Since(t0)

		// Real warmth: gopls answers workspace/symbol only once it has
		// indexed the workspace — the background load that blocks the first
		// cross-file precision query, NOT the initialize handshake above.
		child := mgr.RouteByURI("file://" + filepath.Join(wt, "daemon", "registry.go"))
		warmStart := time.Now()
		deadline := time.Now().Add(60 * time.Second)
		for child != nil && time.Now().Before(deadline) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			raw, err := child.Call(ctx, "workspace/symbol", map[string]any{"query": "RollbackSession"})
			cancel()
			if err == nil {
				s := string(raw)
				if len(s) > 2 && s != "null" && s != "[]" {
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		goplsWarm = time.Since(warmStart)
		_ = mgr.Shutdown(context.Background())
	} else {
		t.Log("gopls not on PATH — skipping warmup measurement")
	}

	t.Logf("indexed names=%d  cache entries=%d", names, cacheLen)
	t.Logf("cold parent build (empty cache):   %v", coldParent)
	t.Logf("warm worktree build (shared cache): %v   <- the COW residual (parse cached)", warmWorktree)
	t.Logf("cold worktree build (fresh cache):  %v", coldWorktree)
	t.Logf("shared cache already saves:         %v", coldWorktree-warmWorktree)
	t.Logf("gopls spawn+init (handshake only):  %v", goplsStart)
	t.Logf("gopls workspace warmth (to 1st workspace/symbol): %v   <- the dominant, UNSHAREABLE cost", goplsWarm)
}
