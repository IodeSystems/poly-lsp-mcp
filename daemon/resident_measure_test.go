//go:build measure

package daemon

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestMeasureRootResident: the Go heap one hosted root holds, against the
// bytes its index read — the factor rootResident uses. Two roots in one
// Registry: this repo with a cold parse cache (index + its cache entries),
// then a worktree of it (index alone: its parses are cache hits). Run with:
//
//	go test ./daemon/ -tags measure -run TestMeasureRootResident -v -count=1
func TestMeasureRootResident(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	cfg, reg := testRegistryDeps(t)
	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", repoRoot, "worktree", "add", "--detach", wt, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("worktree: %v\n%s", err, out)
	}
	defer func() { _ = exec.Command("git", "-C", repoRoot, "worktree", "remove", "--force", wt).Run() }()

	r := NewRegistry(cfg, reg, false, false)
	r.cachePath = ""
	defer r.Close()
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	for _, root := range []string{repoRoot, wt} {
		before := heap()
		srv, err := r.get(root, "", nil, false)
		if err != nil {
			t.Fatal(err)
		}
		grew := heap() - before
		b := srv.IndexCost().Bytes
		t.Logf("%s: %d files, %d KiB indexed, +%d KiB heap, factor %.1f, estimate %d KiB",
			root, srv.IndexCost().Files, b>>10, grew>>10, float64(grew)/float64(b), rootResident(srv)>>10)
	}
}
