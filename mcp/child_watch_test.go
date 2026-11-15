package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// The children hear only about paths this server watches: nothing inside
// a skipped or gitignored dir, nothing outside the root, no chmod. A
// skipped dir's own create is kept (tsserver needs node_modules to
// appear).
func TestChildFileEventFilter(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	for _, d := range []string{"node_modules/foo", "src", "gen", ".claude"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("gen/\n*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	s := New(reg, dir, nil, nil)
	s.setIgnores(s.loadIgnores(dir))

	j := func(rel string) string { return filepath.Join(dir, rel) }
	cases := []struct {
		name string
		op   fsnotify.Op
		want multiplex.FileChangeType // 0 = dropped
	}{
		{j("package.json"), fsnotify.Write, multiplex.FileChanged},
		{j("src/a.ts"), fsnotify.Create, multiplex.FileCreated},
		{j("src/a.ts"), fsnotify.Remove, multiplex.FileDeleted},
		{j("src/a.ts"), fsnotify.Rename, multiplex.FileDeleted},
		{j("src/a.ts"), fsnotify.Chmod, 0},
		{j("node_modules"), fsnotify.Create, multiplex.FileCreated},
		{j("node_modules/foo/index.d.ts"), fsnotify.Create, 0},
		{j(".claude/x.json"), fsnotify.Write, 0},
		{j("gen/out.ts"), fsnotify.Write, 0},
		{j("debug.log"), fsnotify.Write, 0},
		{"/elsewhere/a.ts", fsnotify.Write, 0},
		{dir, fsnotify.Write, 0},
	}
	for _, c := range cases {
		ev, ok := s.childFileEvent(fsnotify.Event{Name: c.name, Op: c.op})
		var got multiplex.FileChangeType
		if ok {
			got = ev.Type
		}
		if got != c.want {
			t.Errorf("childFileEvent(%s, %v) = %d, want %d", c.name, c.op, got, c.want)
		}
	}
}

func TestChildEventBatchCoalesces(t *testing.T) {
	var b childEventBatch
	if !b.add(multiplex.FileEvent{Path: "/r/a", Type: multiplex.FileCreated}) {
		t.Fatal("first add did not report an empty batch")
	}
	if b.add(multiplex.FileEvent{Path: "/r/a", Type: multiplex.FileChanged}) {
		t.Fatal("second add reported an empty batch")
	}
	b.add(multiplex.FileEvent{Path: "/r/b", Type: multiplex.FileDeleted})
	b.add(multiplex.FileEvent{Path: "/r/b", Type: multiplex.FileCreated})
	b.add(multiplex.FileEvent{Path: "/r/c", Type: multiplex.FileChanged})
	b.add(multiplex.FileEvent{Path: "/r/c", Type: multiplex.FileDeleted})
	got := b.take()
	want := []multiplex.FileEvent{
		{Path: "/r/a", Type: multiplex.FileCreated},
		{Path: "/r/b", Type: multiplex.FileChanged},
		{Path: "/r/c", Type: multiplex.FileDeleted},
	}
	if len(got) != len(want) {
		t.Fatalf("take = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("take = %v, want %v", got, want)
		}
	}
	if len(b.take()) != 0 {
		t.Fatal("take did not empty the batch")
	}
}

// A package manifest change brings a Changed event for the node_modules
// beside it, when that exists: the measured smallest event that makes
// tsserver re-resolve after an install.
func TestWithNodeModulesEvents(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	nm := filepath.Join(dir, "node_modules")
	got := withNodeModulesEvents([]multiplex.FileEvent{
		{Path: filepath.Join(dir, "package.json"), Type: multiplex.FileChanged},
		{Path: filepath.Join(dir, "package-lock.json"), Type: multiplex.FileChanged},
		// sub has no node_modules: nothing added for it.
		{Path: filepath.Join(dir, "sub", "yarn.lock"), Type: multiplex.FileChanged},
		{Path: filepath.Join(dir, "src.ts"), Type: multiplex.FileChanged},
	})
	n := 0
	for _, ev := range got {
		if ev.Path == nm {
			n++
			if ev.Type != multiplex.FileChanged {
				t.Errorf("node_modules event type %d, want Changed", ev.Type)
			}
		}
	}
	if n != 1 || len(got) != 5 {
		t.Fatalf("want 4 events plus one node_modules event, got %v", got)
	}

	// Without a manifest event, nothing is added.
	if got := withNodeModulesEvents([]multiplex.FileEvent{{Path: filepath.Join(dir, "a.ts"), Type: multiplex.FileChanged}}); len(got) != 1 {
		t.Fatalf("got %v", got)
	}
}

// A Changed event for an open document whose bytes the child already has
// is the echo of the tool's own write: dropped.
func TestDropEchoes(t *testing.T) {
	dir := t.TempDir()
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	s := New(reg, dir, nil, nil)
	echo := filepath.Join(dir, "a.ts")
	external := filepath.Join(dir, "b.ts")
	for _, p := range []string{echo, external} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s.recordSent(pathToURI(echo), []byte("x"))
	s.recordSent(pathToURI(external), []byte("old"))
	got := s.dropEchoes([]multiplex.FileEvent{
		{Path: echo, Type: multiplex.FileChanged},
		{Path: external, Type: multiplex.FileChanged},
		{Path: echo, Type: multiplex.FileDeleted},
	})
	if len(got) != 2 || got[0].Path != external || got[1].Type != multiplex.FileDeleted {
		t.Fatalf("dropEchoes = %v", got)
	}
}
