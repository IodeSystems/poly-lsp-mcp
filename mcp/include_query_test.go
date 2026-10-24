package mcp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// leftOutWith is leftOutOf with an include list.
func leftOutWith(t *testing.T, s *mcpSession, sel string, include ...string) (map[string]any, string) {
	t.Helper()
	r := s.callTool("node_query", map[string]any{"selector": sel, "limit": 50, "include": include})
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

func prunedOf(lo map[string]any) map[string]any {
	pr, _ := lo["pruned"].(map[string]any)
	return pr
}

func includeFixture() map[string]string {
	return map[string]string{
		"app.ts":              "export function app() {}\n",
		"src/api.ts":          "export function apiFn(): number { return 1 }\n",
		"src/api.js":          "export function apiFn() { return 1 }\n",
		"node_modules/x/a.ts": "export function libFn() {}\n",
		"vendored/y/b.ts":     "export function vendFn() {}\n",
	}
}

func startIncludeSession(t *testing.T, files map[string]string, log string) *mcpSession {
	t.Helper()
	dir := writeTree(t, files)
	s := startSessionWith(t, dir, nil, nil, func(srv *Server) {
		srv.SetExcludes([]string{"vendored/"})
		if log != "" {
			srv.SetUsageLog(log)
		}
	})
	s.request("initialize", map[string]any{})
	return s
}

// A broad query walks a skipped dir fully once include names it, at any
// depth, and stops counting it as pruned.
func TestIncludeOpensSkippedDir(t *testing.T) {
	s := startIncludeSession(t, includeFixture(), "")
	defer s.close()

	// Without include: unchanged. The hint offers the pruned dir keys.
	lo, text := leftOutWith(t, s, "func")
	if strings.Contains(text, "libFn") || prunedOf(lo)["node_modules/"] != float64(1) {
		t.Errorf("without include node_modules stays out and counted:\n%s", text)
	}
	if hint, _ := lo["hint"].(string); !strings.Contains(hint, `include:["node_modules/"]`) {
		t.Errorf("hint should name the pruned dirs: %q", hint)
	}

	for _, pat := range []string{"node_modules", "node_modules/", "/node_modules", "**/node_modules", "node_modules/x"} {
		lo, text = leftOutWith(t, s, "func", pat)
		if !strings.Contains(text, "libFn") {
			t.Errorf("include %q: broad func query should reach node_modules/x/a.ts:\n%s", pat, text)
		}
		if prunedOf(lo)["node_modules/"] != nil {
			t.Errorf("include %q: node_modules/ no longer pruned: %v", pat, lo)
		}
		if prunedOf(lo)["excluded"] != float64(1) {
			t.Errorf("include %q: other dead ends stay counted: %v", pat, lo)
		}
	}

	// A pattern that matches something else opens nothing here.
	_, text = leftOutWith(t, s, "func", "other_modules")
	if strings.Contains(text, "libFn") {
		t.Errorf("unrelated include opened node_modules:\n%s", text)
	}
}

// An anchored pattern steps through a dead end and takes only what leads to
// the match, not the dir's other children.
func TestIncludeAnchoredPathTakesOnlyItsSubtree(t *testing.T) {
	files := includeFixture()
	files["node_modules/z/c.ts"] = "export function zedFn() {}\n"
	s := startIncludeSession(t, files, "")
	defer s.close()
	lo, text := leftOutWith(t, s, "func", "node_modules/x")
	if !strings.Contains(text, "libFn") || strings.Contains(text, "zedFn") {
		t.Errorf("want node_modules/x only:\n%s", text)
	}
	if prunedOf(lo)["node_modules/"] != nil {
		t.Errorf("node_modules/ was entered, not pruned: %v", lo)
	}
}

func TestIncludeOpensExcludedDir(t *testing.T) {
	s := startIncludeSession(t, includeFixture(), "")
	defer s.close()
	lo, text := leftOutWith(t, s, "func")
	if strings.Contains(text, "vendFn") || prunedOf(lo)["excluded"] != float64(1) {
		t.Fatalf("setup: vendored/ should be excluded:\n%s", text)
	}
	for _, pat := range []string{"vendored/", "vendored/y"} {
		lo, text = leftOutWith(t, s, "func", pat)
		if !strings.Contains(text, "vendFn") || prunedOf(lo)["excluded"] != nil {
			t.Errorf("include %q should enter the excluded dir:\n%s", pat, text)
		}
	}
}

// A shadowed file (api.js beside api.ts) is un-shadowed by a pattern naming
// it, by name or by glob.
func TestIncludeUnshadowsFile(t *testing.T) {
	s := startIncludeSession(t, includeFixture(), "")
	defer s.close()
	lo, text := leftOutWith(t, s, "func")
	if strings.Contains(text, "src/api.js") || prunedOf(lo)["shadowed"] != float64(1) {
		t.Fatalf("setup: api.js should be shadowed:\n%s", text)
	}
	for _, pat := range []string{"src/api.js", "*.js", "api.js"} {
		lo, text = leftOutWith(t, s, "func", pat)
		if !strings.Contains(text, "src/api.js") || prunedOf(lo)["shadowed"] != nil {
			t.Errorf("include %q should un-shadow api.js:\n%s", pat, text)
		}
	}
}

// A nested worktree is walked in full, and .git inside it never is.
func TestIncludeWalksNestedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := writeTree(t, map[string]string{"app.go": "package p\n\nfunc App() {}\n"})
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-qm", "init")
	wt := filepath.Join(dir, ".claude", "worktrees", "a1")
	gitIn(t, dir, "worktree", "add", "-q", wt)
	if err := os.WriteFile(filepath.Join(wt, "extra.go"), []byte("package p\n\nfunc Extra() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := startSession(t, dir)
	defer s.close()
	s.request("initialize", map[string]any{})

	lo, text := leftOutWith(t, s, "func")
	if strings.Contains(text, "Extra") || prunedOf(lo)["worktree of this repo"] != float64(1) {
		t.Fatalf("setup: worktree should be pruned:\n%s", text)
	}
	for _, pat := range []string{".claude/worktrees/a1", "a1/"} {
		lo, text = leftOutWith(t, s, "func", pat)
		if !strings.Contains(text, "Extra") || !strings.Contains(text, ".claude/worktrees/a1/app.go") {
			t.Errorf("include %q should walk the whole worktree:\n%s", pat, text)
		}
		if prunedOf(lo)["worktree of this repo"] != nil {
			t.Errorf("include %q: worktree no longer pruned: %v", pat, lo)
		}
	}
	// .git is never included, even when the pattern names it.
	_, text = leftOutWith(t, s, "file[path^=.claude/worktrees/a1/.git]", ".claude/worktrees/a1", ".git")
	if !strings.Contains(text, `"returned":0`) {
		t.Errorf(".git must never be entered:\n%s", text)
	}
}

// The usage log records the patterns, and omits the field without them.
func TestUsageLogRecordsInclude(t *testing.T) {
	log := filepath.Join(t.TempDir(), "usage.jsonl")
	s := startIncludeSession(t, includeFixture(), log)
	defer s.close()
	s.callTool("node_query", map[string]any{"selector": "func"})
	s.callTool("node_query", map[string]any{"selector": "func", "include": []string{"node_modules", "*.js"}})

	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 usage lines, got %d:\n%s", len(lines), b)
	}
	if strings.Contains(lines[0], `"include"`) {
		t.Errorf("no include, no field: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"include":["node_modules","*.js"]`) {
		t.Errorf("include not recorded: %s", lines[1])
	}
}
