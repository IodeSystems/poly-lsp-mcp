package mcp

import (
	"strings"
	"testing"
)

const refsSrc = `package store

// Save persists. It does NOT call itself.
func Save(id string) error { return nil }

// Walk IS recursive.
func Walk(n int) int {
	if n <= 0 {
		return 0
	}
	return Walk(n - 1)
}

func Caller() { _ = Save("x") }
`

// :parents must not report a symbol as referencing itself just for being
// declared. The index is lexical — it holds every occurrence of an identifier,
// including the one in `func Save(...)`, whose enclosing symbol is Save. So
// "who calls Save?" answered "Caller, and also Save": noise on the single query
// that most justifies this tool over grep.
//
// A declaration is exactly the site at the symbol's own NAME position, so
// excluding it separates the two cases that actually differ — which is the
// distinction LSP draws with references' includeDeclaration.
func TestParentsExcludesTheDeclarationItself(t *testing.T) {
	s := startSessionFull(t, goWorkspace(t, refsSrc), nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	got := s.callTool("node_query", map[string]any{"selector": `#'main.go#Save':parents(*)`})
	if got.IsError {
		t.Fatalf("errored: %s", got.Content[0].Text)
	}
	text := got.Content[0].Text
	if !strings.Contains(text, "main.go#Caller") {
		t.Errorf("want Caller (the real caller); got: %s", text)
	}
	if strings.Contains(text, "main.go#Save") {
		t.Errorf("Save does not call itself — its declaration is not a reference: %s", text)
	}
}

// The other half: a genuinely recursive function DOES reference itself, and
// must survive. Its `Walk(n-1)` call is a real site somewhere other than its
// name position, so the decl filter must not swallow it.
func TestParentsKeepsRealRecursion(t *testing.T) {
	s := startSessionFull(t, goWorkspace(t, refsSrc), nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	got := s.callTool("node_query", map[string]any{"selector": `#'main.go#Walk':parents(*)`})
	if got.IsError {
		t.Fatalf("errored: %s", got.Content[0].Text)
	}
	if !strings.Contains(got.Content[0].Text, "main.go#Walk") {
		t.Errorf("Walk really does call itself — that IS a reference: %s", got.Content[0].Text)
	}
}
