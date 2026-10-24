package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
)

// testSocket is a socket path for one test, in a fresh directory under /tmp
// removed at cleanup. Not t.TempDir: on macOS that is under a long
// /var/folders/... and with the test's name appended it passes the 104-byte
// sun_path, so bind fails.
func testSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

// startTestDaemon boots a daemon on a short temp socket (testSocket)
// hosting the given allow prefix, and returns a client + a stop func. No
// child LSPs are configured, so it's index-only and fast.
func startTestDaemon(t *testing.T, allowRoot string) (*Client, func()) {
	t.Helper()
	socket := testSocket(t)

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

	// a root OUTSIDE the allow prefix, never opened, is refused by the
	// entry points that take a root without an open
	if _, err := c.Query(QueryParams{Root: "/etc", Selector: "*"}); err == nil {
		t.Error("query(/etc) succeeded — trust gate not enforced")
	}
	if _, _, err := c.Call("/etc", "node_query", json.RawMessage(`{"selector":"*"}`)); err == nil {
		t.Error("call(/etc) succeeded — trust gate not enforced")
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

// A root opened outside every allowed prefix is added to the allow list
// (the socket admits only this uid, which could run poly-lsp on it
// in-process anyway): the open succeeds, node_query answers, and the
// entry points that do not open (/query) admit it afterwards.
func TestOpenOutsideTheAllowListAddsTheRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"),
		[]byte("package main\n\nfunc Hello() string { return \"hi\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, stop := startTestDaemon(t, t.TempDir()) // allows another directory
	defer stop()

	if _, err := c.Query(QueryParams{Root: root, Selector: "#Hello"}); err == nil {
		t.Fatal("query on a root never opened and outside the allow list succeeded")
	}
	// The catalog names no root's content: a proxy asks for it before its
	// open lands, so it is not gated.
	if _, err := c.Tools(root); err != nil {
		t.Fatalf("tools before the open: %v", err)
	}
	if names, err := c.Open(root); err != nil || names == 0 {
		t.Fatalf("open outside the allow list: names=%d err=%v", names, err)
	}
	content, isErr, err := c.Call(root, "node_query", json.RawMessage(`{"selector":"#Hello"}`))
	if err != nil || isErr || len(content) == 0 || !strings.Contains(content[0].Text, "Hello") {
		t.Fatalf("node_query after the open: isErr=%v err=%v %+v", isErr, err, content)
	}
	if out, err := c.Query(QueryParams{Root: root, Selector: "#Hello"}); err != nil || !strings.Contains(out, "Hello") {
		t.Fatalf("query after the open (root now allowed): %q %v", out, err)
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

// TestDaemonReadOnlyIsPerConnection: the same shared, read-write daemon serves
// a read-write catalog and lets edits through, but a connection that declares
// read-only gets a filtered catalog and is refused mutating tools — the policy
// is per-connection, enforced at the boundary, not global.
func TestDaemonReadOnlyIsPerConnection(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"),
		[]byte("package main\n\nfunc Hello() string { return \"hi\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, _ := resolvePath(root)
	c, stop := startTestDaemon(t, resolved) // daemon baseline is read-WRITE
	defer stop()
	if _, err := c.Open(root); err != nil {
		t.Fatalf("open: %v", err)
	}

	hasEdit := func(tools []mcp.ToolDescriptor) bool {
		for _, tl := range tools {
			if tl.Name == "node_edit" {
				return true
			}
		}
		return false
	}

	// Read-WRITE connection (default policy): node_edit is in the catalog.
	tools, err := c.Tools(root)
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if !hasEdit(tools) {
		t.Fatal("read-write catalog should list node_edit")
	}

	// Same connection turned read-only: catalog filtered, node_edit refused.
	c.SetPolicy(true, false)
	tools, err = c.Tools(root)
	if err != nil {
		t.Fatalf("tools (read-only): %v", err)
	}
	if hasEdit(tools) {
		t.Fatal("read-only catalog must not list node_edit")
	}
	content, isErr, err := c.Call(root, "node_edit",
		json.RawMessage(`{"node":"main.go#Hello","newText":"x"}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !isErr || len(content) == 0 || !strings.Contains(content[0].Text, "read-only") {
		t.Fatalf("read-only connection must be refused node_edit; got isErr=%v %+v", isErr, content)
	}
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

// A root stays while ANY session holds it; a client-less root is in the
// LRU, and with --idle-roots-mb 0 it is evicted (its server — index +
// child LSPs + watcher — shut down) as soon as its LAST holder leaves.
func TestRegistryEvictsOnlyClientlessRoots(t *testing.T) {
	cfg, reg := testRegistryDeps(t)
	r := NewRegistry(cfg, reg, false, false)
	r.SetRootLifetime(time.Hour, 0)
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

	// One holder leaves — s2 still holds it: never evicted, whatever the cap.
	r.Release("s1")
	time.Sleep(50 * time.Millisecond)
	if !hasRoot(r, resolved) {
		t.Fatal("evicted while still held by s2")
	}

	// Last holder leaves — over a cap of 0, so evicted.
	r.Release("s2")
	if !waitRootGone(r, resolved, 3*time.Second) {
		t.Fatal("root not evicted after last holder left")
	}
}

// A client-less root's child LSPs stop --root-lsp-grace after its last
// client left; the index stays. A client coming back before the grace
// ends cancels the stop.
func TestRegistryStopsLSPsAfterTheGrace(t *testing.T) {
	cfg, reg := testRegistryDeps(t)
	r := NewRegistry(cfg, reg, false, false)
	const grace = 100 * time.Millisecond
	r.SetRootLifetime(grace, -1)
	defer r.Close()
	_, resolved := goWorkspace(t)
	stopped := func() bool {
		st, _ := r.RootStats()
		return len(st) == 1 && st[0].LSPsStopped
	}

	if _, err := r.Acquire("s1", resolved); err != nil {
		t.Fatalf("acquire s1: %v", err)
	}
	r.Release("s1")
	if _, err := r.Acquire("s2", resolved); err != nil {
		t.Fatalf("re-acquire s2: %v", err)
	}
	time.Sleep(3 * grace) // past the first release's grace
	if stopped() {
		t.Fatal("a client coming back did not cancel the child-LSP stop")
	}

	r.Release("s2")
	deadline := time.Now().Add(5 * time.Second)
	for !stopped() {
		if time.Now().After(deadline) {
			t.Fatal("child LSPs not stopped after the grace")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !hasRoot(r, resolved) {
		t.Fatal("the index went with the child LSPs")
	}
	// A client coming back clears it; its children start on demand.
	if _, err := r.Acquire("s3", resolved); err != nil {
		t.Fatal(err)
	}
	if stopped() {
		t.Error("still reported stopped with a client")
	}
}

// The client-less roots are an LRU bounded by their resident estimates:
// past --idle-roots-mb the least recently used goes; a root with a client
// stays whatever its size.
func TestRegistryIdleRootsLRU(t *testing.T) {
	cfg, reg := testRegistryDeps(t)
	r := NewRegistry(cfg, reg, false, false)
	defer r.Close()
	mk := func(n int) string {
		root := t.TempDir()
		src := "package main\n\nfunc F() {}\n" + strings.Repeat("// pad\n", n)
		if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		resolved, _ := resolvePath(root)
		return resolved
	}
	a, b, big := mk(100), mk(100), mk(1000)
	size := func(root string) int64 {
		st, _ := r.RootStats()
		for _, s := range st {
			if s.Root == root {
				return s.Resident
			}
		}
		t.Fatalf("%s not hosted", root)
		return 0
	}
	for _, root := range []string{a, b, big} {
		if _, err := r.Acquire("s-"+root, root); err != nil {
			t.Fatal(err)
		}
	}
	if size(a) == 0 || size(big) <= size(a) {
		t.Fatalf("estimates: a %d, big %d", size(a), size(big))
	}
	// Room for one small client-less root, not two, and not the big one.
	r.SetRootLifetime(time.Hour, size(a)+size(b)/2)

	r.Release("s-" + big) // over the cap alone: evicted
	if !waitRootGone(r, big, 3*time.Second) {
		t.Fatal("a client-less root over the cap stayed")
	}
	r.Release("s-" + a)
	time.Sleep(10 * time.Millisecond)
	if _, err := r.Get(b); err != nil { // b used last
		t.Fatal(err)
	}
	r.Release("s-" + b) // a and b over the cap: a, used least recently, goes
	if !waitRootGone(r, a, 3*time.Second) || !hasRoot(r, b) {
		t.Fatalf("LRU kept the wrong root: %v", r.Roots())
	}

	// A held root is never evicted, even over the cap.
	r.SetRootLifetime(time.Hour, 1)
	if _, err := r.Acquire("s4", big); err != nil {
		t.Fatal(err)
	}
	r.Release("s-none")
	time.Sleep(50 * time.Millisecond)
	if !hasRoot(r, big) {
		t.Fatal("a root with a client was evicted")
	}
}

// TestListenUnixReclaimsStaleSocket confirms a leftover socket file with
// no listener is removed and rebound (the kill -9 recovery path).
func TestListenUnixReclaimsStaleSocket(t *testing.T) {
	socket := testSocket(t)
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
	socket := testSocket(t)
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

// A zombie (exited, not yet reaped) is not alive: kill(pid, 0) succeeds
// on it, and Restart used to wait 15s for a daemon that had already exited.
func TestPidAliveFalseForZombie(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for !pidZombie(pid) {
		if time.Now().After(deadline) {
			t.Fatal("child never became a zombie")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if PidAlive(pid) {
		t.Fatal("PidAlive true for a zombie")
	}
	if !PidAlive(os.Getpid()) {
		t.Fatal("PidAlive false for self")
	}
}
