package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "protocol.file.allow=always"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// git decides the file set. A worktree of this repo and an independent
// clone are left out with their reason; a submodule is included; an ignored
// file and .git never appear.
func TestManifestClassifiesNestedRepos(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	lib := t.TempDir()
	run(t, lib, "init", "-q")
	write(t, filepath.Join(lib, "lib.go"), "package lib\n")
	run(t, lib, "add", ".")
	run(t, lib, "commit", "-qm", "lib")

	root := t.TempDir()
	run(t, root, "init", "-q")
	write(t, filepath.Join(root, ".gitignore"), "gen/\n")
	write(t, filepath.Join(root, "main.go"), "package main\n")
	write(t, filepath.Join(root, "gen", "x.go"), "package gen\n")
	run(t, root, "add", ".")
	run(t, root, "commit", "-qm", "init")
	run(t, root, "submodule", "add", "-q", lib, "third_party/lib")
	run(t, root, "worktree", "add", "-q", filepath.Join(root, ".claude", "worktrees", "a1"))
	write(t, filepath.Join(root, "clone", "c.go"), "package c\n")
	run(t, filepath.Join(root, "clone"), "init", "-q")
	write(t, filepath.Join(root, "new.go"), "package main\n") // untracked, not ignored

	m := LoadManifest(root)
	if m == nil {
		t.Fatal("expected a manifest for a git repo")
	}
	for _, want := range []string{"main.go", "new.go", "third_party/lib/lib.go"} {
		if !slices.Contains(m.Files, want) {
			t.Errorf("files should include %s: %v", want, m.Files)
		}
	}
	for _, f := range m.Files {
		if f == "gen/x.go" || filepath.Base(filepath.Dir(f)) == ".git" {
			t.Errorf("ignored or .git path listed: %s", f)
		}
	}
	got := map[string]string{}
	for _, n := range m.Nested {
		got[n.Dir] = n.Reason
	}
	if got[".claude/worktrees/a1"] != NestedWorktree || got["clone"] != NestedRepo || len(got) != 2 {
		t.Errorf("nested = %v", got)
	}
}

func TestManifestNilOutsideARepo(t *testing.T) {
	if LoadManifest(t.TempDir()) != nil {
		t.Error("not a repo: no manifest, the caller walks")
	}
}
