package mcp

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// bigRepo is a repository of 5 files at the top and 45 under big/, plus an
// untracked nested repository of 30 more.
func bigRepo(t *testing.T) string {
	t.Helper()
	files := map[string]string{}
	for i := range 5 {
		files[fmt.Sprintf("top%d.go", i)] = "package top\n"
	}
	for i := range 45 {
		files[fmt.Sprintf("big/f%d.go", i)] = "package big\n"
	}
	for i := range 30 {
		files[fmt.Sprintf("other/f%d.go", i)] = "package other\n"
	}
	dir := writeTree(t, files)
	for _, d := range []string{dir, dir + "/other"} {
		if out, err := exec.Command("git", "-C", d, "init", "-q").CombinedOutput(); err != nil {
			t.Skipf("git unavailable: %v %s", err, out)
		}
	}
	return dir
}

// Over --max-files the root is refused before any read, with the remedy; an
// exclude that narrows it passes; --index-anyway does not lift the cap; 0
// does. A nested repository is left out of the count, as of the build.
func TestMaxFilesRefused(t *testing.T) {
	dir := bigRepo(t)
	build := func(cap int, anyway bool, excl []string) (*Server, error) {
		srv := New(newTestRegistry(t), dir, nil, nil)
		srv.SetMaxFiles(cap)
		srv.SetIndexAnyway(anyway)
		srv.SetExcludes(excl)
		return srv, srv.BuildIndex()
	}

	_, err := build(10, false, nil)
	var tm *symbols.TooManyFilesError
	if !errors.As(err, &tm) {
		t.Fatalf("want TooManyFilesError, got %v", err)
	}
	for _, want := range []string{"has 11+ files (cap 10)", "across 1 git repos"} {
		if !strings.Contains(tm.Error(), want) {
			t.Errorf("error should contain %q: %s", want, tm.Error())
		}
	}
	for _, want := range []string{"open one repo", "--include", "--max-files"} {
		if !strings.Contains(tm.Detail(), want) {
			t.Errorf("detail should contain %q: %s", want, tm.Detail())
		}
	}
	if _, err := build(10, true, nil); !errors.As(err, &tm) {
		t.Errorf("--index-anyway must not lift the cap: %v", err)
	}
	if srv, err := build(10, false, []string{"big"}); err != nil || srv.IndexCost().Files != 5 {
		t.Errorf("narrowed to 5 files should pass: %v", err)
	}
	if srv, err := build(0, false, nil); err != nil || srv.IndexCost().Files != 50 {
		t.Errorf("cap 0 = no cap: %v", err)
	}
}

// The refusal reaches the model as a tool error with the remedy.
func TestMaxFilesToolError(t *testing.T) {
	dir := bigRepo(t)
	s := startSessionWith(t, dir, nil, nil, func(srv *Server) { srv.SetAsyncInit(true); srv.SetMaxFiles(10) })
	defer s.close()
	s.request("initialize", map[string]any{})
	r := s.callTool("node_query", map[string]any{"selector": "file"})
	if !r.IsError {
		t.Fatalf("over the cap should be a tool error: %+v", r)
	}
	text := r.Content[0].Text
	for _, want := range []string{"has 11+ files (cap 10)", "--max-files"} {
		if !strings.Contains(text, want) {
			t.Errorf("tool error should contain %q:\n%s", want, text)
		}
	}
	if note := indexNote(t, s); note["state"] != "failed" || note["reason"] != "too many files" {
		t.Errorf("failure notification: %v", note)
	}
}
