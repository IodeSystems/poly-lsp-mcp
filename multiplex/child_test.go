package multiplex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// polyLspMcpBinary is the path of the poly-lsp-mcp binary built during TestMain.
// Tests spawn this binary as the child LSP — it speaks the protocol and
// indexes the directory we point it at, so we can exercise the supervisor
// end-to-end without an external dependency.
var polyLspMcpBinary string

// buildBinaryForTests builds the poly-lsp-mcp binary into dir and returns its
// path, removing dir on the way out if the build fails. The caller owns the
// directory on success: TestMain must remove it, and it must do so on a
// failing run too — a run whose tests fail must leave no temp dir behind.
func buildBinaryForTests(dir string) (string, error) {
	bin := filepath.Join(dir, "poly-lsp-mcp")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/iodesystems/poly-lsp-mcp").CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("build poly-lsp-mcp for tests: %v\n%s", err, out)
	}
	return bin, nil
}

func TestMain(m *testing.M) {
	if dir := os.Getenv(spawnHelperEnv); dir != "" {
		spawnHelper(dir) // never returns
	}
	dir, err := os.MkdirTemp("", "poly-lsp-mcp-mp-test-*")
	if err != nil {
		panic(err)
	}

	polyLspMcpBinary, err = buildBinaryForTests(dir)
	if err != nil {
		panic(err)
	}

	// A signal-killed run (SIGINT from the user, SIGTERM from a CI timeout)
	// dies before the removal below; the handler closes that path. It removes
	// the dir and exits so a signal leaves no poly-lsp-mcp-mp-test-* behind.
	stop := cleanupOnSignal(dir)
	defer stop()

	// TestTempDirRemovedOnSignal sets this so it can tell when the handler is
	// installed, and signal only once the run is past the slow go build.
	if ready := os.Getenv("POLY_LSP_MCP_MP_READY_FILE"); ready != "" {
		_ = os.WriteFile(ready, []byte("ready"), 0o600)
	}

	// os.Exit skips deferred calls, so the directory is removed before it.
	// m.Run returns a non-zero code when any test fails, and the removal
	// runs on that path too: a failed run leaves no poly-lsp-mcp-mp-test-*.
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// cleanupOnSignal installs a SIGINT/SIGTERM handler that removes dir and
// exits, so a signal-killed test run leaves no temp dir behind. It returns
// a function that stops the handler; the normal exit path calls it before
// removing the dir itself. SIGKILL is not catchable, so it is not handled.
func cleanupOnSignal(dir string) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}()
	return func() { signal.Stop(ch) }
}

func polyglotDir(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	abs, err := filepath.Abs(filepath.Join(filepath.Dir(here), "..", "testdata", "fixtures", "polyglot"))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestSpawnAndHandshake(t *testing.T) {
	child, err := Spawn("test", &config.LSP{Cmd: polyLspMcpBinary}, t.TempDir())
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	caps, err := child.Initialize(ctx, "file://"+t.TempDir())
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if len(caps) == 0 {
		t.Fatal("empty capabilities from child")
	}
	var capsMap map[string]any
	if err := json.Unmarshal(caps, &capsMap); err != nil {
		t.Fatalf("decode caps: %v", err)
	}
	// poly-lsp-mcp child advertises these once the symbol-index wiring lands.
	if capsMap["workspaceSymbolProvider"] != true {
		t.Errorf("workspaceSymbolProvider missing from child caps: %+v", capsMap)
	}
	if capsMap["referencesProvider"] != true {
		t.Errorf("referencesProvider missing from child caps: %+v", capsMap)
	}

	if err := child.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := child.Wait(); err != nil {
		t.Errorf("Wait: %v", err)
	}
	if err := child.Err(); err != nil {
		t.Errorf("Err after clean exit: %v", err)
	}
}

func TestCallRoutesThroughChildLSP(t *testing.T) {
	fixture := polyglotDir(t)
	child, err := Spawn("polyglot", &config.LSP{Cmd: polyLspMcpBinary}, fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Kill()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // the stub child indexes the fixture before answering; 5s flaked on macos-15-intel
	defer cancel()

	if _, err := child.Initialize(ctx, "file://"+fixture); err != nil {
		t.Fatal(err)
	}

	raw, err := child.Call(ctx, "workspace/symbol", map[string]any{"query": "UserID"})
	if err != nil {
		t.Fatalf("Call workspace/symbol: %v", err)
	}

	var syms []map[string]any
	if err := json.Unmarshal(raw, &syms); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(syms) < 6 {
		t.Errorf("got %d UserID syms via multiplex, want >= 6", len(syms))
	}
	// workspace/symbol does case-insensitive substring match, so names
	// like "users_UserID_idx" are valid hits. Just require the query
	// substring to appear in each.
	for _, s := range syms {
		name, _ := s["name"].(string)
		if !strings.Contains(strings.ToLower(name), "userid") {
			t.Errorf("symbol %q does not contain query UserID", name)
		}
	}

	if err := child.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	child.Wait()
}

func TestCallReturnsErrorAfterChildExit(t *testing.T) {
	child, err := Spawn("test", &config.LSP{Cmd: polyLspMcpBinary}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := child.Initialize(ctx, "file://"+t.TempDir()); err != nil {
		t.Fatal(err)
	}

	if err := child.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case <-child.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done did not close after Kill")
	}
	child.Wait()

	if _, err := child.Call(ctx, "workspace/symbol", map[string]any{}); err == nil {
		t.Error("Call after Kill should return an error")
	}
}

func TestCallReturnsErrorOnContextCancel(t *testing.T) {
	fixture := polyglotDir(t)
	child, err := Spawn("polyglot", &config.LSP{Cmd: polyLspMcpBinary}, fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		child.Kill()
		child.Wait()
	}()

	initCtx, initCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer initCancel()
	if _, err := child.Initialize(initCtx, "file://"+fixture); err != nil {
		t.Fatal(err)
	}

	// Issue a call with an already-canceled context; expect ctx.Err.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = child.Call(canceled, "workspace/symbol", map[string]any{"query": ""})
	if err == nil {
		t.Fatal("expected ctx.Err, got nil")
	}
}

func TestSpawnRejectsMissingBinary(t *testing.T) {
	_, err := Spawn("nope", &config.LSP{Cmd: "/no/such/binary"}, t.TempDir())
	if err == nil {
		t.Fatal("expected Spawn to fail for missing binary")
	}
}

func TestSpawnRejectsNilConfig(t *testing.T) {
	_, err := Spawn("nope", nil, t.TempDir())
	if err == nil {
		t.Fatal("expected Spawn to fail for nil config")
	}
}

// TestTempDirCleanupOnFailureSentinel fails on purpose, but only when its
// env var is set: TestTempDirRemovedOnFailedRun sets it on the child so the
// run genuinely fails mid-test, without making this package red on every
// normal run.
func TestTempDirCleanupOnFailureSentinel(t *testing.T) {
	if os.Getenv("POLY_LSP_MCP_MP_SENTINEL_FAIL") != "1" {
		t.Skip("sentinel: only fails when POLY_LSP_MCP_MP_SENTINEL_FAIL=1")
	}
	t.Fatal("sentinel: expected to fail")
}

// TestTempDirRemovedOnFailedRun runs this package's test binary with only the
// failing sentinel test and asserts it leaves no poly-lsp-mcp-mp-test-* dir
// in os.TempDir. This is the failure-proof for the cleanup in TestMain: a
// regression that removes the dir only on success re-creates the leak this
// node exists to close (46 GB of leftover dirs, user quota exhausted).
func TestTempDirRemovedOnFailedRun(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller path")
	}
	abs, err := filepath.Abs(filepath.Dir(thisFile))
	if err != nil {
		t.Fatal(err)
	}

	before, err := tempTestDirs("poly-lsp-mcp-mp-test-")
	if err != nil {
		t.Fatal(err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A compiled test binary selects tests with -test.run, not a subcommand.
	// Stdin is closed: the child must not consume the harness's stdin.
	cmd := exec.Command(exe, "-test.run", "TestTempDirCleanupOnFailureSentinel", "-test.count", "1")
	cmd.Dir = abs
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), "POLY_LSP_MCP_MP_SENTINEL_FAIL=1")
	out, rerr := cmd.CombinedOutput()
	if rerr == nil {
		t.Fatalf("child test run should have failed, output:\n%s", out)
	}
	// The sentinel must be the failure, not a build or TestMain panic.
	if !strings.Contains(string(out), "sentinel: expected to fail") {
		t.Fatalf("child run failed for the wrong reason:\n%s", out)
	}

	after, err := tempTestDirs("poly-lsp-mcp-mp-test-")
	if err != nil {
		t.Fatal(err)
	}
	for d := range after {
		if _, seen := before[d]; !seen {
			t.Fatalf("failing run left a temp dir behind: %s", d)
		}
	}
}

// TestTempDirCleanupOnSignalSentinel blocks forever: TestTempDirRemovedOnSignal
// sends the child run SIGINT while it is stuck here, so the run is killed by a
// signal rather than exiting on its own.
func TestTempDirCleanupOnSignalSentinel(t *testing.T) {
	if os.Getenv("POLY_LSP_MCP_MP_SENTINEL_SIGNAL") != "1" {
		t.Skip("sentinel: only runs when POLY_LSP_MCP_MP_SENTINEL_SIGNAL=1")
	}
	<-make(chan struct{}) // block until the test sends SIGINT
}

// TestTempDirRemovedOnSignal runs this package's test binary with only the
// blocking sentinel test, sends it SIGINT, and asserts it leaves no
// poly-lsp-mcp-mp-test-* dir in os.TempDir. This is the signal-proof for the
// cleanup in TestMain: a regression that removes the dir only on a normal
// exit re-creates the leak a SIGINT/SIGTERM-killed run would otherwise leave.
func TestTempDirRemovedOnSignal(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller path")
	}
	abs, err := filepath.Abs(filepath.Dir(thisFile))
	if err != nil {
		t.Fatal(err)
	}

	before, err := tempTestDirs("poly-lsp-mcp-mp-test-")
	if err != nil {
		t.Fatal(err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "handler-ready")
	cmd := exec.Command(exe, "-test.run", "TestTempDirCleanupOnSignalSentinel", "-test.count", "1")
	cmd.Dir = abs
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), "POLY_LSP_MCP_MP_SENTINEL_SIGNAL=1", "POLY_LSP_MCP_MP_READY_FILE="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// The ready file is written after TestMain installs the signal handler,
	// which is past the slow go build. Wait for it so the signal reaches the
	// handler, not the default disposition.
	deadline := time.Now().Add(120 * time.Second)
	for fileExists(ready) == false && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if !fileExists(ready) {
		cmd.Process.Kill()
		t.Fatal("child test run never installed its signal handler")
	}

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("child test run should have exited non-zero on SIGINT")
	}

	after, err := tempTestDirs("poly-lsp-mcp-mp-test-")
	if err != nil {
		t.Fatal(err)
	}
	for d := range after {
		if _, seen := before[d]; !seen {
			t.Fatalf("signal-killed run left a temp dir behind: %s", d)
		}
	}
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// tempTestDirs returns the set of directories in os.TempDir with the given
// prefix.
func tempTestDirs(prefix string) (map[string]bool, error) {
	set := map[string]bool{}
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			set[filepath.Join(os.TempDir(), e.Name())] = true
		}
	}
	return set, nil
}
