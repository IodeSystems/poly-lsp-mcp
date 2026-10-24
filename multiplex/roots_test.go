package multiplex

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestNearestRoot(t *testing.T) {
	roots := []string{"/w/a", "/w/a/b", "/w/c"}
	for path, want := range map[string]string{
		"/w/a/x.py":     "/w/a",
		"/w/a/b/y.py":   "/w/a/b",
		"/w/a/bb/z.py":  "/w/a", // a/bb is not under a/b
		"/w/c/d/e/f.py": "/w/c",
		"/w/top.py":     "/w",
		"/w/a":          "/w/a",
	} {
		if got := NearestRoot(path, roots, "/w"); got != want {
			t.Errorf("NearestRoot(%s) = %s, want %s", path, got, want)
		}
	}
}

// With Roots, a language's children start on first use, one per project
// root, files going to the nearest root and the rest to the workspace
// root; past RootsMax the least recently used stops.
func TestPerRootChildrenStartLazilyUnderACap(t *testing.T) {
	cwd := t.TempDir()
	a, ab, c := filepath.Join(cwd, "a"), filepath.Join(cwd, "a", "b"), filepath.Join(cwd, "c")
	for _, d := range []string{ab, c} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := NewManager(makeTestRegistry(t))
	m.Roots = map[string][]string{"stub": {a, ab, c}}
	m.RootsMax = 2
	var started atomic.Int32
	m.OnStart = func(string, *Child) { started.Add(1) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Start(ctx, cwd, "file://"+cwd, []string{"stub"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Shutdown(ctx) })

	uri := func(p ...string) string { return "file://" + filepath.Join(append([]string{cwd}, p...)...) }
	if langs := m.Languages(); len(langs) != 1 || langs[0] != "stub" {
		t.Fatalf("Languages = %v, want [stub] before any child runs", langs)
	}
	if st := m.Status(); len(st) != 0 {
		t.Fatalf("a child started before first use: %+v", st)
	}
	if !m.Serves(uri("c", "x.stub")) || m.Running(uri("c", "x.stub")) != nil {
		t.Fatal("an unstarted root should be served but not running")
	}

	inA := m.RouteByURI(uri("a", "x.stub"))
	time.Sleep(2 * time.Millisecond)
	inAB := m.RouteByURI(uri("a", "b", "y.stub"))
	if inA == nil || inAB == nil || inA == inAB {
		t.Fatalf("a and a/b should each get a child: %v %v", inA, inAB)
	}
	time.Sleep(2 * time.Millisecond)
	if again := m.RouteByURI(uri("a", "deep", "z.stub")); again != inA {
		t.Fatal("a/deep should route to a's running child")
	}
	time.Sleep(2 * time.Millisecond)
	// A third root's child: a/b is the least recently used, so it stops.
	top := m.RouteByURI(uri("top.stub"))
	if top == nil || top == inA || top == inAB {
		t.Fatalf("top.stub should start a workspace-root child: %v", top)
	}
	if started.Load() != 3 {
		t.Errorf("OnStart calls = %d, want 3", started.Load())
	}

	rows := map[string]ChildStatus{}
	running := 0
	for _, st := range m.Status() {
		rows[st.Root] = st
		if st.State == "running" {
			running++
		}
	}
	if running != 2 {
		t.Errorf("running = %d, want RootsMax 2: %+v", running, rows)
	}
	if st := rows[ab]; st.State != "stopped" || st.LastStop != StopLRU || st.LRUStops != 1 {
		t.Errorf("a/b should be stopped as least recently used: %+v", st)
	}
	if rows[a].State != "running" || rows[cwd].State != "running" {
		t.Errorf("a and the workspace root should run: %+v", rows)
	}
	if _, ok := rows[c]; ok {
		t.Errorf("c was never used and should not have started: %+v", rows[c])
	}

	if _, err := inA.Call(ctx, "workspace/symbol", map[string]any{"query": ""}); err != nil {
		t.Errorf("a's child does not answer: %v", err)
	}

	// The stopped root starts again on demand, as a fresh child; a, now
	// the least recently used, makes room.
	if back := m.RouteByURI(uri("a", "b", "y.stub")); back == nil || back == inAB {
		t.Fatalf("a/b should start again: %v", back)
	}
	if m.Running(uri("a", "x.stub")) != nil {
		t.Error("a should have stopped to make room for a/b")
	}
}

// Without Roots nothing changes: one eager child at the workspace root.
func TestNoRootsKeepsOneEagerChild(t *testing.T) {
	cwd := t.TempDir()
	m := NewManager(makeTestRegistry(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.Start(ctx, cwd, "file://"+cwd, []string{"stub"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Shutdown(ctx) })
	st := m.Status()
	if len(st) != 1 || st[0].State != "running" || st[0].Root != cwd {
		t.Fatalf("Status = %+v, want one running child at %s", st, cwd)
	}
	if m.RouteByURI("file://"+filepath.Join(cwd, "a", "x.stub")) != m.RouteByURI("file:///elsewhere/y.stub") {
		t.Fatal("every file should route to the one child")
	}
}
