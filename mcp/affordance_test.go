package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two hints that pointed at nothing. Both were correct advice naming a thing
// the caller could not reach, which is worse than no advice: the model spends
// calls looking for it, then falls back to whatever IS actionable — paging a
// file one window at a time, or rewriting a whole file to insert one function.

// The read hint's "don't page, search instead" clause has to name a call
// the caller can actually make. On the modern surface it used to name the
// classic `search` tool (pattern=<regex>), which is not on that surface —
// leaving "call again with startLine=N" as the only actionable advice, i.e.
// the chunk-by-chunk paging the hint exists to prevent.
func TestReadHintNamesASearchThisSurfaceHas(t *testing.T) {
	s, dir := startModern(t)
	defer s.close()

	// A file long enough to trip the auto char cap.
	var big strings.Builder
	for i := 0; i < 400; i++ {
		big.WriteString("// a reasonably long filler line so the char budget fires quickly\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "big.go"), []byte(big.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	m, isErr := readNode(t, s, map[string]any{"node": "big.go"})
	if isErr {
		t.Fatalf("read errored: %+v", m)
	}
	hint, _ := m["hint"].(string)
	if hint == "" {
		t.Fatalf("a truncated read should carry a hint; got %+v", m)
	}
	if strings.Contains(hint, "search tool") || strings.Contains(hint, "pattern=<regex>") {
		t.Errorf("the modern surface has no `search` tool; hint = %q", hint)
	}
	if !strings.Contains(hint, `node_query(selector: "path=big.go ::grep(`) {
		t.Errorf("the hint should spell a runnable node_query over THIS file; hint = %q", hint)
	}
}

// Addressing a symbol that isn't there yet is as often "add it" as "typo".
// The error answered only the typo, so a model that meant to insert had
// nowhere to go and rewrote the whole file instead.
func TestMissingSymbolErrorTeachesHowToAddOne(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	r := s.callTool("node_edit", map[string]any{
		"node": "main.go#NotThereYet", "newText": "func NotThereYet() {}",
	})
	if !r.IsError {
		t.Fatal("newText alone on a missing SYMBOL should not silently create a file")
	}
	msg := r.Content[0].Text
	// Both readings: the typo (did-you-mean) and the insert (the idiom).
	for _, want := range []string{"did you mean", "To ADD it instead", "NEIGHBOUR", "oldText"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error should cover both readings (%q missing); got %q", want, msg)
		}
	}
}
