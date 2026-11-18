package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The parse cache must not live inside the workspace it indexes. It used to,
// which left an untracked .poly-lsp-mcp/ directory in every repo poly-lsp was
// pointed at — dun measured 30 of 34 session worktrees "dirty" for that reason
// alone and had to special-case the path to tell real work from our leftovers.
func TestCachePathIsOutsideTheWorkspace(t *testing.T) {
	root := t.TempDir()
	got := cachePathFor(root)

	if strings.HasPrefix(got, root+string(filepath.Separator)) {
		t.Errorf("cache lives inside the workspace: %s is under %s", got, root)
	}
	if filepath.Base(got) != "cache.gob" {
		t.Errorf("cache file name = %q, want cache.gob", filepath.Base(got))
	}
	// Stable across calls: a cache keyed by path is useless if the key moves.
	if again := cachePathFor(root); again != got {
		t.Errorf("not stable: %q then %q", got, again)
	}
	// A relative path naming the same directory must land in the same place.
	if abs := cachePathFor(filepath.Join(root, ".")); abs != got {
		t.Errorf("path-normalisation differs: %q vs %q", abs, got)
	}
}

// Two workspaces must not share one cache, or each would invalidate the other.
func TestCachePathIsPerWorkspace(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if cachePathFor(a) == cachePathFor(b) {
		t.Fatalf("distinct roots collide on %s", cachePathFor(a))
	}
	// Same basename, different parents — the hash is what separates them.
	p1 := filepath.Join(a, "proj")
	p2 := filepath.Join(b, "proj")
	if cachePathFor(p1) == cachePathFor(p2) {
		t.Errorf("same-named roots collide: %s", cachePathFor(p1))
	}
}

// The status table's ACTIVITY and EV/MIN columns, including a status
// file written before activity existed.
func TestActivityCols(t *testing.T) {
	until := time.Date(2026, 11, 17, 9, 30, 0, 0, time.Local)
	cases := []struct {
		raw, state, ev string
	}{
		{``, "-", "-"},
		{`{"state":"idle","eventsLastMin":0,"ops":{}}`, "idle", "0"},
		{`{"state":"backed-off","backedOffUntil":"` + until.Format(time.RFC3339) + `","eventsLastMin":4120,"ops":{}}`,
			"backed-off until 09:30", "4120"},
	}
	for _, c := range cases {
		state, ev := activityCols(json.RawMessage(c.raw))
		if state != c.state || ev != c.ev {
			t.Errorf("activityCols(%s) = %q, %q; want %q, %q", c.raw, state, ev, c.state, c.ev)
		}
	}
}
