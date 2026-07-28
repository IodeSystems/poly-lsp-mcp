package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func newIgnoreRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"),
		[]byte("generated/\n*.gen.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", dir, "init", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

// The snapshot from LoadIgnores only enumerates paths that EXISTED when
// it ran. A build output or a branch switch creates ignored files
// afterwards, so those must still be recognised — otherwise the watcher
// re-indexes exactly the files the walk skipped.
func TestIgnoreSetMatchesPathsCreatedAfterLoad(t *testing.T) {
	dir := newIgnoreRepo(t)
	ig := LoadIgnores(dir)
	if ig == nil {
		t.Fatal("expected an ignore set for a git repo")
	}

	// Created AFTER the load.
	if err := os.MkdirAll(filepath.Join(dir, "generated"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"generated/dump.go", "thing.gen.go"} {
		abs := filepath.Join(dir, rel)
		if err := os.WriteFile(abs, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !ig.FileIgnored(dir, abs) {
			t.Errorf("%s created after load should still be ignored", rel)
		}
	}
	if ig.FileIgnored(dir, filepath.Join(dir, "real.go")) {
		t.Error("a non-matching path must not be ignored")
	}
}

func TestIgnoreSetNilIsSafe(t *testing.T) {
	var ig *IgnoreSet
	if ig.FileIgnored("/x", "/x/y.go") || ig.DirIgnored("/x", "/x/y") {
		t.Error("a nil set must report nothing ignored — that is what keeps " +
			"non-git workspaces walking unfiltered")
	}
}
