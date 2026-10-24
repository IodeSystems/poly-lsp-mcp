package mcp

import "testing"

func TestIncludeSetMatches(t *testing.T) {
	tests := []struct {
		pat   string
		rel   string
		isDir bool
		want  bool
	}{
		{"node_modules", "node_modules", true, true},
		{"node_modules", "a/b/node_modules", true, true},
		{"node_modules", "node_modules_x", true, false},
		{"node_modules/", "node_modules", true, true},
		{"node_modules/", "node_modules", false, false}, // trailing / = dirs only
		{"*.js", "src/api.js", false, true},
		{"*.js", "src/api.ts", false, false},
		{"*.js", "src/sub/api.js", false, true},
		{"api.?s", "src/api.js", false, true},
		{".claude/worktrees/a1", ".claude/worktrees/a1", true, true},
		{"/.claude/worktrees/a1", ".claude/worktrees/a1", true, true},
		{".claude/worktrees/a1", "x/.claude/worktrees/a1", true, false}, // anchored
		{"src/*.js", "src/api.js", false, true},
		{"src/*.js", "src/sub/api.js", false, false}, // * stays in a segment
		{"src/**/api.js", "src/api.js", false, true},
		{"src/**/api.js", "src/a/b/api.js", false, true},
		{"**/dist", "a/b/dist", true, true},
		{"vendored/y", "vendored", true, false},
		{"[", "x", false, false}, // malformed matches nothing
	}
	for _, tt := range tests {
		got := newIncludeSet([]string{tt.pat}).matches(tt.rel, tt.isDir)
		if got != tt.want {
			t.Errorf("%q matches(%q, dir=%v) = %v, want %v", tt.pat, tt.rel, tt.isDir, got, tt.want)
		}
	}
}

func TestIncludeSetOnWayAndCovers(t *testing.T) {
	tests := []struct {
		pat  string
		rel  string
		way  bool
		cove bool
	}{
		{".claude/worktrees/a1", ".claude", true, false},
		{".claude/worktrees/a1", ".claude/worktrees", true, false},
		{".claude/worktrees/a1", ".claude/worktrees/a1", true, true},
		{".claude/worktrees/a1", ".claude/worktrees/a1/src/x.go", false, true},
		{".claude/worktrees/a1", "docs", false, false},
		{"node_modules", "a", false, false}, // unanchored: never a route
		{"node_modules", "node_modules/x/a.ts", false, true},
		{"**/dist", "a", false, false}, // leading **: no route
		{"node_modules/", "node_modules/x/a.ts", false, true},
	}
	for _, tt := range tests {
		s := newIncludeSet([]string{tt.pat})
		if got := s.onWay(tt.rel); got != tt.way {
			t.Errorf("%q onWay(%q) = %v, want %v", tt.pat, tt.rel, got, tt.way)
		}
		if got := s.covers(tt.rel); got != tt.cove {
			t.Errorf("%q covers(%q) = %v, want %v", tt.pat, tt.rel, got, tt.cove)
		}
	}
	var none *includeSet
	if none.matches("a", true) || none.onWay("a") || none.covers("a") || newIncludeSet([]string{" ", "/"}) != nil {
		t.Error("an empty set matches nothing")
	}
}
