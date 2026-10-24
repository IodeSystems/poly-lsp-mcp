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

// buildTwo builds this module twice into dir, as two different builds:
// the second with -trimpath, which changes the binary and so its BuildID
// (revision + executable hash) with no stamp and no test-only switch.
func buildTwo(t *testing.T, dir string) (a, b string) {
	t.Helper()
	mod, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	a, b = filepath.Join(dir, "a", "poly-lsp-mcp"), filepath.Join(dir, "b", "poly-lsp-mcp")
	for _, c := range [][]string{{"build", "-o", a, "."}, {"build", "-trimpath", "-o", b, "."}} {
		cmd := exec.Command("go", c...)
		cmd.Dir = mod
		cmd.Env = childEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %v: %v\n%s", c, err, out)
		}
	}
	return a, b
}

// e2eEnv is a sandbox for real daemons: their own home, runtime dir and
// cache, no LSP_REBUILD_PATH, and a PATH with no child LSP or git on it,
// so a root comes up from tree-sitter alone, fast.
func e2eEnv(t *testing.T) (env []string, home string) {
	t.Helper()
	home = t.TempDir()
	rt := t.TempDir()
	empty := t.TempDir()
	env = append(childEnv(), "HOME="+home, "XDG_RUNTIME_DIR="+rt, "XDG_CACHE_HOME="+filepath.Join(home, ".cache"), "PATH="+empty)
	// This process reads the same daemon.json the daemons write.
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", rt)
	return env, home
}

// waitState waits for daemon.json to satisfy ok.
func waitState(t *testing.T, d time.Duration, what string, ok func(State) bool) State {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if st, found := ReadState(); found && ok(st) {
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
	st, _ := ReadState()
	t.Fatalf("daemon.json never showed %s; last %+v", what, st)
	return State{}
}

// The whole upgrade, with real binaries: a daemon of build A serves; a
// proxy of build B connects, sees A, and asks it to upgrade into B's
// binary; A starts B beside itself, B warms the root and takes the
// socket; A drains and exits; and the proxy, still the same process, is
// served by B. Read-only calls made all through the swap succeed.
func TestBlueGreenUpgradeEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary twice")
	}
	a, b := buildTwo(t, t.TempDir())
	idA, idB := probeBuildOnce(a), probeBuildOnce(b) // childEnv: no LSP_REBUILD_PATH
	if idA == "" || idA == idB {
		t.Fatalf("builds not distinct: %q %q", idA, idB)
	}
	env, home := e2eEnv(t)
	ws := filepath.Join(home, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		src := fmt.Sprintf("package main\n\nfunc F%d() int { return %d }\n", i, i)
		if err := os.WriteFile(filepath.Join(ws, fmt.Sprintf("f%d.go", i)), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	logf, err := os.Create(filepath.Join(home, "test-daemon-a.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logf.Close()

	daemonA := exec.Command(a, "daemon")
	daemonA.Env, daemonA.Stdout, daemonA.Stderr, daemonA.Dir = env, logf, logf, home
	if err := daemonA.Start(); err != nil {
		t.Fatal(err)
	}
	aExited := make(chan struct{})
	go func() { _ = daemonA.Wait(); close(aExited) }()
	defer func() {
		_ = daemonA.Process.Kill()
		if st, ok := ReadState(); ok && st.PID != daemonA.Process.Pid {
			_ = syscall.Kill(st.PID, syscall.SIGTERM)
			WaitPidGone(st.PID, 10*time.Second)
		}
		if t.Failed() {
			for _, f := range []string{filepath.Join(home, "test-daemon-a.log"), LogPath()} {
				b, _ := os.ReadFile(f)
				t.Logf("%s:\n%s", f, b)
			}
		}
	}()
	waitState(t, 20*time.Second, "daemon A", func(st State) bool { return st.PID == daemonA.Process.Pid && st.Build == idA })

	proxy := exec.Command(b, "mcp", "--daemon", "--root", ws)
	proxy.Env, proxy.Dir = env, home
	stdin, _ := proxy.StdinPipe()
	stdout, _ := proxy.StdoutPipe()
	proxyLog, _ := os.Create(filepath.Join(home, "test-proxy.log"))
	defer proxyLog.Close()
	proxy.Stderr = proxyLog
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = proxy.Process.Kill()
		_ = proxy.Wait()
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(home, "test-proxy.log"))
			t.Logf("proxy log:\n%s", b)
		}
	}()
	h := newHarness(t, stdin, stdout)
	h.reply(h.request("initialize", map[string]any{}), 10*time.Second)

	query := func() {
		t.Helper()
		m := h.reply(h.request("tools/call", map[string]any{"name": "node_query", "arguments": map[string]any{"selector": "#F7"}}), 2*time.Minute)
		if text, isErr := callText(t, m); isErr || !strings.Contains(text, "F7") {
			t.Fatalf("node_query across the swap: isErr=%v %q", isErr, text)
		}
	}
	query() // served by A

	// Calls keep coming while A hands over to B.
	deadline := time.Now().Add(3 * time.Minute)
	swapped := false
	for !swapped && time.Now().Before(deadline) {
		query()
		select {
		case <-aExited:
			swapped = true
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !swapped {
		t.Fatal("daemon A never exited")
	}
	st := waitState(t, 5*time.Second, "daemon B", func(st State) bool { return st.Build == idB })
	if !PidAlive(st.PID) || st.PID == daemonA.Process.Pid {
		t.Fatalf("state after the swap: %+v", st)
	}
	for i := 0; i < 3; i++ {
		query() // served by B
	}
	if hl, err := NewClient(st.Socket).health(); err != nil || hl.Build != idB || hl.PID != st.PID {
		t.Fatalf("health after the swap: %+v %v", hl, err)
	}
	// The client was told, in-band: upgrading by A, upgraded by the proxy.
	up := h.logged(daemonLogger, 5*time.Second, func(d map[string]any) bool { return d["state"] == "upgrading" })
	if up["from"] != idA || up["to"] != idB {
		t.Errorf("upgrading notice: %v", up)
	}
	done := h.logged(daemonLogger, 10*time.Second, func(d map[string]any) bool { return d["state"] == "upgraded" })
	if done["from"] != idA || done["to"] != idB {
		t.Errorf("upgraded notice: %v", done)
	}
	// B warmed the root A had open, and the same proxy reconnected to it.
	dlog, _ := os.ReadFile(LogPath())
	if !strings.Contains(string(dlog), "warm: 1 root(s)") || !strings.Contains(string(dlog), "took over") {
		t.Errorf("daemon log does not show a warm takeover:\n%s", dlog)
	}
	plog, _ := os.ReadFile(filepath.Join(home, "test-proxy.log"))
	if n := strings.Count(string(plog), "served by daemon"); n < 2 {
		t.Errorf("the proxy connected %d time(s), want a reconnect:\n%s", n, plog)
	}
}
