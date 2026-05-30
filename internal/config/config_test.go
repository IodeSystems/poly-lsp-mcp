package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultBuilds(t *testing.T) {
	r, err := Default().Build()
	if err != nil {
		t.Fatalf("Default().Build(): %v", err)
	}
	cases := map[string]string{
		"go":       "go",
		".go":      "go",
		"GO":       "go",
		"ts":       "typescript",
		"tsx":      "typescript",
		"py":       "python",
		"md":       "markdown",
		"markdown": "markdown",
		"yml":      "yaml",
		"json":     "json",
	}
	for ext, want := range cases {
		got := r.LookupByExt(ext)
		if got == nil {
			t.Errorf("LookupByExt(%q) = nil, want %q", ext, want)
			continue
		}
		if got.Name != want {
			t.Errorf("LookupByExt(%q).Name = %q, want %q", ext, got.Name, want)
		}
	}
	if r.LookupByExt("rs") != nil {
		t.Error("LookupByExt(rs) should be nil — rust not in default registry")
	}
	if len(r.Languages()) != len(Default().Languages) {
		t.Errorf("Languages() returned %d, want %d", len(r.Languages()), len(Default().Languages))
	}
}

func TestLanguagesReturnsRegistrationOrder(t *testing.T) {
	r, err := Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"go", "typescript", "python", "markdown", "yaml", "json", "sql", "proto", "graphql"}
	got := r.Languages()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("Languages()[%d] = %s, want %s", i, got[i].Name, name)
		}
	}
}

func TestLoadYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "poly-lsp-mcp.yaml")
	yaml := `
languages:
  - name: rust
    extensions: [rs]
    lsp:
      cmd: rust-analyzer
    treesitter: rust
  - name: zig
    extensions: [zig]
    treesitter: zig
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r, err := cfg.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rust := r.LookupByExt("rs")
	if rust == nil || rust.Name != "rust" || rust.LSP == nil || rust.LSP.Cmd != "rust-analyzer" {
		t.Errorf("rust lookup wrong: %+v", rust)
	}
	zig := r.LookupByExt("zig")
	if zig == nil || zig.LSP != nil {
		t.Errorf("zig should be tree-sitter only, got %+v", zig)
	}
}

func TestLoadYAMLWithBindings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "poly-lsp-mcp.yaml")
	yaml := `
languages:
  - name: go
    extensions: [go]
    treesitter: go

bindings:
  - name: UserType
    sites:
      - {file: main.go, symbol: UserID}
      - {file: client.ts, symbol: UserID}
  - name: Endpoint
    sites:
      - file: config.yaml
        jsonpath: $.endpoints[0].path
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Bindings) != 2 {
		t.Fatalf("got %d bindings, want 2", len(cfg.Bindings))
	}
	if cfg.Bindings[0].Name != "UserType" {
		t.Errorf("binding[0].Name = %q, want UserType", cfg.Bindings[0].Name)
	}
	if len(cfg.Bindings[0].Sites) != 2 {
		t.Errorf("UserType sites = %d, want 2", len(cfg.Bindings[0].Sites))
	}
	if cfg.Bindings[1].Sites[0].JSONPath != "$.endpoints[0].path" {
		t.Errorf("Endpoint jsonpath = %q", cfg.Bindings[1].Sites[0].JSONPath)
	}
}

func TestLoadOrDefaultMergesDefaultLanguagesWhenOmitted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "poly-lsp-mcp.yaml")
	// Only schemas declared — no languages section. Common pattern;
	// the loader must fold defaults in so the registry isn't empty.
	if err := os.WriteFile(path,
		[]byte("schemas:\n  - {file: api.proto, dialect: proto}\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	cfg, used, err := LoadOrDefault(path)
	if err != nil {
		t.Fatal(err)
	}
	if !used {
		t.Error("used = false on existing file")
	}
	if len(cfg.Languages) == 0 {
		t.Error("Languages empty after LoadOrDefault — defaults not merged")
	}
	if len(cfg.Schemas) != 1 || cfg.Schemas[0].Dialect != "proto" {
		t.Errorf("schemas dropped or wrong: %+v", cfg.Schemas)
	}
}

func TestLoadOrDefaultMissingFile(t *testing.T) {
	cfg, used, err := LoadOrDefault(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if used {
		t.Error("used = true for missing file")
	}
	if len(cfg.Languages) == 0 {
		t.Error("default config is empty")
	}
}

func TestBuildRejectsDuplicateExtension(t *testing.T) {
	c := &Config{Languages: []Language{
		{Name: "a", Extensions: []string{"x"}, TreeSitter: "a"},
		{Name: "b", Extensions: []string{"x"}, TreeSitter: "b"},
	}}
	_, err := c.Build()
	if err == nil || !strings.Contains(err.Error(), "claimed by both") {
		t.Errorf("want duplicate-extension error, got: %v", err)
	}
}

func TestBuildRejectsMissingBackend(t *testing.T) {
	c := &Config{Languages: []Language{
		{Name: "a", Extensions: []string{"x"}},
	}}
	_, err := c.Build()
	if err == nil || !strings.Contains(err.Error(), "lsp / treesitter") {
		t.Errorf("want missing-backend error, got: %v", err)
	}
}

func TestBuildRejectsUnknownYAMLFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("languages:\n  - name: x\n    extensions: [x]\n    treesitter: x\n    bogus: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load accepted unknown field 'bogus'")
	}
}
