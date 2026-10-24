package mcp

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// leftOutOf runs node_query and returns the payload's leftOut object, nil
// when the key is absent, and the raw text.
func leftOutOf(t *testing.T, s *mcpSession, sel string) (map[string]any, string) {
	t.Helper()
	r := s.callTool("node_query", map[string]any{"selector": sel, "limit": 50})
	if len(r.Content) == 0 {
		t.Fatalf("%s: no content", sel)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(r.Content[0].Text), &payload); err != nil {
		t.Fatalf("%s: payload is not JSON: %v\n%s", sel, err, r.Content[0].Text)
	}
	lo, _ := payload["leftOut"].(map[string]any)
	return lo, r.Content[0].Text
}

// A broad query rolls up what it did not enter, by reason: a skipped dir, a
// shadowed file, a client-excluded dir. A query that names the only skipped
// path reaches it and has nothing to report.
func TestQueryRollsUpPrunedByReason(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"app.ts":                    "export function app() {}\n",
		"src/api.ts":                "export function apiFn(): number { return 1 }\n",
		"src/api.js":                "export function apiFn() { return 1 }\n",
		"node_modules/x/a.ts":       "export function libFn() {}\n",
		"vendored/y/b.ts":           "export function vendFn() {}\n",
		"node_modules/x/package.js": "",
	})
	s := startSessionWith(t, dir, nil, nil, func(srv *Server) { srv.SetExcludes([]string{"vendored/"}) })
	defer s.close()
	s.request("initialize", map[string]any{})

	lo, text := leftOutOf(t, s, "func")
	pruned, _ := lo["pruned"].(map[string]any)
	for reason, want := range map[string]float64{"node_modules/": 1, "shadowed": 1, "excluded": 1} {
		if pruned[reason] != want {
			t.Errorf("pruned[%q] = %v, want %v\n%s", reason, pruned[reason], want, text)
		}
	}
	if lo["hint"] == nil {
		t.Errorf("leftOut should carry a hint: %v", lo)
	}

	lo, text = leftOutOf(t, s, "file[path=node_modules/x/a.ts] > *")
	if !strings.Contains(text, "libFn") {
		t.Errorf("the named path should be reached:\n%s", text)
	}
	if pr, _ := lo["pruned"].(map[string]any); pr["node_modules/"] != nil {
		t.Errorf("a reached node_modules is not pruned: %v", lo)
	}
}

func TestQueryOmitsLeftOutWhenNothingPruned(t *testing.T) {
	dir := writeTree(t, map[string]string{"app.ts": "export function app() {}\n"})
	s := startSession(t, dir)
	defer s.close()
	s.request("initialize", map[string]any{})
	if lo, text := leftOutOf(t, s, "func"); lo != nil {
		t.Errorf("nothing was left out, leftOut = %v\n%s", lo, text)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// A worktree of the repo is pruned under its own reason.
func TestQueryRollsUpWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := writeTree(t, map[string]string{"app.go": "package p\n\nfunc App() {}\n"})
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-qm", "init")
	gitIn(t, dir, "worktree", "add", "-q", filepath.Join(dir, ".claude", "worktrees", "a1"))

	s := startSession(t, dir)
	defer s.close()
	s.request("initialize", map[string]any{})
	lo, text := leftOutOf(t, s, "func")
	pruned, _ := lo["pruned"].(map[string]any)
	if pruned["worktree of this repo"] != float64(1) {
		t.Errorf("pruned = %v, want one worktree\n%s", pruned, text)
	}
}

// leftOutReason names why a path inside the root has no node; noteDeadEnd
// counts it only then.
func TestLeftOutReason(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := writeTree(t, map[string]string{
		"app.ts":              "export function app() {}\n",
		"src/api.ts":          "export function apiFn(): number { return 1 }\n",
		"src/api.js":          "export function apiFn() { return 1 }\n",
		"node_modules/x/a.ts": "export function libFn() {}\n",
	})
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-qm", "init")
	gitIn(t, dir, "worktree", "add", "-q", filepath.Join(dir, ".claude", "worktrees", "a1"))

	s := startSession(t, dir)
	defer s.close()
	s.request("initialize", map[string]any{})
	e, err := s.srv.buildTreeFor("")
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"node_modules/x/a.ts":         "node_modules/",
		".claude/worktrees/a1/app.ts": "worktree of this repo",
		"src/api.js":                  "shadowed",
		"src/api.ts":                  "",
		"app.ts":                      "",
	} {
		if got := e.leftOutReason(rel); got != want {
			t.Errorf("leftOutReason(%q) = %q, want %q", rel, got, want)
		}
	}
	e.noteDeadEnd("node_modules/x/a.ts")
	e.noteDeadEnd("node_modules/x/a.ts")
	e.noteDeadEnd("app.ts")
	if len(e.deadEnds) != 1 || e.deadEnds["node_modules/"] != 2 {
		t.Errorf("deadEnds = %v, want only node_modules/ x2", e.deadEnds)
	}
}
