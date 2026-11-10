package multiplex

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/internal/jsonrpc"
)

// startInFleet starts a manager with the "stub" child as a member of f.
// Each call gets its own workspace root, as the daemon's roots do.
func startInFleet(t *testing.T, f *Fleet) *Manager {
	t.Helper()
	m := NewManager(makeTestRegistry(t))
	m.Lifecycle = f.lc
	m.Fleet = f
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cwd := t.TempDir()
	if err := m.Start(ctx, cwd, "file://"+cwd, []string{"stub"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Shutdown(ctx) })
	return m
}

func newTestFleet(t *testing.T, lc Lifecycle) *Fleet {
	lc.Interval = time.Hour // tests call check directly
	f := NewFleet(lc)
	t.Cleanup(f.Close)
	return f
}

// fakeRSS returns a readRSS seam that reports rss[pid], read under a lock
// so a test can fill the map after the children start.
func fakeRSS() (func(int) (int64, error), func(pid int, rss int64)) {
	var mu sync.Mutex
	m := map[int]int64{}
	return func(pid int) (int64, error) {
			mu.Lock()
			defer mu.Unlock()
			return m[pid], nil
		}, func(pid int, rss int64) {
			mu.Lock()
			defer mu.Unlock()
			m[pid] = rss
		}
}

func TestFleetCapEvictsLRUAcrossManagers(t *testing.T) {
	f := newTestFleet(t, Lifecycle{MaxChildren: 1})
	a := startInFleet(t, f)
	if st := status(t, a); st.State != "running" {
		t.Fatalf("a: %+v", st)
	}
	b := startInFleet(t, f)
	if st := status(t, a); st.State != "stopped" || st.CapStops != 1 || st.LastStop != StopCap {
		t.Fatalf("a after b started: %+v", st)
	}
	if st := status(t, b); st.State != "running" {
		t.Fatalf("b: %+v", st)
	}
	if fs := f.Status(); fs.Running != 1 || fs.MaxChildren != 1 {
		t.Fatalf("fleet: %+v", fs)
	}

	// Waking a makes room by stopping b.
	if a.RouteByURI(stubURI) == nil {
		t.Fatal("a did not wake")
	}
	if st := status(t, b); st.State != "stopped" || st.CapStops != 1 {
		t.Fatalf("b after a woke: %+v", st)
	}
	if fs := f.Status(); fs.Running != 1 {
		t.Fatalf("fleet after wake: %+v", fs)
	}

	// The status file carries the fleet row.
	a.StatusDir = t.TempDir()
	a.writeStatus()
	files, err := ReadStatusFiles(a.StatusDir)
	if err != nil || len(files) != 1 || files[0].Fleet == nil || files[0].Fleet.MaxChildren != 1 || files[0].Fleet.Running != 1 {
		t.Fatalf("status file: %v %+v", err, files)
	}
}

func TestFleetCapPicksLeastRecentlyUsed(t *testing.T) {
	f := newTestFleet(t, Lifecycle{MaxChildren: 2})
	a := startInFleet(t, f)
	b := startInFleet(t, f)
	time.Sleep(2 * time.Millisecond)
	a.RouteByURI(stubURI) // a is now the most recently used
	startInFleet(t, f)
	if st := status(t, b); st.State != "stopped" || st.LastStop != StopCap {
		t.Fatalf("b (LRU) not stopped: %+v", st)
	}
	if st := status(t, a); st.State != "running" {
		t.Fatalf("a (recent) stopped: %+v", st)
	}
}

func TestFleetCapNeverStopsBusyChild(t *testing.T) {
	f := newTestFleet(t, Lifecycle{MaxChildren: 1})
	a := startInFleet(t, f)
	c := a.Running(stubURI)
	c.pendingMu.Lock()
	c.pending["in-flight"] = make(chan *jsonrpc.Message, 1)
	c.pendingMu.Unlock()
	t.Cleanup(func() { c.removePending("in-flight") })

	b := startInFleet(t, f)
	if st := status(t, a); st.State != "running" {
		t.Fatalf("busy a was stopped: %+v", st)
	}
	if st := status(t, b); st.State != "running" {
		t.Fatalf("b not admitted over the cap: %+v", st)
	}
	if fs := f.Status(); fs.Running != 2 {
		t.Fatalf("fleet: %+v", fs)
	}
}

func TestFleetBudgetStopsLargest(t *testing.T) {
	f := newTestFleet(t, Lifecycle{RSSBudgetPct: 25})
	f.totalRAM = 1000 // budget 250
	read, set := fakeRSS()
	f.readRSS = read
	a := startInFleet(t, f)
	b := startInFleet(t, f)
	set(a.Running(stubURI).Pid(), 100)
	set(b.Running(stubURI).Pid(), 120)

	f.check() // 220 <= 250
	if fs := f.Status(); fs.Running != 2 || fs.RSS != 220 || fs.RSSBudget != 250 {
		t.Fatalf("under budget: %+v", fs)
	}
	set(b.Running(stubURI).Pid(), 200)
	f.check() // 300 > 250: stop b, the largest; 100 left
	if st := status(t, b); st.State != "stopped" || st.BudgetStops != 1 || st.LastStopRSS != 200 {
		t.Fatalf("b: %+v", st)
	}
	if st := status(t, a); st.State != "running" {
		t.Fatalf("a: %+v", st)
	}
	if fs := f.Status(); fs.Running != 1 || fs.RSS != 100 {
		t.Fatalf("after budget stop: %+v", fs)
	}
}

func TestFleetPressureStopsOnePerCheck(t *testing.T) {
	f := newTestFleet(t, Lifecycle{PSIThreshold: 20})
	f.readPSI = func() (float64, error) { return 30, nil }
	a := startInFleet(t, f)
	b := startInFleet(t, f)
	f.check()
	stopped := 0
	for _, m := range []*Manager{a, b} {
		if st := status(t, m); st.State == "stopped" && st.PressureStops == 1 {
			stopped++
		}
	}
	if stopped != 1 {
		t.Fatalf("pressure stopped %d children in one check, want 1", stopped)
	}
}

func TestFleetMacPressureStopsLargest(t *testing.T) {
	f := newTestFleet(t, Lifecycle{MacPressureLevel: 2})
	level := 1
	f.readPressure = func() (int, error) { return level, nil }
	read, set := fakeRSS()
	f.readRSS = read
	a := startInFleet(t, f)
	b := startInFleet(t, f)
	set(a.Running(stubURI).Pid(), 300)
	set(b.Running(stubURI).Pid(), 100)

	f.check()
	if fs := f.Status(); fs.Running != 2 {
		t.Fatalf("normal level stopped a child: %+v", fs)
	}
	level = 4
	f.check()
	if st := status(t, a); st.State != "stopped" || st.PressureStops != 1 {
		t.Fatalf("a (largest): %+v", st)
	}
	if st := status(t, b); st.State != "running" {
		t.Fatalf("b: %+v", st)
	}
}
