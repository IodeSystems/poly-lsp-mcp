package bindings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/internal/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/symbols"
)

func TestApplySchemasProtoPromotesWorkspaceHitsToDeclared(t *testing.T) {
	dir := t.TempDir()
	// Schema file declaring UserID.
	if err := os.WriteFile(filepath.Join(dir, "api.proto"),
		[]byte("syntax = \"proto3\";\n\nmessage UserID {\n  int64 value = 1;\n}\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	// Workspace already has UserID in a Go file via the lexical index.
	idx := symbols.NewIndex()
	idx.Refresh(filepath.Join(dir, "main.go"), "go", []symbols.Hit{
		{Name: "UserID", Line: 5, Col: 6},
		{Name: "UserID", Line: 8, Col: 19},
	})

	r := NewResolver(dir)
	inserted := r.ApplySchemas(idx, []config.Schema{
		{File: "api.proto", Dialect: "proto"},
	})
	if inserted < 3 {
		t.Errorf("inserted = %d, want >= 3 (schema + 2 Go sites)", inserted)
	}

	// Lookup must now return declared sites covering both the proto and
	// the Go positions.
	sites := idx.Lookup("UserID")
	languages := map[string]bool{}
	allDeclared := true
	for _, s := range sites {
		languages[s.Language] = true
		if s.Confidence != symbols.ConfidenceDeclared {
			allDeclared = false
		}
	}
	if !languages["go"] {
		t.Errorf("Go sites not surfaced under declared: %+v", sites)
	}
	if !languages["proto"] {
		t.Errorf("proto declaration site not registered: %+v", sites)
	}
	if !allDeclared {
		t.Errorf("expected every UserID site to be declared after ApplySchemas: %+v", sites)
	}
}

func TestApplySchemasIdempotentOnRepeat(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "api.proto"),
		[]byte("syntax = \"proto3\";\n\nmessage Foo {\n}\n"), 0o644)
	idx := symbols.NewIndex()

	r := NewResolver(dir)
	r.ApplySchemas(idx, []config.Schema{{File: "api.proto", Dialect: "proto"}})
	before := len(idx.Lookup("Foo"))
	r.ApplySchemas(idx, []config.Schema{{File: "api.proto", Dialect: "proto"}})
	after := len(idx.Lookup("Foo"))
	if before != after {
		t.Errorf("repeat ApplySchemas added duplicates: %d → %d", before, after)
	}
}

func TestApplySchemasUnknownDialectLoggedNotFatal(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "api.weird"), []byte("anything"), 0o644)
	idx := symbols.NewIndex()
	r := NewResolver(dir)
	// Mixed: one bad dialect + one good. Good one should still apply.
	os.WriteFile(filepath.Join(dir, "good.proto"),
		[]byte("syntax = \"proto3\";\n\nmessage Good {\n}\n"), 0o644)
	inserted := r.ApplySchemas(idx, []config.Schema{
		{File: "api.weird", Dialect: "weird"},
		{File: "good.proto", Dialect: "proto"},
	})
	if inserted < 1 {
		t.Errorf("expected at least 1 inserted (the good schema), got %d", inserted)
	}
	if len(idx.Lookup("Good")) == 0 {
		t.Error("good.proto schema not applied")
	}
}

func TestApplySchemasJSONSchemaPromotesWorkspaceHits(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "user.schema.json"), []byte(`{
  "title": "User",
  "$defs": {
    "UserID": {"type": "integer"}
  }
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	idx := symbols.NewIndex()
	idx.Refresh(filepath.Join(dir, "main.go"), "go", []symbols.Hit{
		{Name: "UserID", Line: 5, Col: 6},
		{Name: "User", Line: 12, Col: 6},
	})
	r := NewResolver(dir)
	n := r.ApplySchemas(idx, []config.Schema{
		{File: "user.schema.json", Dialect: "jsonschema"},
	})
	if n < 4 {
		t.Errorf("inserted = %d, want >= 4 (2 schema declarations + 2 Go promotions)", n)
	}
	for _, name := range []string{"UserID", "User"} {
		sites := idx.Lookup(name)
		langs := map[string]bool{}
		for _, s := range sites {
			langs[s.Language] = true
			if s.Confidence != symbols.ConfidenceDeclared {
				t.Errorf("%s site not declared after ApplySchemas: %+v", name, s)
			}
		}
		if !langs["go"] || !langs["jsonschema"] {
			t.Errorf("%s missing go or jsonschema language: %+v", name, langs)
		}
	}
}

func TestApplySchemasOpenAPIPromotesWorkspaceHits(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "api.yaml"), []byte(`openapi: 3.0.3
paths:
  /users/{id}:
    get:
      operationId: GetUser
components:
  schemas:
    UserID:
      type: integer
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Workspace has Go uses of UserID and GetUser already lexically.
	idx := symbols.NewIndex()
	idx.Refresh(filepath.Join(dir, "main.go"), "go", []symbols.Hit{
		{Name: "UserID", Line: 5, Col: 6},
		{Name: "GetUser", Line: 10, Col: 6},
	})
	r := NewResolver(dir)
	n := r.ApplySchemas(idx, []config.Schema{{File: "api.yaml", Dialect: "openapi"}})
	if n < 4 {
		t.Errorf("inserted = %d, want >= 4 (2 openapi declarations + 2 Go promotions)", n)
	}
	for _, name := range []string{"UserID", "GetUser"} {
		sites := idx.Lookup(name)
		langs := map[string]bool{}
		for _, s := range sites {
			langs[s.Language] = true
			if s.Confidence != symbols.ConfidenceDeclared {
				t.Errorf("%s site not declared after ApplySchemas: %+v", name, s)
			}
		}
		if !langs["go"] || !langs["openapi"] {
			t.Errorf("%s missing go or openapi language: %+v", name, langs)
		}
	}
}
