package isolation

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTwoConcurrentTempDirScanRuns proves D4 isolation: two concurrent
// go-test runs of the same collision-sensitive package do not interfere
// with each other when each gets its own TMPDIR, and DO collide when they
// share one.
//
// This is the attestation for spec sp_1m7vxgd0n869kcc3kmwrxg:
// "two worktrees of this project can run the suite at once and both pass."
//
// The collision: two worktrees of one project, run at once, share
// os.TempDir(). Each worktree's TestMain creates a prefixed temp dir
// (poly-lsp-mcp-srv-test-* for server, poly-lsp-mcp-mp-test-* for
// multiplex), and TestTempDirRemovedOnFailedRun scans the whole temp dir,
// failing if any new prefixed dir appears between its before/after
// snapshots. Two worktrees sharing /tmp each create their own such dirs,
// so each run sees the other's and fails. Distinct TMPDIR per run removes
// the collision.
func TestTwoConcurrentTempDirScanRuns(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}

	// Find the module root: this file is at internal/isolation/isolation_test.go,
	// so the root is two levels up.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller path")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	// runTest spawns `go test -run TestTempDirRemovedOnFailedRun <pkg>` with
	// the given TMPDIR. GOCACHE is left at its default (shared), which is
	// faithful to the real per-user scenario: Go's build cache is safe under
	// concurrent access (content-addressed, file-locked).
	runTest := func(tmpdir, pkg string) (string, error) {
		cmd := exec.Command("go", "test", "-count=1", "-timeout=90s",
			"-run", "TestTempDirRemovedOnFailedRun", pkg)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "TMPDIR="+tmpdir)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// runPair runs two concurrent `go test` invocations of the same package,
	// each with the given TMPDIR. This mirrors two worktrees of one project
	// running the same test at once.
	runPair := func(tmpdirA, tmpdirB, pkg string) (string, error, string, error) {
		var wg sync.WaitGroup
		var outA, outB string
		var errA, errB error

		wg.Add(2)
		go func() {
			defer wg.Done()
			outA, errA = runTest(tmpdirA, pkg)
		}()
		go func() {
			defer wg.Done()
			outB, errB = runTest(tmpdirB, pkg)
		}()
		wg.Wait()

		return outA, errA, outB, errB
	}

	// runPairStaggered is like runPair but delays run B by delay, so that
	// B's TestMain creates its temp dir after A has already taken its
	// before-snapshot. This widens the collision window: B's dir appears
	// during A's sentinel-child run, which is exactly the window where
	// TestTempDirRemovedOnFailedRun's after-snapshot catches it.
	runPairStaggered := func(tmpdirA, tmpdirB, pkg string, delay time.Duration) (string, error, string, error) {
		var wg sync.WaitGroup
		var outA, outB string
		var errA, errB error

		wg.Add(2)
		go func() {
			defer wg.Done()
			outA, errA = runTest(tmpdirA, pkg)
		}()
		go func() {
			defer wg.Done()
			time.Sleep(delay)
			outB, errB = runTest(tmpdirB, pkg)
		}()
		wg.Wait()

		return outA, errA, outB, errB
	}

	// --- Positive control: distinct TMPDIR per run, both pass ---
	// Run both collision-sensitive packages (server and multiplex) to cover
	// both prefixes.
	t.Run("distinct_tmpdir_both_pass", func(t *testing.T) {
		for _, pkg := range []string{"./server", "./multiplex"} {
			t.Run(pkg, func(t *testing.T) {
				dirA, err := os.MkdirTemp("", "isolation-a-*")
				if err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(dirA)

				dirB, err := os.MkdirTemp("", "isolation-b-*")
				if err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(dirB)

				outA, errA, outB, errB := runPair(dirA, dirB, pkg)

				if errA != nil {
					t.Errorf("run A failed with distinct TMPDIR:\n%s", outA)
				}
				if errB != nil {
					t.Errorf("run B failed with distinct TMPDIR:\n%s", outB)
				}
			})
		}
	})

	// --- Negative control: shared TMPDIR, at least one run collides ---
	// The collision is timing-dependent: the other run's temp dir must appear
	// between the before and after snapshots of TestTempDirRemovedOnFailedRun.
	// Run the pair up to 5 times and assert at least one iteration produces
	// the specific "left a temp dir behind" failure, not a build error,
	// sentinel error, or other unrelated failure.
	//
	// Use ./server for the collision: both runs create poly-lsp-mcp-srv-test-*
	// dirs, so each run's tempTestDirs("poly-lsp-mcp-srv-test-") scan sees the
	// other run's dir.
	//
	// The collision window is narrow: B's temp dir must appear between A's
	// before-snapshot and after-snapshot, which is the duration of A's
	// sentinel-child run. Staggering B's start by 2s ensures B's TestMain
	// (which creates the temp dir) fires after A has taken its before-snapshot
	// but before A's sentinel-child has finished. Without the stagger, both
	// TestMains fire nearly simultaneously and B's dir can appear before A's
	// before-snapshot, missing the window entirely.
	t.Run("shared_tmpdir_collides", func(t *testing.T) {
		const maxAttempts = 5
		const stagger = 2 * time.Second
		collided := false
		var lastOut string

		for i := 0; i < maxAttempts && !collided; i++ {
			sharedDir, err := os.MkdirTemp("", "isolation-shared-*")
			if err != nil {
				t.Fatal(err)
			}

			outA, errA, outB, errB := runPairStaggered(sharedDir, sharedDir, "./server", stagger)
			os.RemoveAll(sharedDir)

			// A collision is specifically a "left a temp dir behind"
			// failure. A build error, sentinel failure, or other error
			// is not the collision this proof is about.
			collisionA := errA != nil && strings.Contains(outA, "left a temp dir behind")
			collisionB := errB != nil && strings.Contains(outB, "left a temp dir behind")

			lastOut = fmt.Sprintf("attempt %d:\n--- run A (err=%v) ---\n%s\n--- run B (err=%v) ---\n%s",
				i+1, errA, outA, errB, outB)

			if collisionA || collisionB {
				collided = true
				if collisionA {
					t.Logf("run A collided: %s", collisionLine(outA))
				}
				if collisionB {
					t.Logf("run B collided: %s", collisionLine(outB))
				}
			} else {
				t.Logf("attempt %d: no collision (A err=%v, B err=%v)", i+1, errA, errB)
			}
		}

		if !collided {
			t.Fatalf("shared TMPDIR: no collision in %d attempts. Last output:\n%s", maxAttempts, lastOut)
		}
	})
}

// collisionLine returns the first line containing "left a temp dir behind"
// from the test output, for logging.
func collisionLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "left a temp dir behind") {
			return strings.TrimSpace(line)
		}
	}
	return "(collision line not found)"
}
