package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func startGenPart(t *testing.T, files map[string]string) *mcpSession {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := startSessionFull(t, dir, nil, nil)
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	return s
}

// ::signature carries the callable's decl head INLINE (no body, no doc), so
// a broad `func::signature` is a one-query overview.
func TestGenPartSignatureAndBody(t *testing.T) {
	s := startGenPart(t, map[string]string{
		"go.mod": "module x\ngo 1.21\n",
		"m.go": `package x

// Add sums two ints.
func Add(a, b int) int {
	return a + b
}

const Pi = 3
`,
	})
	defer s.close()

	// ::signature — the func head, doc EXCLUDED, body EXCLUDED.
	sig := query(t, s, map[string]any{"selector": `func#Add::signature`})
	if sig.TotalMatches != 1 {
		t.Fatalf("want 1 signature, got %d", sig.TotalMatches)
	}
	m := sig.Matches[0]
	if m.Class != "::signature" || m.In != "m.go#Add" {
		t.Errorf("signature row should carry its host; got %+v", m)
	}
	if !strings.Contains(m.Text, "func Add(a, b int) int") {
		t.Errorf("signature text must be the decl head; got %q", m.Text)
	}
	if strings.Contains(m.Text, "return a + b") {
		t.Errorf("signature must NOT include the body; got %q", m.Text)
	}
	if strings.Contains(m.Text, "sums two ints") {
		t.Errorf("signature must NOT include the doc (that's ::comment); got %q", m.Text)
	}

	// ::body — the implementation.
	body := query(t, s, map[string]any{"selector": `func#Add::body`})
	if body.TotalMatches != 1 || !strings.Contains(body.Matches[0].Text, "return a + b") {
		t.Errorf("::body must carry the implementation; got %+v", body.Matches)
	}

	// A non-callable has no signature/body split.
	if q := query(t, s, map[string]any{"selector": `const#Pi::signature`}); q.TotalMatches != 0 {
		t.Errorf("a const has no ::signature; got %v", nodes(q))
	}

	// Generated nodes are invisible to `*` — a bare descendant walk never
	// yields a signature/body node.
	star := query(t, s, map[string]any{"selector": `func#Add > *`, "limit": 50})
	for _, mm := range star.Matches {
		if mm.Class == "::signature" || mm.Class == "::body" {
			t.Errorf("`*` must not match generated ::signature/::body nodes; got %+v", mm)
		}
	}
}

// ::body is EDITABLE: a rewrite replaces just the statements, leaving the
// signature line and the braces intact — via the selector AND the range
// address node_query emits.
func TestGenPartBodyEditable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "m.go")
	if err := os.WriteFile(path, []byte("package x\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// node_query emits a RANGE address for ::body.
	q := query(t, s, map[string]any{"selector": `#'m.go#Add'::body`})
	if q.TotalMatches != 1 {
		t.Fatalf("want 1 body node; got %d", q.TotalMatches)
	}
	addr := q.Matches[0].Node
	if !strings.Contains(addr, "@") || !strings.Contains(addr, "-") {
		t.Errorf("::body address should be a range (file@start-end); got %q", addr)
	}

	// Edit via the SELECTOR — rewrites just the statements.
	r := s.callTool("node_edit", map[string]any{"node": `#'m.go#Add'::body`, "oldText": "return a + b", "newText": "return a * b"})
	if r.IsError {
		t.Fatalf("::body edit errored: %s", r.Content[0].Text)
	}
	got, _ := os.ReadFile(path)
	want := "package x\n\nfunc Add(a, b int) int {\n\treturn a * b\n}\n"
	if string(got) != want {
		t.Fatalf("::body edit must touch only the statements:\n got %q\nwant %q", got, want)
	}

	// Edit via the ADDRESS form (whole-span rewrite).
	r = s.callTool("node_edit", map[string]any{"node": addr, "newText": "\treturn 0"})
	if r.IsError {
		t.Fatalf("::body address edit errored: %s", r.Content[0].Text)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "package x\n\nfunc Add(a, b int) int {\n\treturn 0\n}\n" {
		t.Fatalf("address rewrite wrong:\n got %q", got)
	}
}

// A multi-line signature is captured whole, and TS/Python resolve their
// body field the same way.
func TestGenPartMultilineAndLangs(t *testing.T) {
	s := startGenPart(t, map[string]string{
		"go.mod": "module x\ngo 1.21\n",
		"m.go": `package x

func Multi(
	x int,
	y int,
) (int, error) {
	return x * y, nil
}
`,
		"a.ts": `export function greet(name: string): string {
  return "hi " + name;
}
`,
		"b.py": `def add(a, b):
    return a + b
`,
	})
	defer s.close()

	multi := query(t, s, map[string]any{"selector": `func#Multi::signature`})
	if multi.TotalMatches != 1 {
		t.Fatalf("want 1, got %d", multi.TotalMatches)
	}
	txt := multi.Matches[0].Text
	if !strings.Contains(txt, "x int") || !strings.Contains(txt, "(int, error)") {
		t.Errorf("multi-line signature must span the whole head; got %q", txt)
	}
	if strings.Contains(txt, "return x * y") {
		t.Errorf("signature must stop at the body; got %q", txt)
	}

	if ts := query(t, s, map[string]any{"selector": `#'a.ts#greet'::signature`}); ts.TotalMatches != 1 ||
		!strings.Contains(ts.Matches[0].Text, "function greet(name: string): string") {
		t.Errorf("TS signature wrong; got %+v", ts.Matches)
	}
	if py := query(t, s, map[string]any{"selector": `#'b.py#add'::signature`}); py.TotalMatches != 1 ||
		!strings.Contains(py.Matches[0].Text, "def add(a, b)") {
		t.Errorf("Python signature wrong; got %+v", py.Matches)
	}
}
