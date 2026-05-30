package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectSchemasFindsProtoByExtension(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "api.proto"),
		[]byte("syntax = \"proto3\";\nmessage X {}\n"), 0o644)
	got := DetectSchemas(dir, nil)
	if len(got) != 1 || got[0].File != "api.proto" || got[0].Dialect != "proto" {
		t.Errorf("got %+v, want one proto entry", got)
	}
}

func TestDetectSchemasFindsOpenAPIByTopLevelKey(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "openapi.yaml"),
		[]byte("openapi: 3.0.3\ninfo: {title: X, version: 0.0.0}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "swagger.yaml"),
		[]byte("swagger: '2.0'\ninfo: {title: Y, version: 1.0.0}\n"), 0o644)
	got := DetectSchemas(dir, nil)
	dialects := map[string]string{}
	for _, s := range got {
		dialects[s.File] = s.Dialect
	}
	if dialects["openapi.yaml"] != "openapi" {
		t.Errorf("openapi.yaml dialect = %q, want openapi", dialects["openapi.yaml"])
	}
	if dialects["swagger.yaml"] != "openapi" {
		t.Errorf("swagger.yaml dialect = %q, want openapi (swagger 2.0 is the legacy openapi)", dialects["swagger.yaml"])
	}
}

func TestDetectSchemasFindsJSONSchemaByFilenameOrSchemaKey(t *testing.T) {
	dir := t.TempDir()
	// Filename heuristic.
	os.WriteFile(filepath.Join(dir, "user.schema.json"),
		[]byte(`{"title": "User", "type": "object"}`), 0o644)
	// $schema key heuristic.
	os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object"}`), 0o644)
	got := DetectSchemas(dir, nil)
	d := map[string]string{}
	for _, s := range got {
		d[s.File] = s.Dialect
	}
	if d["user.schema.json"] != "jsonschema" {
		t.Errorf("user.schema.json dialect = %q", d["user.schema.json"])
	}
	if d["config.json"] != "jsonschema" {
		t.Errorf("config.json (with $schema key) dialect = %q", d["config.json"])
	}
}

func TestDetectSchemasIgnoresGenericYAMLAndJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "values.yaml"), []byte("name: foo\nport: 8080\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "data.json"), []byte(`{"name":"foo","port":8080}`), 0o644)
	got := DetectSchemas(dir, nil)
	if len(got) != 0 {
		t.Errorf("expected no schemas for generic data files, got %+v", got)
	}
}

func TestDetectSchemasSkipsAlreadyDeclaredFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "api.proto"),
		[]byte("syntax = \"proto3\";\n"), 0o644)
	existing := []Schema{{File: "api.proto", Dialect: "proto"}}
	got := DetectSchemas(dir, existing)
	if len(got) != 0 {
		t.Errorf("expected detector to skip explicitly-declared schemas, got %+v", got)
	}
}

func TestDetectSchemasSkipsExcludedDirs(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{".git", "node_modules", "vendor"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, sub, "api.proto"),
			[]byte("syntax = \"proto3\";\n"), 0o644)
	}
	// Plus one real one to make sure detection still runs.
	os.WriteFile(filepath.Join(dir, "real.proto"),
		[]byte("syntax = \"proto3\";\n"), 0o644)

	got := DetectSchemas(dir, nil)
	if len(got) != 1 || got[0].File != "real.proto" {
		t.Errorf("detector descended into junk dirs: %+v", got)
	}
}

func TestDetectSchemasTolerantOfMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{"unclosed`), 0o644)
	os.WriteFile(filepath.Join(dir, "ok.proto"), []byte("syntax = \"proto3\";"), 0o644)
	got := DetectSchemas(dir, nil)
	// broken.json should silently NOT be classified; ok.proto should still appear.
	files := map[string]bool{}
	for _, s := range got {
		files[s.File] = true
	}
	if files["broken.json"] {
		t.Errorf("broken.json should not be classified as a schema")
	}
	if !files["ok.proto"] {
		t.Errorf("ok.proto should still be detected even when sibling parse fails")
	}
}
