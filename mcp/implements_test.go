package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// .implements is LSP-native — no lexical fallback. Without a child LSP it
// must match NOTHING and record that the answer is UNAVAILABLE, never read
// as "nothing implements this".
func TestImplementsWithoutLSPIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module impl\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\ntype Animal interface{ Sound() string }\n\ntype Dog struct{}\n\nfunc (d Dog) Sound() string { return \"woof\" }\n"), 0o644); err != nil {
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
	list, err := parseModernSelector(`#'main.go#Animal'::in.implements`) //nolint
	if err != nil {
		t.Fatal(err)
	}
	e, err := srv.buildTree()
	if err != nil {
		t.Fatal(err)
	}
	rows := e.evaluate(list)

	if len(rows) != 0 {
		t.Errorf("no LSP → .implements resolves nothing; got %d rows", len(rows))
	}
	if !e.implementsUnavailable {
		t.Error("the query must record that .implements was unavailable for lack of an LSP")
	}
}

// .implements parses as a KIND class on ::in / ::out.
func TestImplementsParses(t *testing.T) {
	for _, sel := range []string{
		`interface#Foo::in.implements`,
		`type#Bar::out.implements > *`,
	} {
		if _, err := parseModernSelector(sel); err != nil {
			t.Errorf("%s should parse; got %v", sel, err)
		}
	}
}
