package bindings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iodesystems/tslsmcp/internal/config"
	"github.com/iodesystems/tslsmcp/internal/symbols"
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
		[]byte("message Foo {\n}\n"), 0o644)
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
		[]byte("message Good {\n}\n"), 0o644)
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

func TestApplySchemasOpenAPIAndJSONSchemaReserved(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "api.yaml"), []byte("openapi: 3.0\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "api.schema.json"), []byte("{}"), 0o644)
	idx := symbols.NewIndex()
	r := NewResolver(dir)
	// Should log warnings but not panic; returns 0 inserted.
	n := r.ApplySchemas(idx, []config.Schema{
		{File: "api.yaml", Dialect: "openapi"},
		{File: "api.schema.json", Dialect: "jsonschema"},
	})
	if n != 0 {
		t.Errorf("inserted = %d, want 0 for not-yet-implemented dialects", n)
	}
}
