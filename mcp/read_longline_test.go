package mcp

import (
	"strings"
	"testing"
)

// A whole-file browse must BOUND its ops: a pathologically long line
// (minified JS, a JSON blob, a generated bundle) must not dump megabytes
// in one default node_read. The auto char budget used to be defeated
// because the FIRST line is appended before the budget check, and with
// no lineLength there was no per-line cap — so a 5 MB line-1 returned all
// 5 MB. The file stays VISIBLE (truncated view + true maxLineLength), but
// the payload is bounded. Dogfood 2026-07-21.
func TestReadPayloadBoundsPathologicalLongLine(t *testing.T) {
	huge := strings.Repeat("A", 5_000_000)
	content := []byte("const x = \"" + huge + "\";\nconst y = 2;\n")

	// Default browse: no lineLimit, no lineLength (the modern node_read path).
	out := buildReadPayload(content, "big.js", 1, 0, 0, targetedSearchAdvice("big.js", true))

	text, _ := out["text"].(string)
	if len(text) > 64*1024 {
		t.Fatalf("default node_read returned %d chars; must be bounded (~a few KB), not dump the whole long line", len(text))
	}
	if out["truncated"] != true {
		t.Errorf("a bounded long-line read must report truncated:true")
	}
	// The TRUE size must still be reported so the agent can widen if it means to.
	if ml, _ := out["maxLineLength"].(int); ml < 5_000_000 {
		t.Errorf("maxLineLength must report the real longest line (%d), got %v", 5_000_000, out["maxLineLength"])
	}
	if h, _ := out["hint"].(string); h == "" {
		t.Errorf("a truncated long-line read must include a hint on how to widen")
	}
}

// A legit long line (a wrapped markdown paragraph, a long string literal)
// sits FAR below the generated-line threshold and must be returned WHOLE
// — feeding an LLM a mid-content clip of real prose is worse than the
// extra few hundred chars. Guards against the "clip everything at 500"
// over-correction: the repo's longest real line is 742 chars.
func TestReadPayloadLegitLongLineNotClipped(t *testing.T) {
	para := strings.Repeat("word ", 160) // 800 chars: a real wrapped paragraph
	content := []byte("# Title\n\n" + para + "\n")
	out := buildReadPayload(content, "README.md", 1, 0, 0, targetedSearchAdvice("README.md", true))

	text, _ := out["text"].(string)
	if !strings.Contains(text, para) {
		t.Fatalf("an 800-char legit line must be returned whole, not clipped; text=%q", text)
	}
	if strings.Contains(text, "…") {
		t.Errorf("no ellipsis should appear for a sub-threshold line")
	}
}

// The bound must not disturb ordinary source: a normal file reads exactly
// as before, no spurious truncation.
func TestReadPayloadNormalFileUnaffected(t *testing.T) {
	content := []byte("package p\n\nfunc F() int { return 42 }\n")
	out := buildReadPayload(content, "p.go", 1, 0, 0, targetedSearchAdvice("p.go", true))
	if out["truncated"] == true {
		t.Errorf("a small normal file must not be truncated: %v", out)
	}
	if got, _ := out["text"].(string); got != string(content) {
		t.Errorf("normal whole-file read must be byte-exact\n got: %q\nwant: %q", got, content)
	}
}
