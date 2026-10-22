package multiplex

import (
	"context"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// startStub starts a manager with the "stub" child and the given
// lifecycle. The monitor is not relied on: tests call checkLifecycle
// directly so they do not depend on ticker timing.
func startStub(t *testing.T, lc Lifecycle) *Manager {
	t.Helper()
	m := NewManager(makeTestRegistry(t))
	m.RestartInitialBackoff = 50 * time.Millisecond
	m.RestartMaxBackoff = 100 * time.Millisecond
	m.RestartMaxAttempts = 3
	lc.Interval = time.Hour // keep the background monitor out of the way
	m.Lifecycle = lc
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cwd := t.TempDir()
	if err := m.Start(ctx, cwd, "file://"+cwd, []string{"stub"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Shutdown(ctx) })
	return m
}

func status(t *testing.T, m *Manager) ChildStatus {
	t.Helper()
	st := m.Status()
	if len(st) != 1 {
		t.Fatalf("Status: want 1 row, got %+v", st)
	}
	return st[0]
}

func gone(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return os.IsNotExist(err)
}

const stubURI = "file:///x.stub"

func TestIdleStopReapsAndWakesOnRoute(t *testing.T) {
	m := startStub(t, Lifecycle{IdleStop: time.Millisecond})
	var started atomic.Int32
	m.OnStart = func(lang string, c *Child) { started.Add(1) }

	first := m.RouteByURI(stubURI)
	pid := first.Pid()
	time.Sleep(5 * time.Millisecond)
	m.checkLifecycle()

	st := status(t, m)
	if st.State != "stopped" || st.IdleStops != 1 || st.LastStop != StopIdle {
		t.Fatalf("after idle check: %+v", st)
	}
	if !gone(pid) {
		t.Fatalf("idle-stopped pid %d not reaped", pid)
	}
	if m.Running(stubURI) != nil {
		t.Fatal("Running woke or returned a stopped child")
	}
	if !m.Serves(stubURI) {
		t.Fatal("Serves false for a stopped (wakeable) child")
	}
	if langs := m.Languages(); len(langs) != 1 {
		t.Fatalf("Languages dropped stopped child: %v", langs)
	}

	second := m.RouteByURI(stubURI)
	if second == nil || second == first {
		t.Fatalf("RouteByURI did not wake a fresh child: %v", second)
	}
	if started.Load() != 1 {
		t.Fatalf("OnStart calls = %d, want 1", started.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := second.Call(ctx, "workspace/symbol", map[string]any{"query": ""}); err != nil {
		t.Fatalf("woken child does not answer: %v", err)
	}
	st = status(t, m)
	if st.State != "running" || st.Starts != 1 || st.CrashRestarts != 0 {
		t.Fatalf("after wake: %+v", st)
	}
}

func TestMemoryStopIsNotACrash(t *testing.T) {
	m := startStub(t, Lifecycle{MaxRSS: 100})
	m.readRSS = func(int) (int64, error) { return 200, nil }

	// More memory stops than RestartMaxAttempts (3): each wake must still
	// succeed, because policy stops do not count against crash restarts.
	for i := 1; i <= 5; i++ {
		if m.RouteByURI(stubURI) == nil {
			t.Fatalf("round %d: no child", i)
		}
		m.checkLifecycle()
		st := status(t, m)
		if st.State != "stopped" || st.MemoryStops != i || st.LastStop != StopMemory || st.LastStopRSS != 200 {
			t.Fatalf("round %d: %+v", i, st)
		}
		if st.CrashRestarts != 0 {
			t.Fatalf("round %d: memory stop counted as crash: %+v", i, st)
		}
	}
}

func TestPressureStopsIdleLongestFirst(t *testing.T) {
	m := startStub(t, Lifecycle{PSIThreshold: 20})
	psi := 5.0
	m.readPSI = func() (float64, error) { return psi, nil }

	m.checkLifecycle()
	if st := status(t, m); st.State != "running" {
		t.Fatalf("below threshold: %+v", st)
	}
	psi = 30
	m.checkLifecycle()
	if st := status(t, m); st.State != "stopped" || st.PressureStops != 1 {
		t.Fatalf("above threshold: %+v", st)
	}
}

func TestPolicyStopIsNotRestartedByWatchdog(t *testing.T) {
	m := startStub(t, Lifecycle{IdleStop: time.Millisecond})
	time.Sleep(5 * time.Millisecond)
	m.checkLifecycle()
	// Give a (wrong) watchdog restart time to happen.
	time.Sleep(300 * time.Millisecond)
	st := status(t, m)
	if st.State != "stopped" || st.CrashRestarts != 0 {
		t.Fatalf("watchdog restarted a policy-stopped child: %+v", st)
	}
}

func TestParsePSI(t *testing.T) {
	v, err := parsePSISomeAvg10("some avg10=12.34 avg60=0.29 avg300=0.24 total=7299493004\nfull avg10=0.07 avg60=0.21 avg300=0.18 total=1\n")
	if err != nil || v != 12.34 {
		t.Fatalf("got %v, %v", v, err)
	}
	if _, err := parsePSISomeAvg10("garbage"); err == nil {
		t.Fatal("want error")
	}
}

func TestReadVmRSSSelf(t *testing.T) {
	rss, err := readVmRSS(os.Getpid())
	if err != nil || rss <= 0 {
		t.Fatalf("got %d, %v", rss, err)
	}
}

func TestStatusFileWrittenAndRemoved(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(makeTestRegistry(t))
	m.StatusDir = dir
	m.Lifecycle = Lifecycle{IdleStop: time.Millisecond, Interval: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cwd := t.TempDir()
	if err := m.Start(ctx, cwd, "file://"+cwd, []string{"stub"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	m.checkLifecycle() // idle stop writes the file

	files, err := ReadStatusFiles(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("ReadStatusFiles: %v, %+v", err, files)
	}
	f := files[0]
	if f.PID != os.Getpid() || f.Root != cwd || len(f.Children) != 1 {
		t.Fatalf("status file: %+v", f)
	}
	if c := f.Children[0]; c.State != "stopped" || c.IdleStops != 1 {
		t.Fatalf("child row: %+v", c)
	}

	m.Shutdown(ctx)
	if files, _ := ReadStatusFiles(dir); len(files) != 0 {
		t.Fatalf("status file left after Shutdown: %+v", files)
	}
}
