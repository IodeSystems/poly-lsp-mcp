package pathpat

import "testing"

func TestSetMatches(t *testing.T) {
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
		got := New([]string{tt.pat}).Matches(tt.rel, tt.isDir)
		if got != tt.want {
			t.Errorf("%q matches(%q, dir=%v) = %v, want %v", tt.pat, tt.rel, tt.isDir, got, tt.want)
		}
	}
}

func TestSetOnWayAndCovers(t *testing.T) {
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
		s := New([]string{tt.pat})
		if got := s.OnWay(tt.rel); got != tt.way {
			t.Errorf("%q onWay(%q) = %v, want %v", tt.pat, tt.rel, got, tt.way)
		}
		if got := s.Covers(tt.rel); got != tt.cove {
			t.Errorf("%q covers(%q) = %v, want %v", tt.pat, tt.rel, got, tt.cove)
		}
	}
	var none *Set
	if none.Matches("a", true) || none.OnWay("a") || none.Covers("a") || New([]string{" ", "/"}) != nil {
		t.Error("an empty set matches nothing")
	}
}
