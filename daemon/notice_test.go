package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// serveInProcess runs Serve in this process, in a sandboxed home, and
// returns its socket. Stopped at cleanup.
func serveInProcess(t *testing.T, allow string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cfg, _, _ := config.LoadOrDefault("nonexistent.yaml")
	reg, _ := cfg.Build()
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Serve(Config{Allow: NewAllowList([]string{allow}), Reg: NewRegistry(cfg, reg, false, false), stop: stop})
	}()
	c := NewClient(SocketPath())
	deadline := time.Now().Add(10 * time.Second)
	for !c.Healthy() {
		if time.Now().After(deadline) {
			t.Fatal("in-process daemon did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() {
		close(stop)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return")
		}
	})
	return SocketPath()
}

// badBuild is a candidate binary that says its build and then fails to
// serve: what a broken new build looks like to an upgrade.
func badBuild(t *testing.T, build string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "poly-lsp-mcp")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$2\" = \"--build-id\" ]; then echo %s; exit 0; fi\necho broken >&2\nexit 3\n", build)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// An upgrade that fails says so in-band, to every client, with the way
// back; the old daemon keeps serving.
func TestUpgradeNoticesReachTheClient(t *testing.T) {
	root, resolved := goWorkspace(t)
	socket := serveInProcess(t, resolved)
	c := NewClient(socket)
	h := startProxy(t, c, root, OpenOptions{})
	h.reply(h.request("initialize", map[string]any{}), 5*time.Second)
	h.logged(indexLogger, 30*time.Second, nil) // the stream is live

	before, _ := c.health()
	a, err := c.RequestUpgrade(UpgradeRequest{Exe: badBuild(t, "bad/00000001"), Build: "bad/00000001", Why: "test"})
	if err != nil || !a.Accepted {
		t.Fatalf("upgrade request: %+v %v", a, err)
	}
	up := h.logged(daemonLogger, 10*time.Second, func(d map[string]any) bool { return d["state"] == "upgrading" })
	if up["to"] != "bad/00000001" || up["from"] != BuildID() || strings.Contains(fmt.Sprint(up["message"]), "\n") {
		t.Errorf("upgrading notice: %v", up)
	}
	failed := h.logged(daemonLogger, 30*time.Second, func(d map[string]any) bool { return d["state"] == "failed" })
	if !strings.Contains(fmt.Sprint(failed["error"]), "exited before it was warm") || failed["recover"] != recoverHint ||
		!strings.Contains(fmt.Sprint(failed["message"]), recoverHint) {
		t.Errorf("failed notice: %v", failed)
	}

	after, err := c.health()
	if err != nil || after.PID != before.PID || after.Build != BuildID() {
		t.Fatalf("the old daemon stopped serving: %+v %v", after, err)
	}
	text, isErr := callText(t, h.reply(h.request("tools/call", map[string]any{"name": "node_query", "arguments": map[string]any{"selector": "#F4"}}), 30*time.Second))
	if isErr || !strings.Contains(text, "F4") {
		t.Fatalf("call after the failed upgrade: isErr=%v %q", isErr, text)
	}
}
