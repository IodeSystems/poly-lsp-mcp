//go:build measure

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// TestMeasureWorktreeOpen quantifies step-6's cost/benefit: how much a
// worktree open actually costs, split into the index build (what a true COW
// overlay would optimize) vs gopls warmup (which a COW cannot touch — gopls
// binds per module root). Run with:
//
//	go test ./daemon/ -tags measure -run TestMeasureWorktreeOpen -v -count=1
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

// TestMeasureWorktreeSharingFleet is TestMeasureWorktreeSharing under the
// child-LSP fleet at the daemon's defaults (multiplex.Fleet: MaxChildren 4,
// RSSBudgetPct 25, IdleStop 10m), which the issue's measurements predate:
// does the fleet change what two worktrees of one repo cost — does it evict
// the first gopls when the second opens, and at what cost to restart it?
// Run with:
//
//	go test ./daemon/ -tags measure -run TestMeasureWorktreeSharingFleet -v -count=1
func TestMeasureWorktreeSharingFleet(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}
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
	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", repoRoot, "worktree", "add", "--detach", wt, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("worktree: %v\n%s", err, out)
	}
	defer func() { _ = exec.Command("git", "-C", repoRoot, "worktree", "remove", "--force", wt).Run() }()

	r := NewRegistry(cfg, reg, false, false)
	r.cachePath = "" // cold: no ~/.poly-lsp/cache.gob
	r.SetRootLifetime(time.Hour, -1)
	// The daemon's lifecycle defaults (cmd_mcp.go lifecycleFlags): the
	// fleet's process-wide rules, applied to every root's Manager.
	r.SetLifecycle(multiplex.Lifecycle{
		IdleStop:     10 * time.Minute,
		MaxRSS:       multiplex.DefaultMaxRSS(multiplex.TotalRAM()),
		PSIThreshold: 20,
		MaxChildren:  4,
		RSSBudgetPct: 25,
	})
	defer r.Close()

	query := func(srv *mcp.Server) {
		// A resolved query: the root's gopls loads its workspace.
		if _, _, err := srv.CallTool("", "node_query", json.RawMessage(`{"selector":"#RollbackSession::in"}`), mcp.CallOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// children reads the root's OWN child-LSP manager: the authoritative
	// per-child state, RSS and stop counters (a ps listing cannot tell
	// which gopls serves which root).
	children := func(srv *mcp.Server) {
		mgr := srv.Manager()
		if mgr == nil {
			t.Log("  (no child-LSP manager)")
			return
		}
		for _, cs := range mgr.Status() {
			if cs.Language != "go" {
				continue
			}
			t.Logf("  go child at %s: state=%s rss=%d MiB starts=%d capStops=%d budgetStops=%d lastStop=%s",
				cs.Root, cs.State, cs.RSS>>20, cs.Starts, cs.CapStops, cs.BudgetStops, cs.LastStop)
		}
	}
	fleet := func(label string) {
		fs := r.fleet.Status()
		t.Logf("%s: fleet running=%d cap=%d rss=%d MiB budget=%d MiB", label, fs.Running, fs.MaxChildren, fs.RSS>>20, fs.RSSBudget>>20)
	}
	goChild := func(srv *mcp.Server) (state string, rss int64, starts, capStops, budgetStops int) {
		for _, cs := range srv.Manager().Status() {
			if cs.Language == "go" {
				return cs.State, cs.RSS, cs.Starts, cs.CapStops, cs.BudgetStops
			}
		}
		return "none", 0, 0, 0, 0
	}

	t0 := time.Now()
	srvA, err := r.Get(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("root A (repo) open: %v", time.Since(t0))
	query(srvA)
	fleet("after A's query")
	children(srvA)

	t0 = time.Now()
	srvB, err := r.Get(wt)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("root B (worktree) open: %v", time.Since(t0))
	query(srvB)
	fleet("after B's query")
	children(srvA)
	children(srvB)
	stA, rssA, startsA, capA, budA := goChild(srvA)
	stB, rssB, _, capB, budB := goChild(srvB)
	t.Logf("gopls A: %s rss=%d MiB (starts=%d capStops=%d budgetStops=%d)", stA, rssA>>20, startsA, capA, budA)
	t.Logf("gopls B: %s rss=%d MiB (capStops=%d budgetStops=%d)", stB, rssB>>20, capB, budB)
	evicted := capA+capB+budA+budB > 0 || stA != "running"
	t.Logf("fleet evicted a gopls to make room: %v", evicted)

	// Does the fleet restart what it stopped? Re-query A and time it:
	// spawn+initialize+workspace load if A's child was stopped, near-instant
	// if it is still running.
	t0 = time.Now()
	query(srvA)
	t.Logf("re-query A: %v (includes gopls wake if the fleet stopped it)", time.Since(t0))
	fleet("after A re-query")
	children(srvA)
}

// TestMeasureWorktreeSharing: what one daemon shares between two roots that
// are worktrees of the same repository, and what it holds twice. Two roots
// in one Registry, as the daemon hosts them: time to open, parse-cache
// entries added, Go heap retained (after GC), and the child LSPs (count,
// RSS) once a resolved query has made each load its workspace. Run with:
//
//	go test ./daemon/ -tags measure -run TestMeasureWorktreeSharing -v -count=1
func TestMeasureWorktreeSharing(t *testing.T) {
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
	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", repoRoot, "worktree", "add", "--detach", wt, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("worktree: %v\n%s", err, out)
	}
	defer func() { _ = exec.Command("git", "-C", repoRoot, "worktree", "remove", "--force", wt).Run() }()

	r := NewRegistry(cfg, reg, false, false)
	r.cachePath = "" // cold: no ~/.poly-lsp/cache.gob
	r.SetRootLifetime(time.Hour, -1)
	defer r.Close()

	heap := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	h0 := heap()
	open := func(root string) (time.Duration, int, uint64) {
		before, c0 := heap(), r.cache.Len()
		start := time.Now()
		srv, err := r.Get(root)
		if err != nil {
			t.Fatal(err)
		}
		took := time.Since(start)
		// A resolved query: each root's gopls loads its workspace.
		if _, _, err := srv.CallTool("", "node_query", json.RawMessage(`{"selector":"#RollbackSession::in"}`), mcp.CallOptions{}); err != nil {
			t.Fatal(err)
		}
		return took, r.cache.Len() - c0, heap() - before
	}
	tA, cA, hA := open(repoRoot)
	tB, cB, hB := open(wt)
	t.Logf("root A (repo):     open %v, +%d cache entries, +%d MiB heap", tA.Round(time.Millisecond), cA, hA>>20)
	t.Logf("root B (worktree): open %v, +%d cache entries, +%d MiB heap", tB.Round(time.Millisecond), cB, hB>>20)
	t.Logf("cache total %d entries; heap over baseline %d MiB", r.cache.Len(), (heap()-h0)>>20)

	out, _ := exec.Command("ps", "-o", "pid=,rss=,args=", "--ppid", strconv.Itoa(os.Getpid())).Output()
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.Contains(line, "gopls") {
			n++
			t.Logf("child LSP: %s", strings.TrimSpace(line))
		}
	}
	t.Logf("gopls processes: %d (one per root)", n)
}
