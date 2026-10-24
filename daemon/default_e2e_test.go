package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Plain `mcp`, no flags, proxies to the shared daemon (starting one in
// the sandbox home); `mcp --no-daemon` serves in-process and starts none.
func TestMCPDaemonIsTheDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
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
	ws, err := resolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "f.go"), []byte("package main\n\nfunc Seven() int { return 7 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// run serves one node_query through `mcp args...` in a fresh sandbox
	// and returns the sandbox home and the server's stderr. readOnlyHome
	// leaves no ~/.poly-lsp to make.
	run := func(t *testing.T, readOnlyHome bool, args ...string) (home, stderr string) {
		t.Helper()
		env, home := e2eEnv(t)
		// Settings that would choose in-process on their own.
		kept := env[:0:0]
		for _, kv := range env {
			if !strings.HasPrefix(kv, "POLY_LSP_NO_DAEMON=") && !strings.HasPrefix(kv, "POLY_LSP_READ_CHAR_BUDGET=") {
				kept = append(kept, kv)
			}
		}
		t.Cleanup(func() {
			// The daemon this run started, if any: sandboxed, but stop it.
			if st, ok := ReadState(); ok && PidAlive(st.PID) {
				_ = syscall.Kill(st.PID, syscall.SIGTERM)
				WaitPidGone(st.PID, 10*time.Second)
			}
		})
		if readOnlyHome {
			// No ~/.poly-lsp can be made: the daemon cannot start.
			if err := os.Chmod(home, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(home, 0o755) })
		}
		logPath := filepath.Join(t.TempDir(), "test-mcp.log")
		logf, err := os.Create(logPath)
		if err != nil {
			t.Fatal(err)
		}
		defer logf.Close()
		srv := exec.Command(bin, append([]string{"mcp", "--root", ws}, args...)...)
		srv.Env, srv.Dir, srv.Stderr = kept, home, logf
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
			t.Fatalf("node_query: isErr=%v %q", isErr, text)
		}
		b, _ := os.ReadFile(logPath)
		return home, string(b)
	}

	t.Run("default", func(t *testing.T) {
		_, log := run(t, false)
		st, ok := ReadState()
		if !ok || !PidAlive(st.PID) {
			t.Fatalf("no daemon running in the sandbox after plain mcp; state %+v\n%s", st, log)
		}
		if !strings.Contains(log, "proxying root "+ws) || !strings.Contains(log, "served by daemon") {
			t.Errorf("plain mcp did not proxy:\n%s", log)
		}
	})
	t.Run("no-daemon", func(t *testing.T) {
		home, log := run(t, false, "--no-daemon")
		if _, ok := ReadState(); ok {
			t.Errorf("--no-daemon left a daemon state file")
		}
		if _, err := os.Stat(filepath.Join(home, ".poly-lsp", "daemon.log")); err == nil {
			t.Errorf("--no-daemon started a daemon")
		}
		if strings.Contains(log, "proxying") || !strings.Contains(log, "mcp: workspace root "+ws) {
			t.Errorf("--no-daemon did not serve in-process:\n%s", log)
		}
	})
	// A daemon that cannot start: plain mcp serves in-process, logged.
	t.Run("unavailable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes a read-only home")
		}
		_, log := run(t, true)
		if _, ok := ReadState(); ok {
			t.Errorf("a daemon state file was written under a read-only home")
		}
		if !strings.Contains(log, "the shared daemon is unavailable") || !strings.Contains(log, "mcp: workspace root "+ws) {
			t.Errorf("plain mcp did not fall back to in-process:\n%s", log)
		}
	})
}
