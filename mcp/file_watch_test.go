package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// newIndexedServer builds a registry + index over dir and returns a
// Server wired to them, with the LSP manager and the git/proactive
// kicks left off so tests exercise only the index path.
func newIndexedServer(t *testing.T, dir string) *Server {
	t.Helper()
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetGitPrewarm(false)
	srv.SetProactiveOpen(false)
	idx, err := symbols.Build(dir, reg)
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	srv.setIndex(idx)
	return srv
}

func TestWatchableFile(t *testing.T) {
	srv := newIndexedServer(t, t.TempDir())
	cases := map[string]bool{
		"a.go": true, "b.ts": true, "c.py": true,
		"notes.txt": false, "data.bin": false, "noext": false,
	}
	for name, want := range cases {
		if got := srv.watchableFile(filepath.Join("/x", name)); got != want {
			t.Errorf("watchableFile(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestWatchRefreshFileUpdatesIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("package p\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := newIndexedServer(t, dir)
	idx := srv.getIndex()
	if len(idx.Lookup("Foo")) == 0 {
		t.Fatal("setup: Foo not indexed")
	}

	// Rewrite the file out-of-band, then refresh as the watcher would.
	if err := os.WriteFile(path, []byte("package p\nfunc Bar() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv.watchRefreshFile(path)

	if len(idx.Lookup("Bar")) == 0 {
		t.Error("Bar not in index after refresh")
	}
	if len(idx.Lookup("Foo")) != 0 {
		t.Error("stale Foo still in index after refresh")
	}
}

func TestWatchRefreshFileVanishedIsRemoval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("package p\nfunc Zed() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := newIndexedServer(t, dir)
	if len(srv.getIndex().Lookup("Zed")) == 0 {
		t.Fatal("setup: Zed not indexed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	srv.watchRefreshFile(path) // read fails → treated as removal
	if got := srv.getIndex().Lookup("Zed"); len(got) != 0 {
		t.Errorf("Zed still indexed after its file vanished: %+v", got)
	}
}

// TestFileWatchEndToEnd drives the real fsnotify path: a file created
// after the watcher starts should land in the index without any tool
// call. Polled with a generous deadline so a slow box is slow, not
// flaky.
func TestFileWatchEndToEnd(t *testing.T) {
	dir := t.TempDir()
	srv := newIndexedServer(t, dir)
	srv.kickFileWatch()
	defer func() {
		srv.stopFileWatch()
		_ = srv.WaitForFileWatch(mustCtx(t, time.Second))
	}()

	// Give the watcher a moment to register its watch descriptors.
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package p\nfunc Fresh() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(srv.getIndex().LookupExisting("Fresh")) > 0 {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("watcher did not index a newly-created file within 3s")
}

func mustCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
