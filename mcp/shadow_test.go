package mcp

import (
	"strings"
	"testing"
)

// foo.js compiled beside foo.ts defines every name twice. The stronger file
// answers; the lesser one is out of the index and broad queries, counted in
// the cost report, and still reachable by naming its path.
func TestCompiledJSIsShadowedByTS(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"src/api.ts":   "export function apiFn(): number { return 1 }\n",
		"src/api.js":   "export function apiFn() { return 1 }\n",
		"src/plain.js": "export function plainFn() {}\n",
	})
	s := startSession(t, dir)
	defer s.close()
	s.request("initialize", map[string]any{})

	text := func(sel string) string {
		r := s.callTool("node_query", map[string]any{"selector": sel, "limit": 50})
		if len(r.Content) == 0 {
			t.Fatalf("%s: no content", sel)
		}
		return r.Content[0].Text
	}
	got := text("#apiFn")
	if !strings.Contains(got, "src/api.ts#apiFn") || strings.Contains(got, "src/api.js") {
		t.Errorf("only the .ts definition should answer:\n%s", got)
	}
	if got := text("func"); !strings.Contains(got, "plainFn") {
		t.Errorf("a .js with no stronger sibling is not shadowed:\n%s", got)
	}
	if got := text("file[path=src/api.js] > *"); !strings.Contains(got, "apiFn") {
		t.Errorf("naming the shadowed file reaches it:\n%s", got)
	}
	if n := s.srv.IndexCost().Shadowed["ts over js"]; n != 1 {
		t.Errorf("cost report shadowed = %v, want 1 under ts over js", s.srv.IndexCost().Shadowed)
	}
	if len(s.srv.getIndex().Lookup("apiFn")) != 1 {
		t.Errorf("the index should hold apiFn once: %v", s.srv.getIndex().Lookup("apiFn"))
	}
}
