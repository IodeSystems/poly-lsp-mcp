package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// buildOne builds this module once into dir, as TestBlueGreenUpgradeEndToEnd
// does (without LSP_REBUILD_PATH, so the build is the one under test).
func buildOne(t *testing.T, dir string) string {
	t.Helper()
	mod, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "poly-lsp-mcp")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = mod
	cmd.Env = childEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// The whole idle cycle, with a real binary: a daemon with a short
// --idle-exit serves a proxy; when the proxy leaves, the daemon goes idle,
// exits, and removes daemon.json; a second proxy runs, its mcp --daemon
// path finds no daemon and auto-starts a replacement via EnsureRunning;
// node_query answers against the replacement, and a fresh daemon.json with
// a new, live PID exists.
func TestIdleExitAutoStartEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := buildOne(t, t.TempDir())
	env, home := e2eEnv(t)
	ws, err := resolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		src := fmt.Sprintf("package main\n\nfunc F%d() int { return %d }\n", i, i)
		if err := os.WriteFile(filepath.Join(ws, fmt.Sprintf("f%d.go", i)), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	logf, err := os.Create(filepath.Join(home, "test-daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logf.Close()

	const idleExit = 2 * time.Second
	daemon1 := exec.Command(bin, "daemon", "--allow="+ws, "--idle-exit="+idleExit.String())
	daemon1.Env, daemon1.Stdout, daemon1.Stderr, daemon1.Dir = env, logf, logf, home
	if err := daemon1.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = daemon1.Wait(); close(exited) }()
	firstPID := daemon1.Process.Pid
	defer func() {
		_ = daemon1.Process.Kill()
		if st, ok := ReadState(); ok && st.PID != firstPID {
			_ = syscall.Kill(st.PID, syscall.SIGTERM)
			WaitPidGone(st.PID, 10*time.Second)
		}
		if t.Failed() {
			for _, f := range []string{filepath.Join(home, "test-daemon.log"), LogPath()} {
				b, _ := os.ReadFile(f)
				t.Logf("%s:\n%s", f, b)
			}
		}
	}()
	waitState(t, 20*time.Second, "daemon 1", func(st State) bool { return st.PID == firstPID })

	// The proxy, in the shape the next client takes: a real `mcp --daemon`
	// process, so its connect path is EnsureRunning, not a test stub.
	startProxy := func() (*proxyHarness, *exec.Cmd) {
		t.Helper()
		proxy := exec.Command(bin, "mcp", "--daemon", "--root", ws)
		proxy.Env, proxy.Dir = env, home
		stdin, _ := proxy.StdinPipe()
		stdout, _ := proxy.StdoutPipe()
		proxyLog, _ := os.Create(filepath.Join(home, "test-proxy.log"))
		proxy.Stderr = proxyLog
		if err := proxy.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = stdin.Close()
			_ = proxy.Process.Kill()
			_ = proxy.Wait()
			if t.Failed() {
				b, _ := os.ReadFile(filepath.Join(home, "test-proxy.log"))
				t.Logf("proxy log:\n%s", b)
			}
		})
		h := newHarness(t, stdin, stdout)
		h.reply(h.request("initialize", map[string]any{}), 10*time.Second)
		return h, proxy
	}
	query := func(h *proxyHarness) {
		t.Helper()
		m := h.reply(h.request("tools/call", map[string]any{"name": "node_query", "arguments": map[string]any{"selector": "#F3"}}), 2*time.Minute)
		if text, isErr := callText(t, m); isErr || !strings.Contains(text, "F3") {
			t.Fatalf("node_query: isErr=%v %q", isErr, text)
		}
	}

	h1, _ := startProxy()
	query(h1) // served by daemon 1

	// The proxy leaves; the daemon is idle, and exits after --idle-exit.
	select {
	case <-exited:
		t.Fatal("daemon 1 exited while its proxy was connected")
	case <-time.After(2 * idleExit):
	}
	h1.request("shutdown", nil)
	_ = h1.in.Close()
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		t.Fatal("daemon 1 did not exit after its proxy left")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := ReadState(); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon.json left behind after the idle exit")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The second proxy auto-starts a replacement daemon; node_query answers
	// against it, and the state file is the new daemon's.
	h2, _ := startProxy()
	query(h2)
	st, ok := ReadState()
	if !ok {
		t.Fatal("no daemon.json after the auto-start")
	}
	if st.PID == firstPID || !PidAlive(st.PID) {
		t.Fatalf("daemon.json after the auto-start: %+v", st)
	}
}
