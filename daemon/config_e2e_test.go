package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// bogusConfig is a repository's poly-lsp-mcp.yaml that, if loaded, would
// replace the registry and give .go a made-up child LSP command. The daemon
// logs which config it loaded; that line is what the test checks.
const bogusConfig = `languages:
  - name: bogus
    extensions: [".go"]
    lsp:
      cmd: poly-lsp-bogus-child
`

// A shared daemon does not load the poly-lsp-mcp.yaml of the directory it
// was started from: not when `mcp --daemon` auto-starts it from a repo, and
// not when a person runs `daemon` there by hand.
func TestDaemonIgnoresTheStartDirectoryConfig(t *testing.T) {
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
	for _, how := range []string{"auto-start", "by-hand"} {
		t.Run(how, func(t *testing.T) {
			env, home := e2eEnv(t)
			t.Cleanup(func() { stopIn(home) })
			repo := t.TempDir()
			if err := os.WriteFile(filepath.Join(repo, "poly-lsp-mcp.yaml"), []byte(bogusConfig), 0o644); err != nil {
				t.Fatal(err)
			}
			// The daemon's log: daemon.log when auto-started, its stderr
			// when run by hand.
			dlogPath := filepath.Join(home, "daemon.log")
			if how == "by-hand" {
				dlogPath = filepath.Join(t.TempDir(), "daemon-stderr.log")
				dlogf, err := os.Create(dlogPath)
				if err != nil {
					t.Fatal(err)
				}
				defer dlogf.Close()
				d := exec.Command(bin, "daemon")
				d.Env, d.Dir, d.Stderr = env, repo, dlogf
				if err := d.Start(); err != nil {
					t.Fatal(err)
				}
				go func() { _ = d.Wait() }()
				waitStateIn(t, home, 20*time.Second)
			}
			logPath := filepath.Join(t.TempDir(), "test-mcp.log")
			logf, err := os.Create(logPath)
			if err != nil {
				t.Fatal(err)
			}
			defer logf.Close()
			srv := exec.Command(bin, "mcp", "--daemon", "--root", ws)
			srv.Env, srv.Dir, srv.Stderr = env, repo, logf
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
			text, isErr := callText(t, m)
			dlog, _ := os.ReadFile(dlogPath)
			if isErr || !strings.Contains(text, "Seven") {
				b, _ := os.ReadFile(logPath)
				t.Fatalf("node_query: isErr=%v %q\nmcp:\n%s\ndaemon:\n%s", isErr, text, b, dlog)
			}
			if strings.Contains(string(dlog), "config: loaded") || !strings.Contains(string(dlog), "no config file") {
				t.Errorf("the daemon loaded a config file:\n%s", dlog)
			}
			// Linux shows a process's working directory: the auto-started
			// daemon's is the daemon home.
			st := waitStateIn(t, home, 10*time.Second)
			if cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(st.PID), "cwd")); err == nil && how == "auto-start" {
				want, _ := filepath.EvalSymlinks(home)
				if cwd != want && cwd != home {
					t.Errorf("auto-started daemon runs in %s, want the daemon home %s", cwd, home)
				}
			}
		})
	}
}
