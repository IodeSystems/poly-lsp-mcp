package bindings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// TestApplyDerived: a gat @derived(operationId) edge in the SDL binds the Go
// OperationID source as a DECLARED site — the authoritative replacement for guessing.
func TestApplyDerived(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, content string) {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("schema.graphql", "type Mutation {\n  doThing(x: Int): Int @derived(operationId: \"doThing\")\n}\n")
	mk("main.go", "package main\nvar _ = Op{OperationID: \"doThing\", Path: \"/x\"}\n")

	idx := symbols.NewIndex()
	if n := NewResolver(root).ApplyDerived(idx); n != 1 {
		t.Fatalf("ApplyDerived added %d sites, want 1", n)
	}
	var goDecl *symbols.Site
	for i, s := range idx.Lookup("doThing") {
		if s.Language == "go" && s.Confidence == symbols.ConfidenceDeclared {
			goDecl = &idx.Lookup("doThing")[i]
		}
	}
	if goDecl == nil {
		t.Fatalf("no declared go site for doThing; sites=%+v", idx.Lookup("doThing"))
	}
}
