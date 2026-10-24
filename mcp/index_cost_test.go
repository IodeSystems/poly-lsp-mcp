package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// The initialize result says what the index cost, by directory, so a client
// can decide what to ignore — the handshake is where a slow build is felt.
func TestInitializeReportsIndexCost(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"main.go":      "package main\n\nfunc main() {}\n",
		"clone/.git":   "gitdir: /elsewhere\n",
		"clone/x/a.go": "package x\n\nfunc A() {}\n",
		"clone/x/b.go": "package x\n\nfunc B() {}\n",
		"clone/x/c.go": "package x\n\nfunc C() {}\n",
	} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := startSession(t, dir)
	defer s.close()
	resp := s.request("initialize", map[string]any{})
	var got struct {
		Index *symbols.CostReport `json:"index"`
	}
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatal(err)
	}
	if got.Index == nil {
		t.Fatalf("initialize result has no index report: %s", resp.Result)
	}
	if got.Index.Files != 4 {
		t.Errorf("files = %d, want 4", got.Index.Files)
	}
	if s.srv.IndexCost().Files != 4 {
		t.Errorf("IndexCost() disagrees with the initialize result")
	}
}
