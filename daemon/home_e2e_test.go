package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// buildBinary builds this module once into a temp dir.
func buildBinary(t *testing.T) string {
	t.Helper()
	mod, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "poly-lsp-mcp")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir, build.Env = mod, childEnv()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// readStateIn reads daemon.json from a daemon home other than this
// process's.
func readStateIn(dir string) (State, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "daemon.json"))
	if err != nil {
		return State{}, false
	}
	var st State
	if json.Unmarshal(b, &st) != nil {
		return State{}, false
	}
	return st, true
}

func waitStateIn(t *testing.T, dir string, d time.Duration) State {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if st, ok := readStateIn(dir); ok && PidAlive(st.PID) && NewClient(st.Socket).Healthy() {
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "daemon.log"))
	t.Fatalf("no healthy daemon recorded in %s\n%s", dir, b)
	return State{}
}

// stopIn stops the daemon recorded in dir, if one is alive.
func stopIn(dir string) {
	if st, ok := readStateIn(dir); ok && PidAlive(st.PID) {
		_ = syscall.Kill(st.PID, syscall.SIGTERM)
		WaitPidGone(st.PID, 10*time.Second)
	}
}

// Two daemon homes, two daemons: one chosen by --daemon-home, one by
// $POLY_LSP_HOME, running side by side, each with its own state, socket
// and lock; `daemon --stop --daemon-home` stops only its own.
func TestTwoDaemonHomesRunSideBySide(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := buildBinary(t)
	env, _ := e2eEnv(t)
	var base []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, HomeEnv+"=") {
			base = append(base, kv)
		}
	}
	homeA, homeB := t.TempDir(), t.TempDir()
	t.Cleanup(func() { stopIn(homeA); stopIn(homeB) })

	start := func(env []string, args ...string) {
		cmd := exec.Command(bin, append([]string{"daemon"}, args...)...)
		cmd.Env, cmd.Dir = env, t.TempDir()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { _ = cmd.Wait() }()
	}
	start(base, "--daemon-home="+homeA)
	start(append(base, HomeEnv+"="+homeB))
	a := waitStateIn(t, homeA, 20*time.Second)
	b := waitStateIn(t, homeB, 20*time.Second)
	if a.PID == b.PID || a.Socket == b.Socket {
		t.Fatalf("one daemon for two homes: %+v %+v", a, b)
	}
	for _, c := range []struct {
		st   State
		home string
	}{{a, homeA}, {b, homeB}} {
		if filepath.Dir(c.st.Socket) != c.home && !strings.HasPrefix(filepath.Base(c.st.Socket), "poly-lsp-") {
			t.Errorf("socket %s is neither in %s nor the hashed fallback", c.st.Socket, c.home)
		}
		if _, err := os.Stat(filepath.Join(c.home, "daemon.lock")); err != nil {
			t.Errorf("no lock in %s: %v", c.home, err)
		}
		for _, kv := range c.st.Args {
			if strings.HasPrefix(kv, "--daemon-home") {
				t.Errorf("--daemon-home recorded in the replayed flags: %v", c.st.Args)
			}
		}
	}

	stop := exec.Command(bin, "daemon", "--stop", "--daemon-home", homeA)
	stop.Env = base
	if out, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("daemon --stop --daemon-home: %v\n%s", err, out)
	}
	if !WaitPidGone(a.PID, 10*time.Second) {
		t.Fatalf("daemon A (pid %d) did not stop", a.PID)
	}
	if !PidAlive(b.PID) || !NewClient(b.Socket).Healthy() {
		t.Fatalf("stopping A stopped B too")
	}
}

// `mcp` with $POLY_LSP_HOME auto-starts its daemon in that home: the state,
// log, lock and socket are there, and the (sandbox) $HOME gets no .poly-lsp.
// A relative --daemon-home is taken from the working directory.
func TestMCPProxyStartsItsDaemonInPolyLSPHome(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := buildBinary(t)
	ws, err := resolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "f.go"), []byte("package main\n\nfunc Seven() int { return 7 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, how := range []string{"env", "relative-flag"} {
		t.Run(how, func(t *testing.T) {
			env, _ := e2eHomeEnv(t) // a sandbox $HOME to watch
			sandboxHome := os.Getenv("HOME")
			cwd := t.TempDir()
			dh := filepath.Join(cwd, "dh")
			args := []string{"mcp", "--root", ws}
			if how == "env" {
				env = append(env, HomeEnv+"="+dh)
			} else {
				args = append(args, "--daemon-home", "dh")
			}
			t.Cleanup(func() { stopIn(dh) })
			logPath := filepath.Join(t.TempDir(), "test-mcp.log")
			logf, err := os.Create(logPath)
			if err != nil {
				t.Fatal(err)
			}
			defer logf.Close()
			srv := exec.Command(bin, args...)
			srv.Env, srv.Dir, srv.Stderr = env, cwd, logf
			stdin, _ := srv.StdinPipe()
			stdout, _ := srv.StdoutPipe()
			if err := srv.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = stdin.Close()
				_ = srv.Process.Kill()
				_ = srv.Wait()
			}()
			h := newHarness(t, stdin, stdout)
			h.reply(h.request("initialize", map[string]any{}), 20*time.Second)
			m := h.reply(h.request("tools/call", map[string]any{"name": "node_query", "arguments": map[string]any{"selector": "#Seven"}}), 2*time.Minute)
			if text, isErr := callText(t, m); isErr || !strings.Contains(text, "Seven") {
				b, _ := os.ReadFile(logPath)
				t.Fatalf("node_query: isErr=%v %q\n%s", isErr, text, b)
			}
			b, _ := os.ReadFile(logPath)
			if !strings.Contains(string(b), "served by daemon") {
				t.Fatalf("mcp did not proxy:\n%s", b)
			}
			st := waitStateIn(t, dh, 10*time.Second)
			fi, err := os.Stat(dh)
			if err != nil || fi.Mode().Perm() != 0o700 {
				t.Errorf("daemon home %s: %v mode %v, want 0700", dh, err, fi.Mode().Perm())
			}
			for _, f := range []string{"daemon.log", "daemon.lock"} {
				if _, err := os.Stat(filepath.Join(dh, f)); err != nil {
					t.Errorf("%s not in the daemon home: %v", f, err)
				}
			}
			if filepath.Dir(st.Socket) != dh && !strings.HasPrefix(filepath.Base(st.Socket), "poly-lsp-") {
				t.Errorf("socket %s is neither in %s nor the hashed fallback", st.Socket, dh)
			}
			if _, err := os.Stat(filepath.Join(sandboxHome, ".poly-lsp")); err == nil {
				t.Errorf("%s/.poly-lsp was made with a daemon home set", sandboxHome)
			}
		})
	}
}
