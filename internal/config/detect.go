package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DetectSchemas walks root and returns Schema entries for files that
// look like schema documents. Conservative heuristics by design — a
// false positive promotes random file content to declared bindings,
// which is worse than a missed file the user can declare explicitly.
//
// Heuristics:
//
//	*.proto                                          → proto
//	YAML/JSON with top-level openapi: or swagger:    → openapi
//	YAML/JSON with top-level $schema: OR             → jsonschema
//	*.schema.json filename
//
// existing is the user's explicitly-declared schemas list; files
// already present there are skipped (user wins).
func DetectSchemas(root string, existing []Schema) []Schema {
	seen := map[string]bool{}
	for _, e := range existing {
		seen[e.File] = true
	}
	var out []Schema
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDetectDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		if seen[rel] {
			return nil
		}
		if dialect := classifySchemaFile(path); dialect != "" {
			out = append(out, Schema{File: rel, Dialect: dialect})
			seen[rel] = true
		}
		return nil
	})
	return out
}

// skipDetectDir mirrors the walker exclusions used elsewhere. We never
// descend into .git, vendor dirs, build output, or our own
// .tslsmcp cache directory.
func skipDetectDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "__pycache__",
		"dist", "build", ".idea", ".vscode", ".tslsmcp":
		return true
	}
	return false
}

// classifySchemaFile returns the dialect name for a single file or ""
// if the file doesn't look like a schema. Extension-based hints come
// first (cheapest); content-based peeking only happens for YAML/JSON.
func classifySchemaFile(path string) string {
	name := filepath.Base(path)
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))

	if ext == "proto" {
		return "proto"
	}
	// Filename hint: *.schema.json is a strong JSONSchema convention.
	if ext == "json" && strings.HasSuffix(strings.ToLower(name), ".schema.json") {
		return "jsonschema"
	}
	if ext == "yaml" || ext == "yml" || ext == "json" {
		return peekDialect(path)
	}
	return ""
}

// maxSchemaProbeSize caps how much we'll read off disk during
// detection. 5 MiB covers any reasonable schema; bigger files almost
// certainly aren't schemas and aren't worth slurping at startup.
const maxSchemaProbeSize = 5 * 1024 * 1024

// peekDialect reads a YAML/JSON file and looks for distinctive
// top-level keys. yaml.v3 parses both formats since JSON is a strict
// YAML subset. Parse failures, missing keys, and out-of-budget files
// all return "" — the file just isn't a schema we recognize.
func peekDialect(path string) string {
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxSchemaProbeSize {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		return ""
	}
	if _, ok := top["openapi"]; ok {
		return "openapi"
	}
	if _, ok := top["swagger"]; ok {
		return "openapi"
	}
	if _, ok := top["$schema"]; ok {
		return "jsonschema"
	}
	return ""
}
