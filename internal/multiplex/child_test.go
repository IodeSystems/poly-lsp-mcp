package multiplex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/tslsmcp/internal/config"
)

// tslsmcpBinary is the path of the tslsmcp binary built during TestMain.
// Tests spawn this binary as the child LSP — it speaks the protocol and
// indexes the directory we point it at, so we can exercise the supervisor
// end-to-end without an external dependency.
var tslsmcpBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tslsmcp-mp-test-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	tslsmcpBinary = filepath.Join(dir, "tslsmcp")
	out, err := exec.Command("go", "build", "-o", tslsmcpBinary, "github.com/iodesystems/tslsmcp").CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("build tslsmcp for tests: %v\n%s", err, out))
	}

	os.Exit(m.Run())
}

func polyglotDir(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	abs, err := filepath.Abs(filepath.Join(filepath.Dir(here), "..", "..", "testdata", "fixtures", "polyglot"))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestSpawnAndHandshake(t *testing.T) {
	child, err := Spawn("test", &config.LSP{Cmd: tslsmcpBinary}, t.TempDir())
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
	// tslsmcp child advertises these once the symbol-index wiring lands.
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
	child, err := Spawn("polyglot", &config.LSP{Cmd: tslsmcpBinary}, fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Kill()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	child, err := Spawn("test", &config.LSP{Cmd: tslsmcpBinary}, t.TempDir())
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
	child, err := Spawn("polyglot", &config.LSP{Cmd: tslsmcpBinary}, fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		child.Kill()
		child.Wait()
	}()

	initCtx, initCancel := context.WithTimeout(context.Background(), 5*time.Second)
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
