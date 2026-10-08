package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// query --resolve against a REAL gopls, through the same bring-up runQuery
// uses (InitResolve, QueryText, Shutdown).
//
// The fixture is the collision only a language server settles: two
// packages each declare Save, and each Save has exactly one caller. Name-
// keyed, store.Save's callers are BOTH Run and Flush; resolved, only Run,
// and the edge says lsp.
func TestQueryResolveListsOnlyTheAskedFunctionsCallersLabelledLSP(t *testing.T) {
	requireGopls(t)
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module collide\ngo 1.21\n")
	write("store/store.go", "package store\n\nfunc Save(v string) string {\n\treturn \"store:\" + v\n}\n")
	write("cache/cache.go", "package cache\n\nfunc Save(v string) string {\n\treturn \"cache:\" + v\n}\n")
	write("main.go", "package main\n\nimport \"collide/store\"\n\nfunc Run() string {\n\treturn store.Save(\"x\")\n}\n")
	write("flush.go", "package main\n\nimport \"collide/cache\"\n\nfunc Flush() string {\n\treturn cache.Save(\"y\")\n}\n")

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	run := func(srv *Server, sel string) string {
		t.Helper()
		var b strings.Builder
		if err := srv.QueryText(sel, 0, 0, "", &b); err != nil {
			t.Fatalf("%s: %v", sel, err)
		}
		return b.String()
	}
	const callers = `#'store/store.go#Save'::in.call > *`
	const edges = `#'store/store.go#Save'::in.call`

	// Control: without --resolve the fixture IS a collision, so the name-
	// keyed answer lists the other Save's caller too. Without this the
	// resolved assertions below could pass on a fixture that never collided.
	lex := New(reg, dir, nil, nil)
	lex.SetQuiet(true)
	if out := run(lex, callers); !strings.Contains(out, "func Flush") {
		t.Fatalf("fixture does not collide: name-keyed callers of store.Save should include Flush\n%s", out)
	}

	srv := New(reg, dir, nil, nil)
	srv.SetQuiet(true)
	if err := srv.InitResolve(multiplex.NewManager(reg)); err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	out := run(srv, callers)
	if !strings.Contains(out, "func Run") {
		t.Errorf("Run calls store.Save — it must be listed\n%s", out)
	}
	if strings.Contains(out, "Flush") {
		t.Errorf("Flush calls cache.Save, not store.Save — listed only by name\n%s", out)
	}

	out = run(srv, edges)
	if !strings.Contains(out, "main.go#Run  [conf: lsp]") {
		t.Errorf("the edge from Run must be labelled conf lsp\n%s", out)
	}
	if strings.Contains(out, "Flush") {
		t.Errorf("the edge lists cache.Save's caller\n%s", out)
	}
	// A child LSP served, so the no-LSP caveat would be false here.
	if strings.Contains(out, "name-keyed (lexical) here") {
		t.Errorf("lexical-edge note printed although a child LSP served\n%s", out)
	}
}
