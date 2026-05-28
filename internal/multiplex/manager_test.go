package multiplex

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/tslsmcp/internal/config"
)

// makeTestRegistry builds a Config / Registry with one synthetic language
// whose LSP "binary" is the tslsmcp binary built by TestMain. The
// extension is chosen to avoid clashing with the default registry so
// routing tests are unambiguous.
func makeTestRegistry(t *testing.T) *config.Registry {
	t.Helper()
	cfg := &config.Config{Languages: []config.Language{
		{
			Name:       "stub",
			Extensions: []string{"stub"},
			LSP:        &config.LSP{Cmd: tslsmcpBinary},
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
		{Name: "go", Extensions: []string{"go", "mod"}, LSP: &config.LSP{Cmd: tslsmcpBinary}, TreeSitter: "go"},
		{Name: "typescript", Extensions: []string{"ts", "tsx"}, LSP: &config.LSP{Cmd: tslsmcpBinary}, TreeSitter: "typescript"},
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
