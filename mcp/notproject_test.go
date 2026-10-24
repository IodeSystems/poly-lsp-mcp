package mcp

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// clonesDir is a directory outside any repository holding n clones (a .git
// dir each is all the check looks at) and a loose file.
func clonesDir(t *testing.T, n int) string {
	t.Helper()
	files := map[string]string{"notes.go": "package notes\n\nfunc Loose() {}\n"}
	for i := range n {
		files[fmt.Sprintf("repo%d/.git/HEAD", i)] = "ref: refs/heads/main\n"
		files[fmt.Sprintf("repo%d/main.go", i)] = "package main\n"
	}
	return writeTree(t, files)
}

// A directory of clones is refused before any build: the notification and
// every tool call name the reason, the repositories found, and both ways
// out. --index-anyway indexes it.
func TestDirOfClonesRefused(t *testing.T) {
	dir := clonesDir(t, 3)
	s := startSessionWith(t, dir, nil, nil, func(srv *Server) { srv.SetAsyncInit(true) })
	defer s.close()
	s.request("initialize", map[string]any{})

	r := s.callTool("node_query", map[string]any{"selector": "file"})
	if !r.IsError {
		t.Fatalf("a tool call on a dir of clones should be refused: %+v", r)
	}
	text := r.Content[0].Text
	for _, want := range []string{"not a project", "a directory of 3 git repositories", "repo0, repo1, repo2", "--root " + filepath.Join(dir, "repo0"), "--index-anyway"} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal should contain %q:\n%s", want, text)
		}
	}
	note := indexNote(t, s)
	repos, _ := note["repos"].([]any)
	if note["state"] != "failed" || note["reason"] != "not a project" || len(repos) != 3 ||
		!strings.Contains(fmt.Sprint(note["remedy"]), "--index-anyway") {
		t.Errorf("failure notification: %v", note)
	}

	two := clonesDir(t, 2)
	if err := New(newTestRegistry(t), two, nil, nil).BuildIndex(); err != nil {
		t.Errorf("two clones and a file is a project, not a refusal: %v", err)
	}

	srv := New(newTestRegistry(t), dir, nil, nil)
	srv.SetIndexAnyway(true)
	if err := srv.BuildIndex(); err != nil {
		t.Fatalf("--index-anyway should index it: %v", err)
	}
	if srv.IndexCost().Files == 0 {
		t.Error("--index-anyway: nothing indexed")
	}
}

// The home directory is refused whatever it holds; the check is a stat per
// ancestor and one ReadDir, so it costs milliseconds.
func TestHomeDirRefused(t *testing.T) {
	home := clonesDir(t, 1)
	t.Setenv("HOME", home)
	srv := New(newTestRegistry(t), home, nil, nil)
	start := time.Now()
	err := srv.BuildIndex()
	took := time.Since(start)
	var np *symbols.NotProjectError
	if !errors.As(err, &np) || np.Why != "the home directory" || np.Total != 1 || np.Repos[0] != "repo0" {
		t.Fatalf("home should be refused naming repo0: %v %+v", err, np)
	}
	if took > time.Second {
		t.Errorf("the refusal took %s; it should not walk", took)
	}
	if !strings.Contains(srv.indexFailure(err), "repo0") {
		t.Errorf("tool refusal should list the repos:\n%s", srv.indexFailure(err))
	}
	srv.SetIndexAnyway(true)
	if err := srv.BuildIndex(); err != nil {
		t.Fatalf("--index-anyway: %v", err)
	}
	// A project under home is not home.
	if err := New(newTestRegistry(t), filepath.Join(home, "repo0"), nil, nil).BuildIndex(); err != nil {
		t.Errorf("a repo under home is a project: %v", err)
	}
}

// An include names a nested repository: it is indexed as part of this one
// and no longer reported as left out.
func TestIncludeOpensNestedRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := writeTree(t, map[string]string{
		"app.go":           "package p\n\nfunc App() {}\n",
		"clone/c.go":       "package c\n\nfunc Cloned() {}\n",
		"clone/sub/s.go":   "package sub\n\nfunc Sub() {}\n",
		"clone/.gitignore": "gen/\n",
		"clone/gen/g.go":   "package gen\n\nfunc Gen() {}\n",
	})
	for _, d := range []string{dir, filepath.Join(dir, "clone")} {
		if out, err := exec.Command("git", "-C", d, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}
	srv := New(newTestRegistry(t), dir, nil, nil)
	if err := srv.BuildIndex(); err != nil {
		t.Fatal(err)
	}
	if len(srv.getIndex().Lookup("Cloned")) != 0 {
		t.Fatal("without an include the clone is left out")
	}

	srv = New(newTestRegistry(t), dir, nil, nil)
	srv.SetIncludes([]string{"clone"})
	if err := srv.BuildIndex(); err != nil {
		t.Fatal(err)
	}
	idx := srv.getIndex()
	if len(idx.Lookup("Cloned")) == 0 || len(idx.Lookup("Sub")) == 0 {
		t.Error("include clone: its files are indexed")
	}
	if len(idx.Lookup("Gen")) != 0 {
		t.Error("the clone's own .gitignore applies")
	}
	for _, l := range srv.IndexCost().LeftOut {
		if l.Dir == "clone" {
			t.Errorf("an included clone is not left out: %+v", l)
		}
	}

	srv = New(newTestRegistry(t), dir, nil, nil)
	srv.SetIncludes([]string{"clone/sub"})
	if err := srv.BuildIndex(); err != nil {
		t.Fatal(err)
	}
	idx = srv.getIndex()
	if len(idx.Lookup("Sub")) == 0 || len(idx.Lookup("Cloned")) != 0 {
		t.Error("include clone/sub: only that path is indexed")
	}
}

func newTestRegistry(t *testing.T) *config.Registry {
	t.Helper()
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}
