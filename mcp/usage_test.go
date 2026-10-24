package mcp

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// Each node_query leaves one line saying how many matches were gitignored,
// and how many of those the selector named — the numbers that decide
// whether the query tree should honour .gitignore.
func TestUsageLogCountsIgnoredMatches(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := writeTree(t, map[string]string{
		".gitignore":   "gen/\n",
		"app.go":       "package p\n\nfunc App() {}\n",
		"gen/types.go": "package gen\n\nfunc Generated() {}\n",
	})
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	log := filepath.Join(t.TempDir(), "usage.jsonl")
	s := startSessionWith(t, dir, nil, nil, func(srv *Server) { srv.SetUsageLog(log) })
	defer s.close()
	s.request("initialize", map[string]any{})

	s.callTool("node_query", map[string]any{"selector": "func"})
	s.callTool("node_query", map[string]any{"selector": "#'gen/types.go' func"})
	s.callTool("node_query", map[string]any{"selector": "#App"})

	sum, err := SummarizeUsage(log)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Queries != 3 {
		t.Errorf("queries = %d, want 3", sum.Queries)
	}
	if sum.WithIgnored != 2 || sum.IgnoredByName != 1 {
		t.Errorf("want 2 queries returning gen/ (1 by name), got %+v", sum)
	}
}

// node_edit calls leave a record naming the op, the hidden params used and
// success or failure; SummarizeUsage counts them apart from node_query.
func TestUsageLogRecordsNodeEdit(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"go.mod":  "module x\ngo 1.26\n",
		"main.go": "package main\n\nfunc fetchUser() {}\n\nfunc other() {}\n",
	})
	log := filepath.Join(t.TempDir(), "usage.jsonl")
	s := startSessionWith(t, dir, nil, nil, func(srv *Server) { srv.SetUsageLog(log) })
	defer s.close()
	s.request("initialize", map[string]any{})

	s.callTool("node_query", map[string]any{"selector": "func"})
	s.callTool("node_edit", map[string]any{"node": "main.go#other", "oldText": "other", "newText": "another"})
	s.callTool("node_edit", map[string]any{"node": "main.go#fetchUser", "rename": "loadUser", "includeComments": false})
	s.callTool("node_edit", map[string]any{"node": "main.go#nope", "delete": true})

	sum, err := SummarizeUsage(log)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Queries != 1 || sum.Edits != 3 {
		t.Errorf("want 1 query and 3 edits, got %+v", sum)
	}
	if sum.EditOps["edit"] != 1 || sum.EditOps["rename"] != 1 || sum.EditOps["delete"] != 1 {
		t.Errorf("editOps = %v", sum.EditOps)
	}
	if sum.EditFailed["delete"] != 1 || sum.EditFailed["edit"] != 0 || sum.EditFailed["rename"] != 0 {
		t.Errorf("editFailed = %v, want only delete", sum.EditFailed)
	}
	if sum.EditHidden["includeComments=false"] != 1 {
		t.Errorf("editHidden = %v", sum.EditHidden)
	}
}
