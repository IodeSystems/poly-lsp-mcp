package mcp

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// The type-scoped fix for the lexical-rename collision: when a language
// server serves the file, renaming one type's method must touch ONLY that
// method, never an unrelated same-named method in another package. This is
// the ab_bench islive-rename bug (renaming payments.Gateway.IsLive also hit
// llm.Rewriter.IsLive), reduced to a hermetic fixture.
func TestRefactorRenameTypeScopedViaGopls(t *testing.T) {
	if testing.Short() {
		t.Skip("gopls rename e2e skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}
	dir := t.TempDir()
	write := func(rel, body string) {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\n\ngo 1.21\n")
	// Two unrelated packages, each with a method named Ping. No shared type.
	write("gw/gw.go", "package gw\n\ntype A struct{}\n\nfunc (A) Ping() bool { return true }\n\nfunc UseA() bool { return A{}.Ping() }\n")
	write("llm/llm.go", "package llm\n\ntype B struct{}\n\nfunc (B) Ping() bool { return false }\n")

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetDiagnosticWait(8 * time.Second)

	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	sess := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
	defer sess.close()
	sess.request("initialize", map[string]any{})
	sess.notify("notifications/initialized", map[string]any{})

	r := sess.callTool("node_edit", map[string]any{"node": "gw/gw.go#A.Ping", "rename": "Pong"})
	if r.IsError {
		t.Fatalf("type-scoped rename should succeed; got %s", r.Content[0].Text)
	}
	var m map[string]any
	json.Unmarshal([]byte(r.Content[0].Text), &m)
	if m["resolvedBy"] != "lsp" {
		t.Errorf("expected resolvedBy=lsp (gopls path); got %v", m["resolvedBy"])
	}
	// A.Ping and its caller renamed; B.Ping in the other package UNTOUCHED.
	gw := string(mustRead(t, filepath.Join(dir, "gw/gw.go")))
	if !strings.Contains(gw, "func (A) Pong()") || !strings.Contains(gw, "A{}.Pong()") || strings.Contains(gw, "Ping") {
		t.Errorf("gw.go not correctly renamed:\n%s", gw)
	}
	llm := string(mustRead(t, filepath.Join(dir, "llm/llm.go")))
	if !strings.Contains(llm, "func (B) Ping()") || strings.Contains(llm, "Pong") {
		t.Errorf("llm.go was wrongly touched (lexical over-reach):\n%s", llm)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A workspace-wide rename that INTRODUCES an error (renaming Foo → Bar where
// Bar already exists = redeclaration) must NEVER leave the workspace partially
// written. With a language server the rename is type-scoped and gopls
// front-stops the redeclaration before any file is touched (rename-error);
// without one, the validationTxn applies-then-reverts. Either way the invariant
// is the same: the colliding rename is refused and every file is byte-for-byte
// its original.
func TestRefactorRenameValidateRevertsAllFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("validate rename e2e skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Foo and Bar both exist; renaming Foo→Bar redeclares Bar (a compile error).
	aPath := filepath.Join(dir, "a.go")
	bPath := filepath.Join(dir, "b.go")
	origA := "package x\n\nfunc Foo() int { return 1 }\nfunc Bar() int { return 2 }\n"
	origB := "package x\n\nfunc UseFoo() int { return Foo() }\n"
	if err := os.WriteFile(aPath, []byte(origA), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, []byte(origB), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil) // modern 3-tool surface (node_edit has rename)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetValidateEdits(true)
	srv.SetDiagnosticWait(8 * time.Second)

	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	sess := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
	defer sess.close()

	sess.request("initialize", map[string]any{})
	sess.notify("notifications/initialized", map[string]any{})

	r := sess.callTool("node_edit", map[string]any{
		"node":   "a.go#Foo",
		"rename": "Bar",
	})
	if !r.IsError {
		t.Fatalf("colliding rename should surface as isError=true; response=%+v", r.Content)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Content[0].Text), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Accept either safety mechanism: gopls declining upfront (rename-error)
	// or the validationTxn applying-then-reverting (rejected+reverted). Both
	// leave the workspace pristine, which the byte checks below prove.
	switch {
	case m["kind"] == "rename-error":
	case m["rejected"] == true && m["reverted"] == true:
	default:
		t.Fatalf("expected a rename-error OR rejected+reverted; got %+v", m)
	}
	// Every touched file must be back to its original bytes.
	if got, _ := os.ReadFile(aPath); string(got) != origA {
		t.Fatalf("a.go not restored:\n got: %q\nwant: %q", got, origA)
	}
	if got, _ := os.ReadFile(bPath); string(got) != origB {
		t.Fatalf("b.go not restored:\n got: %q\nwant: %q", got, origB)
	}
}

// An LSP position's `character` counts UTF-16 code units; every column
// this tool works in is a byte. On a line carrying multibyte text BEFORE
// the identifier, reading the server's column as bytes lands early and
// the edit slices through neighbouring source.
//
// Measured before the fix, this exact fixture produced:
//
//	café := "naïve — résultat";SommeTotal(1); _ = café
//
// — five bytes early (é, ï, —, é), eating ` _ = `. It corrupted the file
// it was asked to rename.
func TestRefactorRenameAcrossMultibyteLineViaGopls(t *testing.T) {
	if testing.Short() {
		t.Skip("gopls rename e2e skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}
	dir := t.TempDir()
	write := func(rel, body string) {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\n\ngo 1.21\n")
	write("main.go", "package main\n\n"+
		"func Total(a int) int { return a }\n\n"+
		"func main() {\n"+
		"\tcafé := \"naïve — résultat\"; _ = Total(1); _ = café\n"+
		"}\n")

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetDiagnosticWait(8 * time.Second)

	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	sess := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
	defer sess.close()
	sess.request("initialize", map[string]any{})
	sess.notify("notifications/initialized", map[string]any{})

	r := sess.callTool("node_edit", map[string]any{"node": "main.go#Total", "rename": "Somme"})
	if r.IsError {
		t.Fatalf("rename should succeed; got %s", r.Content[0].Text)
	}
	got := string(mustRead(t, filepath.Join(dir, "main.go")))
	want := "\tcafé := \"naïve — résultat\"; _ = Somme(1); _ = café\n"
	if !strings.Contains(got, want) {
		t.Errorf("call site mangled by a UTF-16/byte column mismatch:\n%s", got)
	}
	if !strings.Contains(got, "func Somme(a int) int") {
		t.Errorf("declaration not renamed:\n%s", got)
	}
	// The surrounding multibyte text must be byte-identical.
	for _, keep := range []string{"café", "naïve", "—", "résultat"} {
		if !strings.Contains(got, keep) {
			t.Errorf("multibyte text %q was damaged:\n%s", keep, got)
		}
	}
}
