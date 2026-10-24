package daemon

import (
	"io"
	"os"
	"testing"
	"time"
)

// shortIdle shortens the idle-exit timings for one test.
func shortIdle(t *testing.T, empty time.Duration) {
	t.Helper()
	oldEmpty, oldCheck := emptyExitWait, idleCheck
	emptyExitWait, idleCheck = empty, 20*time.Millisecond
	t.Cleanup(func() { emptyExitWait, idleCheck = oldEmpty, oldCheck })
}

// serveIdle runs Serve in this process, in a sandboxed home, with idle
// exit after idleExit and the given root lifetime. done gets Serve's
// result when the daemon ends.
func serveIdle(t *testing.T, allow string, idleExit, grace time.Duration, idleCap int64) (socket string, done chan error) {
	t.Helper()
	cfg, reg := testRegistryDeps(t)
	r := NewRegistry(cfg, reg, false, false)
	r.SetRootLifetime(grace, idleCap)
	done = make(chan error, 1)
	go func() { done <- Serve(Config{Allow: NewAllowList([]string{allow}), Reg: r, IdleExit: idleExit}) }()
	c := NewClient(SocketPath())
	deadline := time.Now().Add(10 * time.Second)
	for !c.Healthy() {
		if time.Now().After(deadline) {
			t.Fatal("in-process daemon did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return SocketPath(), done
}

// proxyUntil runs a proxy of root on socket until the returned end is
// called: a client connecting and leaving.
func proxyUntil(t *testing.T, socket, root string) (*proxyHarness, func()) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- RunProxy(func() (*Client, error) { return NewClient(socket), nil }, root, OpenOptions{}, nil, inR, outW)
		_ = outW.Close()
	}()
	h := newHarness(t, inW, outR)
	h.reply(h.request("initialize", map[string]any{}), 5*time.Second)
	h.logged(indexLogger, 30*time.Second, nil)
	return h, func() {
		h.request("shutdown", nil)
		_ = inW.Close()
		<-done
	}
}

func waitExit(t *testing.T, done chan error, d time.Duration) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(d):
		t.Fatal("the daemon did not exit")
	}
}

// A connected client keeps the daemon; when the last client left and no
// root is kept (here --idle-roots-mb 0 evicts it), the daemon exits,
// removing daemon.json and its socket and letting go of the lock, and the
// next daemon starts in its place.
func TestDaemonExitsWithNoClientAndNoRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	shortIdle(t, 200*time.Millisecond)
	root, resolved := goWorkspace(t)
	socket, done := serveIdle(t, resolved, time.Hour, time.Hour, 0)

	_, end := proxyUntil(t, socket, root)
	select {
	case err := <-done:
		t.Fatalf("exited with a client connected: %v", err)
	case <-time.After(5 * emptyExitWait):
	}
	end()
	waitExit(t, done, 10*time.Second)

	if _, ok := ReadState(); ok {
		t.Error("daemon.json left behind")
	}
	if _, err := os.Stat(socket); err == nil {
		t.Error("socket left behind")
	}
	l, err := TryLock()
	if err != nil {
		t.Fatalf("lock still held: %v", err)
	}
	l.Release()

	// The next start (what EnsureRunning does for the next proxy) serves.
	socket2, done2 := serveIdle(t, resolved, time.Hour, time.Hour, 0)
	_, end2 := proxyUntil(t, socket2, root)
	end2()
	waitExit(t, done2, 10*time.Second)
}

// With a client-less root kept, the daemon waits --idle-exit, then exits.
// Child LSPs of the root stop after --root-lsp-grace meanwhile.
func TestDaemonExitsAfterIdleExitWithARootKept(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	shortIdle(t, 50*time.Millisecond)
	root, resolved := goWorkspace(t)
	const idleExit = 1500 * time.Millisecond
	socket, done := serveIdle(t, resolved, idleExit, 50*time.Millisecond, -1)

	_, end := proxyUntil(t, socket, root)
	end()
	c := NewClient(socket)
	// Each /health is a request, so the daemon cannot go idle meanwhile.
	deadline := time.Now().Add(10 * time.Second)
	for {
		h, err := c.Health(time.Second)
		if err != nil {
			t.Fatalf("exited before --idle-exit with a root kept: %v", err)
		}
		if len(h.RootStats) != 1 {
			t.Fatalf("root stats: %+v", h.RootStats)
		}
		if st := h.RootStats[0]; st.State == "up" && st.Resident > 0 && st.Clients == 0 && st.LSPsStopped && h.Clients == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the root's child LSPs were not stopped after its last client left: %+v", h)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// /health is a request: the idle clock runs from the last one.
	start := time.Now()
	waitExit(t, done, idleExit+5*time.Second)
	if took := time.Since(start); took < idleExit-idleExit/4 {
		t.Errorf("exited %s after the last request, before --idle-exit %s", took, idleExit)
	}
}
