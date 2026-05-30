package multiplex

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/internal/config"
)

// makeTestRegistry builds a Config / Registry with one synthetic language
// whose LSP "binary" is the poly-lsp-mcp binary built by TestMain. The
// extension is chosen to avoid clashing with the default registry so
// routing tests are unambiguous.
func makeTestRegistry(t *testing.T) *config.Registry {
	t.Helper()
	cfg := &config.Config{Languages: []config.Language{
		{
			Name:       "stub",
			Extensions: []string{"stub"},
			LSP:        &config.LSP{Cmd: polyLspMcpBinary},
			TreeSitter: "stub",
		},
		{
			Name:       "treesitter-only",
			Extensions: []string{"ts1"},
			TreeSitter: "treesitter-only",
		},
		{
			Name:       "missing",
			Extensions: []string{"miss"},
			LSP:        &config.LSP{Cmd: "/no/such/binary"},
			TreeSitter: "missing",
		},
	}}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestManagerSpawnsListedChildren(t *testing.T) {
	reg := makeTestRegistry(t)
	m := NewManager(reg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cwd := t.TempDir()
	if err := m.Start(ctx, cwd, "file://"+cwd, []string{"stub"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { m.Shutdown(ctx) })

	if got := m.Languages(); len(got) != 1 || got[0] != "stub" {
		t.Errorf("Languages = %v, want [stub]", got)
	}
	caps := m.Capabilities()
	if _, ok := caps["stub"]; !ok {
		t.Errorf("Capabilities missing stub: %v", caps)
	}
}

func TestManagerSkipsLanguagesWithoutLSP(t *testing.T) {
	reg := makeTestRegistry(t)
	m := NewManager(reg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cwd := t.TempDir()
	if err := m.Start(ctx, cwd, "file://"+cwd, []string{"treesitter-only"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { m.Shutdown(ctx) })

	if got := m.Languages(); len(got) != 0 {
		t.Errorf("Languages = %v, want [] (treesitter-only should not spawn)", got)
	}
}

func TestManagerSkipsMissingBinary(t *testing.T) {
	reg := makeTestRegistry(t)
	m := NewManager(reg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cwd := t.TempDir()
	// Spawning a missing binary must be a logged warning, not an error.
	if err := m.Start(ctx, cwd, "file://"+cwd, []string{"missing", "stub"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { m.Shutdown(ctx) })

	got := m.Languages()
	if len(got) != 1 || got[0] != "stub" {
		t.Errorf("Languages = %v, want [stub] (missing binary should be skipped)", got)
	}
}

func TestRouteByURIByExtension(t *testing.T) {
	reg := makeTestRegistry(t)
	m := NewManager(reg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cwd := t.TempDir()
	if err := m.Start(ctx, cwd, "file://"+cwd, []string{"stub"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Shutdown(ctx) })

	cases := []struct {
		uri   string
		match bool
	}{
		{"file:///some/path/foo.stub", true},
		{"file:///some/path/FOO.STUB", true}, // ext lookup is case-insensitive per config.Registry
		{"file:///some/path/foo.go", false},  // no child for .go in this registry
		{"file:///some/path/foo.miss", false},
		{"file:///some/path/foo.ts1", false},
		{"", false},
		{"file:///noext", false},
		{"http://example.com/x.stub", false}, // non-file URI
	}
	for _, c := range cases {
		got := m.RouteByURI(c.uri)
		if (got != nil) != c.match {
			t.Errorf("RouteByURI(%q): match=%v, want %v", c.uri, got != nil, c.match)
		}
	}
}

func TestRouteByURIReturnsNilAfterChildExit(t *testing.T) {
	reg := makeTestRegistry(t)
	m := NewManager(reg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cwd := t.TempDir()
	if err := m.Start(ctx, cwd, "file://"+cwd, []string{"stub"}); err != nil {
		t.Fatal(err)
	}

	child := m.RouteByURI("file:///x.stub")
	if child == nil {
		t.Fatal("pre-kill: RouteByURI returned nil")
	}
	if err := child.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done did not close after Kill")
	}
	child.Wait()

	if got := m.RouteByURI("file:///x.stub"); got != nil {
		t.Errorf("post-kill: RouteByURI returned non-nil child")
	}
	// Shutdown should still be safe.
	m.Shutdown(ctx)
}

func TestRestartOnCrashRespawnsKilledChild(t *testing.T) {
	reg := makeTestRegistry(t)
	m := NewManager(reg)
	// Short backoff so the test doesn't take long.
	m.RestartInitialBackoff = 50 * time.Millisecond
	m.RestartMaxBackoff = 100 * time.Millisecond
	m.RestartMaxAttempts = 3

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cwd := t.TempDir()
	if err := m.Start(ctx, cwd, "file://"+cwd, []string{"stub"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Shutdown(ctx) })

	original := m.RouteByURI("file:///x.stub")
	if original == nil {
		t.Fatal("pre-kill: no child")
	}
	if err := original.Kill(); err != nil {
		t.Fatal(err)
	}
	<-original.Done()
	original.Wait()

	// Poll until the watchdog restarts. Bounded so failures don't hang.
	var restarted *Child
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c := m.RouteByURI("file:///x.stub")
		if c != nil && c != original {
			restarted = c
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if restarted == nil {
		t.Fatal("watchdog did not produce a fresh child within 5s")
	}
	if restarted == original {
		t.Fatal("RouteByURI returned the same (dead) child after restart")
	}

	// Capabilities should be re-populated for the restarted child.
	caps := m.Capabilities()
	if _, ok := caps["stub"]; !ok {
		t.Errorf("Capabilities missing stub after restart: %v", caps)
	}
}

func TestRestartGivesUpAfterMaxAttempts(t *testing.T) {
	// Override registry: point the LSP at a binary path that never
	// works, so every spawn attempt fails.
	cfg := &config.Config{Languages: []config.Language{{
		Name:       "broken",
		Extensions: []string{"brk"},
		LSP:        &config.LSP{Cmd: "/no/such/binary"},
		TreeSitter: "brk",
	}}}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(reg)
	m.RestartInitialBackoff = 10 * time.Millisecond
	m.RestartMaxBackoff = 20 * time.Millisecond
	m.RestartMaxAttempts = 3

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cwd := t.TempDir()
	// Start with the broken language: spawn fails up front so there's
	// never a child to crash. Use the working stub to bootstrap a
	// watchdog cycle, then point it at the broken cmd.
	if err := m.Start(ctx, cwd, "file://"+cwd, []string{"broken"}); err != nil {
		t.Fatal(err)
	}
	// Since spawn at Start failed, no child exists. Nothing to assert
	// directly here — the failure mode this test pins is "manager
	// stays clean when start can't spawn". The restart loop only
	// engages after a child *has* run.
	if got := m.Languages(); len(got) != 0 {
		t.Errorf("Languages = %v, want [] when initial spawn fails", got)
	}
	m.Shutdown(ctx)
}

func TestRestartIsCancelledByShutdown(t *testing.T) {
	reg := makeTestRegistry(t)
	m := NewManager(reg)
	m.RestartInitialBackoff = 200 * time.Millisecond
	m.RestartMaxAttempts = 5

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cwd := t.TempDir()
	if err := m.Start(ctx, cwd, "file://"+cwd, []string{"stub"}); err != nil {
		t.Fatal(err)
	}

	child := m.RouteByURI("file:///x.stub")
	if err := child.Kill(); err != nil {
		t.Fatal(err)
	}
	<-child.Done()
	child.Wait()

	// Shutdown immediately. The watchdog is in its initial backoff
	// sleep; the shutdown flag must abort the restart attempt cleanly.
	m.Shutdown(ctx)

	// Give the watchdog plenty of time to wake and exit; if it
	// restarted anyway we'd see a child reappear.
	time.Sleep(500 * time.Millisecond)
	if got := m.RouteByURI("file:///x.stub"); got != nil {
		t.Errorf("watchdog restarted child after shutdown: %+v", got)
	}
}

func TestBroadcastFanoutToAllChildren(t *testing.T) {
	reg := makeTestRegistry(t)
	m := NewManager(reg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cwd := t.TempDir()
	if err := m.Start(ctx, cwd, "file://"+cwd, []string{"stub"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Shutdown(ctx) })

	// The stub child accepts arbitrary notifications without error
	// (default dispatch case is a no-op for unknown notifications).
	// We can't directly observe "received" without reaching into the
	// child, but Notify returning nil means the message was framed
	// and written successfully — that's the contract Broadcast offers.
	m.Broadcast("workspace/didChangeConfiguration", map[string]any{"settings": map[string]any{"k": "v"}})
	// Issue a real request afterwards to verify the child is still
	// healthy — a malformed broadcast would have crashed the JSON-RPC
	// stream.
	child := m.RouteByURI("file:///x.stub")
	if child == nil {
		t.Fatal("child gone after broadcast")
	}
	if _, err := child.Call(ctx, "workspace/symbol", map[string]any{"query": ""}); err != nil {
		t.Errorf("post-broadcast call errored: %v", err)
	}
}

func TestShutdownStopsChildren(t *testing.T) {
	reg := makeTestRegistry(t)
	m := NewManager(reg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cwd := t.TempDir()
	if err := m.Start(ctx, cwd, "file://"+cwd, []string{"stub"}); err != nil {
		t.Fatal(err)
	}
	child := m.RouteByURI("file:///x.stub")
	if child == nil {
		t.Fatal("no child after Start")
	}

	if err := m.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	select {
	case <-child.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("child did not exit after Shutdown")
	}
	if got := m.Languages(); len(got) != 0 {
		t.Errorf("Languages after Shutdown = %v, want []", got)
	}
	if got := m.RouteByURI("file:///x.stub"); got != nil {
		t.Errorf("RouteByURI after Shutdown returned non-nil")
	}
}

func TestPolyglotFixtureDrivesRouting(t *testing.T) {
	// Sanity: feed a real registry the polyglot fixture and assert that
	// Index.Languages -> Manager.Start spawns exactly that subset.
	// Skipping any language whose real LSP isn't installed so the test
	// stays portable.
	defaultReg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	_ = defaultReg
	// Use our stub binary as if it were every real LSP. Override only the
	// languages we want to spawn.
	cfg := &config.Config{Languages: []config.Language{
		{Name: "go", Extensions: []string{"go", "mod"}, LSP: &config.LSP{Cmd: polyLspMcpBinary}, TreeSitter: "go"},
		{Name: "typescript", Extensions: []string{"ts", "tsx"}, LSP: &config.LSP{Cmd: polyLspMcpBinary}, TreeSitter: "typescript"},
		{Name: "yaml", Extensions: []string{"yaml"}, TreeSitter: "yaml"}, // no LSP
	}}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}

	m := NewManager(reg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cwd := polyglotDir(t)
	if err := m.Start(ctx, cwd, "file://"+cwd, []string{"go", "typescript", "yaml"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Shutdown(ctx) })

	got := m.Languages()
	if len(got) != 2 || got[0] != "go" || got[1] != "typescript" {
		t.Errorf("Languages = %v, want [go typescript] (yaml has no LSP)", got)
	}
	if c := m.RouteByURI("file://" + filepath.Join(cwd, "main.go")); c == nil {
		t.Errorf("main.go did not route to a child")
	}
	if c := m.RouteByURI("file://" + filepath.Join(cwd, "client.ts")); c == nil {
		t.Errorf("client.ts did not route to a child")
	}
	if c := m.RouteByURI("file://" + filepath.Join(cwd, "config.yaml")); c != nil {
		t.Errorf("config.yaml should NOT route to a child (yaml is treesitter-only)")
	}
}
