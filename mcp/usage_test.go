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
