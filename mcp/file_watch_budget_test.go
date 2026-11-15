package mcp

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

type fakeAdder struct {
	added  []string
	failAt int // fail with ENOSPC on the Nth Add (1-based); 0 = never
}

func (f *fakeAdder) Add(name string) error {
	if f.failAt > 0 && len(f.added)+1 == f.failAt {
		return fmt.Errorf("add %s: %w", name, syscall.ENOSPC)
	}
	f.added = append(f.added, name)
	return nil
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// mkTree makes root/<top>/d0..d<n-1> for each entry.
func mkTree(t *testing.T, tops map[string]int) string {
	t.Helper()
	root := t.TempDir()
	for top, n := range tops {
		for i := 0; i < n; i++ {
			if err := os.MkdirAll(filepath.Join(root, top, fmt.Sprintf("d%d", i)), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func TestReadInotifyMax(t *testing.T) {
	p := filepath.Join(t.TempDir(), "max")
	if err := os.WriteFile(p, []byte("65536\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, ok := readInotifyMax(p); !ok || n != 65536 {
		t.Errorf("got %d,%v", n, ok)
	}
	if _, ok := readInotifyMax(filepath.Join(t.TempDir(), "missing")); ok {
		t.Error("missing file reported ok")
	}
	buf := captureLog(t)
	b := budgetFromHost(filepath.Join(t.TempDir(), "missing"))
	if b.max != inotifyMaxFallback || b.limit != inotifyMaxFallback/10 {
		t.Errorf("fallback budget = %d/%d", b.max, b.limit)
	}
	if !strings.Contains(buf.String(), "assuming max_user_watches=8192") {
		t.Errorf("fallback not logged: %q", buf.String())
	}
}

func TestAddWatchDirsStopsAtBudgetAndWarnsOnce(t *testing.T) {
	// max 100 -> limit 10. aaa has 4 dirs (+itself), bbb 8, ccc 3.
	root := mkTree(t, map[string]int{"aaa": 4, "bbb": 8, "ccc": 3})
	buf := captureLog(t)
	srv := newIndexedServer(t, root)
	srv.watchBudget = newWatchBudget(100)
	w := &fakeAdder{}
	n := srv.addWatchDirs(w, root)
	if n != 10 || len(w.added) != 10 {
		t.Fatalf("added %d (%d calls), want 10", n, len(w.added))
	}
	// A second pass (a dir-create rewatch) must add nothing and not warn again.
	srv.addWatchDirs(w, root)
	if len(w.added) != 10 {
		t.Errorf("rewatch exceeded budget: %d", len(w.added))
	}
	out := buf.String()
	if c := strings.Count(out, "watch budget of 10 reached"); c != 1 {
		t.Fatalf("budget warning count = %d\n%s", c, out)
	}
	// Walk order is lexical: root, aaa(5), bbb partial; ccc never reached.
	if !strings.Contains(out, "bbb (partial)") || !strings.Contains(out, "ccc") {
		t.Errorf("unwatched list wrong:\n%s", out)
	}
	if strings.Contains(out, "aaa") {
		t.Errorf("fully watched dir listed:\n%s", out)
	}
}

func TestAddWatchDirsPerTopDirWarning(t *testing.T) {
	// max 100: >5 watches in one top dir warns. big has 7 dirs + itself.
	root := mkTree(t, map[string]int{"big": 7, "small": 1})
	buf := captureLog(t)
	srv := newIndexedServer(t, root)
	b := newWatchBudget(1000) // limit 100: never full here
	b.max = 100               // per-dir threshold from max
	srv.watchBudget = b
	srv.addWatchDirs(&fakeAdder{}, root)
	out := buf.String()
	if c := strings.Count(out, "big uses "); c != 1 {
		t.Errorf("big warnings = %d\n%s", c, out)
	}
	if strings.Contains(out, "small uses") {
		t.Errorf("small warned:\n%s", out)
	}
}

func TestAddWatchDirsENOSPC(t *testing.T) {
	root := mkTree(t, map[string]int{"aaa": 3, "bbb": 3})
	buf := captureLog(t)
	srv := newIndexedServer(t, root)
	srv.watchBudget = newWatchBudget(100000)
	w := &fakeAdder{failAt: 4}
	n := srv.addWatchDirs(w, root)
	if n != 3 {
		t.Errorf("added %d, want 3 before ENOSPC", n)
	}
	out := buf.String()
	if c := strings.Count(out, "watch limit reached"); c != 1 {
		t.Errorf("ENOSPC warning count = %d\n%s", c, out)
	}
	if !strings.Contains(out, "aaa") {
		t.Errorf("dir not named:\n%s", out)
	}
	// Stays stopped.
	srv.addWatchDirs(w, root)
	if len(w.added) != 3 {
		t.Errorf("kept adding after ENOSPC: %d", len(w.added))
	}
}

func TestWatchBudgetForgetFreesSlot(t *testing.T) {
	root := mkTree(t, map[string]int{"aaa": 4})
	captureLog(t)
	srv := newIndexedServer(t, root)
	srv.watchBudget = newWatchBudget(30) // limit 3
	w := &fakeAdder{}
	srv.addWatchDirs(w, root)
	if len(w.added) != 3 {
		t.Fatalf("added %d", len(w.added))
	}
	srv.watchBudget.forget(root, filepath.Join(root, "aaa"))
	srv.addWatchDirs(w, root)
	if len(w.added) != 4 {
		t.Errorf("slot not reused: %d", len(w.added))
	}
}
