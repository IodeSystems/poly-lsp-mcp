package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// startTestDaemon boots a daemon on a short temp socket (os.MkdirTemp is
// under /tmp, safely below the 108-byte sun_path limit) hosting the given
// allow prefix, and returns a client + a stop func. No child LSPs are
// configured, so it's index-only and fast.
func startTestDaemon(t *testing.T, allowRoot string) (*Client, func()) {
	t.Helper()
	sockDir := t.TempDir()
	socket := filepath.Join(sockDir, "d.sock")

	cfg, _, err := config.LoadOrDefault("nonexistent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}

	ln, err := listenUnix(socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	handler, err := buildHandler(NewAllowList([]string{allowRoot}), NewRegistry(cfg, reg, false, false))
	if err != nil {
		t.Fatal(err)
	}
	// Build the server inline rather than calling the exported Serve —
	// Serve writes the real state file and installs signal handlers,
	// neither of which a test wants.
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()

	c := NewClient(socket)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.Healthy() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !c.Healthy() {
		_ = ln.Close()
		t.Fatal("daemon did not become healthy")
	}
	return c, func() { _ = ln.Close() }
}

func TestDaemonEndToEnd(t *testing.T) {
	// A tiny workspace with one Go file so the index has something.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"),
		[]byte("package main\n\nfunc Hello() string { return \"hi\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// listenUnix's credListener needs the resolved root for the allow
	// check; t.TempDir on macOS is a symlink, so resolve it.
	resolved, _ := resolvePath(root)

	c, stop := startTestDaemon(t, resolved)
	defer stop()

	// health
	if !c.Healthy() {
		t.Fatal("not healthy")
	}

	// open the allowed root
	names, err := c.Open(root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if names == 0 {
		t.Error("open reported 0 indexed names for a non-empty workspace")
	}

	// tools/list
	tools, err := c.Tools(root)
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if len(tools) != 3 {
		t.Errorf("got %d tools, want 3 (modern surface)", len(tools))
	}

	// a real tool call
	content, isErr, err := c.Call(root, "node_query", json.RawMessage(`{"selector":":root > *","limit":5}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if isErr {
		t.Errorf("node_query returned isError: %+v", content)
	}
	if len(content) == 0 || content[0].Text == "" {
		t.Error("node_query returned empty content")
	}

	// a root OUTSIDE the allow prefix is refused
	if _, err := c.Open("/etc"); err == nil {
		t.Error("open(/etc) succeeded — trust gate not enforced")
	}

	// FileSymbols: content-first, no root/file access. Language derived
	// from the path extension.
	lang, syms, err := c.FileSymbols("package p\n\n// Doc.\nfunc F() int { return 1 }\n", "", "x.go")
	if err != nil {
		t.Fatalf("filesymbols: %v", err)
	}
	if lang != "go" {
		t.Errorf("derived language %q, want go", lang)
	}
	var foundF bool
	for _, s := range syms {
		if s.Sym == "F" && s.Class == "func" {
			foundF = true
			if s.BodyStartLine == 0 {
				t.Error("F has no BodyStartLine (fragment boundary lost)")
			}
			if s.CommentStartLine == 0 {
				t.Error("F lost its doc-comment span")
			}
		}
	}
	if !foundF {
		t.Errorf("FileSymbols did not return func F; got %d symbols", len(syms))
	}

	// An unknown extension with no explicit language is a 400.
	if _, _, err := c.FileSymbols("plain", "", "x.unknownext"); err == nil {
		t.Error("FileSymbols accepted content with no derivable language")
	}
}

// TestDaemonRollsBackBatchOnDisconnect: a client stages an edit (commit:
// false, so it's on disk but uncommitted) and then vanishes. The daemon
// watches the client's /session/watch connection; when it drops, the
// session's open batch is auto-rolled-back so the broken intermediate does
// not stay stranded on disk — the failure mode the daemon introduces by
// splitting the client and server lifetimes.
func TestDaemonRollsBackBatchOnDisconnect(t *testing.T) {
	root := t.TempDir()
	mainPath := filepath.Join(root, "main.go")
	orig := []byte("package main\n\nfunc Hello() string { return \"hi\" }\n")
	if err := os.WriteFile(mainPath, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, _ := resolvePath(root)
	c, stop := startTestDaemon(t, resolved)
	defer stop()

	if _, err := c.Open(root); err != nil {
		t.Fatalf("open: %v", err)
	}

	// Stage an edit — lands on disk, not yet committed.
	_, isErr, err := c.Call(root, "node_edit",
		json.RawMessage(`{"node":"main.go#Hello","oldText":"return \"hi\"","newText":"return \"bye\"","commit":false}`))
	if err != nil || isErr {
		t.Fatalf("stage: isErr=%v err=%v", isErr, err)
	}
	if b, _ := os.ReadFile(mainPath); string(b) == string(orig) {
		t.Fatal("staged edit is not on disk")
	}

	// Open the watch, let it register server-side, then drop it (the client
	// vanishing).
	ctx, cancel := context.WithCancel(context.Background())
	watchDone := make(chan struct{})
	go func() { _ = c.WatchSession(ctx); close(watchDone) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-watchDone

	// The daemon rolls back asynchronously once it sees the drop; poll for
	// the restore.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(mainPath); string(b) == string(orig) {
			return // rolled back
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(mainPath)
	t.Fatalf("batch not rolled back on disconnect; file still %q", b)
}

// TestRegistryCachePersistence checks the daemon-owned load/save of the
// shared parse cache round-trips: entries saved by one registry are
// loaded by the next (a restarted daemon comes up warm).
func TestRegistryCachePersistence(t *testing.T) {
	cfg, _, err := config.LoadOrDefault("nonexistent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cache.gob")

	r1 := NewRegistry(cfg, reg, false, false)
	r1.cachePath = path
	// Seed one entry through the shared cache (content-keyed put).
	r1.cache.Put("go", []byte("package p\nfunc F(){}\n"), nil)
	if r1.cache.Len() == 0 {
		t.Fatal("cache Put did not register an entry")
	}
	r1.SaveCache()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}

	r2 := NewRegistry(cfg, reg, false, false)
	r2.cachePath = path
	r2.LoadCache()
	if r2.cache.Len() != r1.cache.Len() {
		t.Errorf("loaded %d cache entries, want %d", r2.cache.Len(), r1.cache.Len())
	}
}

func testRegistryDeps(t *testing.T) (*config.Config, *config.Registry) {
	t.Helper()
	cfg, _, err := config.LoadOrDefault("nonexistent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	return cfg, reg
}

func hasRoot(r *Registry, root string) bool {
	for _, x := range r.Roots() {
		if x == root {
			return true
		}
	}
	return false
}

func waitRootGone(r *Registry, root string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !hasRoot(r, root) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// A root stays warm while ANY session holds it and is evicted (its server —
// index + child LSPs + watcher — shut down) idleTimeout after the LAST holder
// leaves. This is step 5's resource win: an abandoned root stops pinning a
// gopls fleet.
func TestRegistryRefCountEviction(t *testing.T) {
	cfg, reg := testRegistryDeps(t)
	r := NewRegistry(cfg, reg, false, false)
	r.idleTimeout = 80 * time.Millisecond
	defer r.Close()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, _ := resolvePath(root)

	if _, err := r.Acquire("s1", resolved); err != nil {
		t.Fatalf("acquire s1: %v", err)
	}
	if _, err := r.Acquire("s2", resolved); err != nil {
		t.Fatalf("acquire s2: %v", err)
	}
	if !hasRoot(r, resolved) {
		t.Fatal("root not open after acquire")
	}

	// One holder leaves — s2 still holds it, so no eviction even past idle.
	r.Release("s1")
	time.Sleep(3 * r.idleTimeout)
	if !hasRoot(r, resolved) {
		t.Fatal("evicted while still held by s2")
	}

	// Last holder leaves — evicted after idle.
	r.Release("s2")
	if !waitRootGone(r, resolved, 3*time.Second) {
		t.Fatal("root not evicted after last holder left")
	}
}

// Re-acquiring an unheld root before its idle timer fires cancels the pending
// eviction — a client reconnecting keeps its warm index.
func TestRegistryReacquireCancelsEviction(t *testing.T) {
	cfg, reg := testRegistryDeps(t)
	r := NewRegistry(cfg, reg, false, false)
	r.idleTimeout = 100 * time.Millisecond
	defer r.Close()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, _ := resolvePath(root)

	if _, err := r.Acquire("s1", resolved); err != nil {
		t.Fatalf("acquire s1: %v", err)
	}
	r.Release("s1")               // schedules eviction
	if _, err := r.Acquire("s2", resolved); err != nil {
		t.Fatalf("re-acquire s2: %v", err)
	}
	time.Sleep(3 * r.idleTimeout) // past the original deadline
	if !hasRoot(r, resolved) {
		t.Fatal("re-acquire did not cancel the pending eviction")
	}
}

// TestListenUnixReclaimsStaleSocket confirms a leftover socket file with
// no listener is removed and rebound (the kill -9 recovery path).
func TestListenUnixReclaimsStaleSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "d.sock")
	// Create a stale socket file that nothing is listening on: bind then
	// close the underlying listener but leave the file (simulate crash).
	ln1, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	// net.UnixListener removes the file on Close by default; recreate a
	// bare file to stand in for the crash-leftover.
	_ = ln1.Close()
	if err := os.WriteFile(socket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln2, err := listenUnix(socket)
	if err != nil {
		t.Fatalf("listenUnix did not reclaim stale socket: %v", err)
	}
	_ = ln2.Close()
}

// TestListenUnixRefusesLiveSocket confirms a second daemon won't steal a
// socket that is actively served.
func TestListenUnixRefusesLiveSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "d.sock")
	ln1, err := listenUnix(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln1.Close()
	// Accept in the background so Dial in listenUnix connects.
	go func() {
		for {
			c, err := ln1.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if _, err := listenUnix(socket); err == nil {
		t.Error("listenUnix stole a live socket")
	}
}
