package mcp

import (
	"strings"
	"testing"
)

// Over budget, with two Go modules under one gopls: the remedy and the
// setup notification offer --lsp-roots auto.
func TestBudgetRemedyOffersLSPRoots(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"a/go.mod": "module a\n", "a/x.go": "package a\n",
		"b/go.mod": "module b\n", "b/y.go": "package b\n",
	})
	s := startSessionWith(t, dir, nil, nil, func(srv *Server) {
		srv.SetAsyncInit(true)
		if err := srv.SetIndexBudget("1ops"); err != nil {
			t.Fatal(err)
		}
	})
	defer s.close()
	s.request("initialize", map[string]any{})

	r := s.callTool("node_query", map[string]any{"selector": "file"})
	want := "go: 2 project roots"
	if !r.IsError || !strings.Contains(r.Content[0].Text, want) || !strings.Contains(r.Content[0].Text, "Refine: --lsp-roots auto") {
		t.Fatalf("the budget remedy should offer --lsp-roots auto:\n%+v", r)
	}
	note := loggerNote(t, s, setupLogger)
	hints, _ := note["hints"].([]any)
	if len(hints) != 1 || !strings.Contains(hints[0].(string), want) {
		t.Fatalf("setup notification hints: %v", note)
	}
	if roots, _ := note["roots"].(map[string]any); len(roots["go"].([]any)) != 2 {
		t.Errorf("setup notification roots: %v", note["roots"])
	}
}

// In single mode with no trigger nothing is offered, and the bring-up does
// not search for roots at all.
func TestSingleModeWithoutTriggerHasNoRootsHint(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"a/go.mod": "module a\n", "a/x.go": "package a\n\nfunc A() {}\n",
		"b/go.mod": "module b\n", "b/y.go": "package b\n",
	})
	s := startSessionWith(t, dir, nil, nil, func(srv *Server) { srv.SetAsyncInit(true) })
	defer s.close()
	s.request("initialize", map[string]any{})
	s.callTool("node_query", map[string]any{"selector": "#A"})
	if note := loggerNote(t, s, setupLogger); note["hints"] != nil {
		t.Fatalf("no trigger, yet a hint: %v", note)
	}
	s.srv.rootsMu.Lock()
	searched := s.srv.roots != nil
	s.srv.rootsMu.Unlock()
	if searched {
		t.Error("single mode searched for roots without a trigger")
	}
}

// --lsp-roots auto: the setup notification lists the roots and how to
// revert.
func TestAutoModeSetupListsRootsAndRevert(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"svc/pyproject.toml": "", "svc/m.py": "def f(): pass\n",
		"lib/setup.py": "", "lib/n.py": "def g(): pass\n",
	})
	s := startSessionWith(t, dir, nil, nil, func(srv *Server) {
		srv.SetAsyncInit(true)
		srv.SetLSPRoots(true, 3)
	})
	defer s.close()
	s.request("initialize", map[string]any{})
	s.callTool("node_query", map[string]any{"selector": "#f"})

	note := loggerNote(t, s, setupLogger)
	hints, _ := note["hints"].([]any)
	if len(hints) != 1 {
		t.Fatalf("setup notification hints: %v", note)
	}
	h := hints[0].(string)
	for _, want := range []string{"python: 2 project roots (lib, svc)", "at most 3 running", "Revert: --lsp-roots single"} {
		if !strings.Contains(h, want) {
			t.Errorf("hint should contain %q: %s", want, h)
		}
	}
}
