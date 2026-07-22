package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// Without a child LSP, :recursive cannot confirm a self-call (a name-unique
// self-edge might be an interface method of the same name), so it must NOT
// flag the func AND must record that the answer is under-resolved — never a
// silent "nothing is recursive".
func TestRecursiveWithoutLSPIsUnderResolved(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module rec\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc fib(n int) int { return fib(n-1) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := config.LoadOrDefault("nonexistent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil) // no SetManager — the CLI's shape
	if err := srv.BuildIndex(); err != nil {
		t.Fatal(err)
	}
	list, err := parseModernSelector(`func:recursive`) //nolint
	if err != nil {
		t.Fatal(err)
	}
	e, err := srv.buildTree()
	if err != nil {
		t.Fatal(err)
	}
	rows := e.evaluate(list)

	if len(rows) != 0 {
		t.Errorf("no LSP means no confirmed self-call — :recursive must match nothing; got %d", len(rows))
	}
	if !e.recursiveUnconfirmed {
		t.Error("a self-edge went unconfirmed for lack of an LSP; the query must record it")
	}
}
