package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/internal/bindings"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// A rename of a @derived name cascades to every source and reference by
// default and says what it did; there is no resolution parameter to ask first.
func TestRenameDerivedCascadesByDefault(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"go.mod":     "module x\ngo 1.26\n",
		"a/a.go":     "package a\n\nfunc user_id() {}\n",
		"b/b.go":     "package b\n\nfunc user_id() {}\n",
		"use/use.go": "package use\n\nfunc F() { _ = \"user_id\" }\n",
	})
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	root := func(kind, rel string) bindings.DerivRoot {
		return bindings.DerivRoot{Name: "user_id", Kind: kind, Source: symbols.Site{
			File: filepath.Join(dir, rel), Line: 3, Col: 6, Language: "go", Confidence: symbols.ConfidenceDeclared,
		}}
	}
	// Fan-in: two sources share the name.
	s.srv.setDerivRoots([]bindings.DerivRoot{root("sqlc-column", "a/a.go"), root("gat-operation", "b/b.go")})

	r := s.callTool("node_edit", map[string]any{"node": "a/a.go#user_id", "rename": "account_id"})
	if r.IsError {
		t.Fatalf("derived rename errored: %s", r.Content[0].Text)
	}
	for _, rel := range []string{"a/a.go", "b/b.go"} {
		b, _ := os.ReadFile(filepath.Join(dir, rel))
		if !strings.Contains(string(b), "func account_id()") {
			t.Errorf("%s: source not renamed by the cascade:\n%s", rel, b)
		}
	}

	var m struct {
		Cascade struct {
			Sources   []map[string]any `json:"sources"`
			Contracts []string         `json:"contracts"`
			Note      string           `json:"note"`
		} `json:"cascade"`
	}
	if err := json.Unmarshal([]byte(r.Content[0].Text), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Cascade.Sources) != 2 {
		t.Errorf("cascade should list both sources; got %v", m.Cascade.Sources)
	}
	all := strings.Join(m.Cascade.Contracts, "\n")
	if !strings.Contains(all, "DB column user_id → account_id") || !strings.Contains(all, "needs a migration") {
		t.Errorf("sqlc-column cascade should carry the migration note; got %q", all)
	}
	if !strings.Contains(all, "GraphQL operation") || !strings.Contains(all, "external clients") {
		t.Errorf("gat-operation cascade should carry the client-break note; got %q", all)
	}
	if !strings.Contains(m.Cascade.Note, "commit:false") || !strings.Contains(m.Cascade.Note, "rename") {
		t.Errorf("cascade note should name the narrower route and the undo; got %q", m.Cascade.Note)
	}
}

// resolution is gone from node_edit: not in the schema, not accepted.
func TestNodeEditHasNoResolutionParam(t *testing.T) {
	tools := registerModernTools()
	if strings.Contains(string(tools["node_edit"].InputSchema), "resolution") || strings.Contains(tools["node_edit"].Description, "resolution") {
		t.Error("node_edit must not mention resolution")
	}
	for _, u := range tools["node_edit"].Undeclared {
		if u == "resolution" {
			t.Error("resolution must not be a hidden node_edit param either")
		}
	}
}
