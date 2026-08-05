package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The volcycle bench measured a run that used poly-lsp as a grep plus a line
// reader and never once crossed from `file@line` to `file#symbol`: 0 symbol
// addresses in 52 reads and 17 edits, 85% of reads truncated, 489KB of
// newText to change 5.7KB. Every one of those is the same missing step.
//
// The read hint ALREADY told it what to do — "Avoid paging chunk-by-chunk:
// re-read with lineLimit=N to get the whole file in ONE call" — in prose, in
// first position, 44 times, ignored 44 times. The same model corrected itself
// within one call of any note saying "you got zero results", and pasted
// `matches[].node` addresses constantly. So the fix is not more advice: it is
// an address in a field, which is the same reason `outline` was promoted out
// of the hint.
func TestReadCarriesTheEnclosingSymbolAddress(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	// Anchor on a line that is genuinely inside a declaration. Line 1 is the
	// package clause, which is deliberately NOT addressable (see below), so
	// the line is derived from the symbol rather than assumed.
	q := query(t, s, map[string]any{"selector": "method[name=Start]"})
	if len(q.Matches) != 1 {
		t.Fatalf("fixture drift: expected exactly one method Start, got %+v", q.Matches)
	}
	inside := q.Matches[0].At[0]

	// A windowed read of a FILE — the shape the whole bench run used.
	m, isErr := readNode(t, s, map[string]any{"node": "main.go", "startLine": inside, "lineLimit": 1})
	if isErr {
		t.Fatalf("read errored: %+v", m)
	}
	addr, _ := m["enclosingSymbol"].(string)
	if addr == "" {
		t.Fatalf("a windowed file read inside a declaration must hand back its address; got %+v", m)
	}
	if !strings.HasPrefix(addr, "main.go#") {
		t.Errorf("enclosingSymbol should be a node address, got %q", addr)
	}

	// It must be usable as-is — that is the entire point of it being a field.
	whole, isErr := readNode(t, s, map[string]any{"node": addr})
	if isErr {
		t.Fatalf("the address handed back does not read: %+v", whole)
	}
	if trunc, _ := whole["truncated"].(bool); trunc {
		t.Errorf("an addressed symbol must not truncate; got %+v", whole)
	}

	// Never the package clause. It is one symbol per file named for the last
	// path segment, so `main.go#x` is a legal address that answers nothing —
	// 5 of the first 7 values in the live run were exactly this.
	for _, n := range []string{"main.go", "web/some_file.ts"} {
		r, isErr := readNode(t, s, map[string]any{"node": n, "startLine": 1, "lineLimit": 1})
		if isErr {
			continue
		}
		if got, _ := r["enclosingSymbol"].(string); strings.HasSuffix(got, "#x") || strings.HasSuffix(got, "#web") {
			t.Errorf("%s: enclosingSymbol must never be the package clause; got %q", n, got)
		}
	}

	// Not repeated back when the caller already addressed that node — an echo
	// of the input is not information.
	same, _ := readNode(t, s, map[string]any{"node": addr, "startLine": 1})
	if got, _ := same["enclosingSymbol"].(string); got == addr {
		t.Errorf("enclosingSymbol should be omitted when it equals the addressed node; got %q", got)
	}
}

// A whole-file rewrite that changes a few dozen bytes is the shape that sent
// 489KB to change 5.7KB. It is legitimate (reformats, codegen), so this
// reports rather than refuses — but it names the declaration that actually
// changed, so the next edit can address it.
func TestWholeFileRewriteNamesTheNodeItActuallyChanged(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("package p\n\n")
	for i := 0; i < 400; i++ {
		b.WriteString("// filler line to push this file past the amplification floor\n")
	}
	b.WriteString("func Target() int {\n\treturn 1\n}\n")
	orig := b.String()
	if err := os.WriteFile(filepath.Join(dir, "big.go"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// Rewrite the whole file to change one return value — passing the whole
	// current text as oldText, which is the shape the surface itself teaches
	// ("To replace it entirely, pass its whole current text as oldText") and
	// the shape all 17 bench edits used.
	r := s.callTool("node_edit", map[string]any{
		"node":    "big.go",
		"oldText": orig,
		"newText": strings.Replace(orig, "return 1", "return 2", 1),
	})
	var m map[string]any
	if len(r.Content) > 0 {
		_ = json.Unmarshal([]byte(r.Content[0].Text), &m)
	}
	if r.IsError {
		t.Fatalf("edit errored: %+v", m)
	}
	got, _ := m["editedNode"].(string)
	if got != "big.go#Target" {
		t.Errorf("a whole-file rewrite should name the declaration it changed; got %q from %+v", got, m)
	}
	if hint, _ := m["editedNodeHint"].(string); !strings.Contains(hint, "newText is just the declaration") {
		t.Errorf("the hint should say what addressing it buys; got %q", hint)
	}

	// An honest whole-file change stays quiet — no ratio, nothing to say.
	cur, err := os.ReadFile(filepath.Join(dir, "big.go"))
	if err != nil {
		t.Fatal(err)
	}
	r2 := s.callTool("node_edit", map[string]any{
		"node":    "big.go",
		"oldText": string(cur),
		"newText": "package p\n\nfunc Target() int { return 3 }\n",
	})
	var m2 map[string]any
	if len(r2.Content) > 0 {
		_ = json.Unmarshal([]byte(r2.Content[0].Text), &m2)
	}
	if _, ok := m2["editedNode"]; ok {
		t.Errorf("a genuine whole-file rewrite must not be flagged; got %+v", m2)
	}
}
