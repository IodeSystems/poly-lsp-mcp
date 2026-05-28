// Package config holds the language registry: how to recognize a file by
// extension and which child LSP / tree-sitter grammar handles it.
//
// The on-disk format is YAML. A single config file (default tslsmcp.yaml at
// the workspace root) declares languages; built-in defaults cover go/ts/py
// when no file is present.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// LSP describes how to launch a child language server. A nil *LSP means
// the language has no LSP backend and is served by tree-sitter only.
type LSP struct {
	Cmd  string   `yaml:"cmd"`
	Args []string `yaml:"args,omitempty"`
	Env  []string `yaml:"env,omitempty"`
}

// Language is one entry in the registry.
type Language struct {
	Name       string   `yaml:"name"`
	Extensions []string `yaml:"extensions"`
	LSP        *LSP     `yaml:"lsp,omitempty"`
	TreeSitter string   `yaml:"treesitter,omitempty"`
}

// Config is the on-disk shape.
type Config struct {
	Languages []Language `yaml:"languages"`
}

// Registry is the in-memory lookup view: extension → language. Built from a
// Config via Build. Extensions are normalized to lowercase, leading dot
// stripped, so callers may pass ".go", "go", or "GO" interchangeably.
type Registry struct {
	byExt  map[string]*Language
	byName map[string]*Language
	order  []*Language
}

// Default returns the baked-in registry for go/ts/py plus tree-sitter-only
// entries for markdown/yaml/json. Used when no config file is found.
func Default() *Config {
	return &Config{
		Languages: []Language{
			{
				Name:       "go",
				Extensions: []string{"go"},
				LSP:        &LSP{Cmd: "gopls"},
				TreeSitter: "go",
			},
			{
				Name:       "typescript",
				Extensions: []string{"ts", "tsx", "js", "jsx", "mjs", "cjs"},
				LSP:        &LSP{Cmd: "typescript-language-server", Args: []string{"--stdio"}},
				TreeSitter: "typescript",
			},
			{
				Name:       "python",
				Extensions: []string{"py", "pyi"},
				LSP:        &LSP{Cmd: "pylsp"},
				TreeSitter: "python",
			},
			{
				Name:       "markdown",
				Extensions: []string{"md", "markdown"},
				TreeSitter: "markdown",
			},
			{
				Name:       "yaml",
				Extensions: []string{"yaml", "yml"},
				TreeSitter: "yaml",
			},
			{
				Name:       "json",
				Extensions: []string{"json"},
				TreeSitter: "json",
			},
		},
	}
}

// Load reads a YAML config file from path. Returns the parsed Config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// LoadOrDefault loads from path if it exists; otherwise returns Default().
// The bool return is true when the on-disk file was used.
func LoadOrDefault(path string) (*Config, bool, error) {
	c, err := Load(path)
	if err == nil {
		return c, true, nil
	}
	if os.IsNotExist(err) {
		return Default(), false, nil
	}
	return nil, false, err
}

// Build validates the config and produces a Registry. Returns an error if
// two languages claim the same extension or any required field is missing.
func (c *Config) Build() (*Registry, error) {
	r := &Registry{
		byExt:  make(map[string]*Language, len(c.Languages)*2),
		byName: make(map[string]*Language, len(c.Languages)),
	}
	for i := range c.Languages {
		lang := &c.Languages[i]
		if lang.Name == "" {
			return nil, fmt.Errorf("language %d: name is required", i)
		}
		if _, dup := r.byName[lang.Name]; dup {
			return nil, fmt.Errorf("duplicate language name: %s", lang.Name)
		}
		r.byName[lang.Name] = lang
		r.order = append(r.order, lang)
		if len(lang.Extensions) == 0 {
			return nil, fmt.Errorf("language %s: at least one extension required", lang.Name)
		}
		for _, ext := range lang.Extensions {
			ext = normalizeExt(ext)
			if other, dup := r.byExt[ext]; dup {
				return nil, fmt.Errorf("extension %q claimed by both %s and %s", ext, other.Name, lang.Name)
			}
			r.byExt[ext] = lang
		}
		if lang.LSP == nil && lang.TreeSitter == "" {
			return nil, fmt.Errorf("language %s: must declare at least one of lsp / treesitter", lang.Name)
		}
	}
	return r, nil
}

// LookupByExt returns the language for the given file extension, or nil if
// no language is registered. Accepts ".go", "go", "GO" — all equivalent.
func (r *Registry) LookupByExt(ext string) *Language {
	return r.byExt[normalizeExt(ext)]
}

// LookupByName returns the language by its declared name.
func (r *Registry) LookupByName(name string) *Language {
	return r.byName[name]
}

// Languages returns all registered languages in registration order.
func (r *Registry) Languages() []*Language {
	out := make([]*Language, len(r.order))
	copy(out, r.order)
	return out
}

func normalizeExt(s string) string {
	s = strings.TrimPrefix(s, ".")
	return strings.ToLower(s)
}
