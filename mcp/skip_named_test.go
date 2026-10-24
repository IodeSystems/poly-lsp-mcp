package mcp

import (
	"strings"
	"testing"
)

// node_modules and its kind are out of broad traversal — a query for every
// function must not walk every dependency — but a selector that names a
// path inside one is asking for that dependency, and reaches it.
func TestSkippedDirReachableWhenNamed(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"app.ts":                        "export function app() {}\n",
		"node_modules/lib/index.ts":     "export function libFn() {}\n",
		"node_modules/other/index.ts":   "export function otherFn() {}\n",
		"node_modules/lib/deep/more.ts": "export function deepFn() {}\n",
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
	if got := text("func"); strings.Contains(got, "node_modules") {
		t.Errorf("broad traversal walked node_modules:\n%s", got)
	}
	got := text("file[path=node_modules/lib/index.ts] > *")
	if !strings.Contains(got, "libFn") {
		t.Errorf("a named file inside node_modules should be reached:\n%s", got)
	}
	got = text("#'node_modules/lib' func")
	if !strings.Contains(got, "libFn") || !strings.Contains(got, "deepFn") {
		t.Errorf("a named directory is walked whole:\n%s", got)
	}
	if strings.Contains(got, "otherFn") {
		t.Errorf("only the named path, not its siblings:\n%s", got)
	}
}

func TestNamedSkipPaths(t *testing.T) {
	for sel, want := range map[string]string{
		"func": "",
		"file[path=node_modules/react/index.d.ts]": "node_modules/react/index.d.ts",
		"#'vendor/x/y.go#F'::in":                   "vendor/x/y.go",
		"#'./dist/'":                               "dist",
		"#'src/build.go'":                          "", // a file named build.go is not the build/ dir
	} {
		if got := strings.Join(namedSkipPaths(sel), ","); got != want {
			t.Errorf("namedSkipPaths(%q) = %q, want %q", sel, got, want)
		}
	}
}
