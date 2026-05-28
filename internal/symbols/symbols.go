// Package symbols is the cross-language index that sits above multiplex.
// It answers "where does this name appear?" across every file regardless
// of which child LSP owns it — the unique value-add of tslsmcp vs any
// single-language LSP.
//
// Three tiers (see plan/plan.md):
//
//   - Tier 1 (this file): lexical. Word-token extraction with optional
//     keyword filtering per language. Cheap, noisy, useful for
//     workspace/symbol and as a soft signal for textDocument/references.
//   - Tier 2: declared bindings from tslsmcp.yaml. Precise; drives safe
//     cross-language rename.
//   - Tier 3: schema-anchored (proto/openapi/jsonschema). Deferred.
package symbols

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/iodesystems/tslsmcp/internal/config"
)

// Confidence ranks how trustworthy a Site is for a given Name.
type Confidence int

const (
	ConfidenceLexical  Confidence = iota // word-token match; high recall, low precision
	ConfidenceDeclared                   // user-declared binding in tslsmcp.yaml
	ConfidenceLSP                        // result from a child LSP
)

// Site is one occurrence of a name in a file. Line/Col are 1-based; Col
// is a byte offset within the line, not a rune offset (matches LSP's
// byte-offset convention before UTF-16 conversion at the wire).
type Site struct {
	File       string
	Line       int
	Col        int
	Language   string
	Confidence Confidence
}

// Index maps name → []Site. Safe for concurrent reads; Replace serializes
// writes so per-file incremental updates can run from a single watcher.
type Index struct {
	mu    sync.RWMutex
	sites map[string][]Site
}

func NewIndex() *Index {
	return &Index{sites: map[string][]Site{}}
}

// Lookup returns every site for name, or nil if unknown. The returned
// slice is a copy; callers may mutate it freely.
func (i *Index) Lookup(name string) []Site {
	i.mu.RLock()
	defer i.mu.RUnlock()
	src := i.sites[name]
	if src == nil {
		return nil
	}
	out := make([]Site, len(src))
	copy(out, src)
	return out
}

// Names returns every indexed name, sorted. Useful for diagnostics.
func (i *Index) Names() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]string, 0, len(i.sites))
	for k := range i.sites {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Refresh atomically clears every site whose File matches file and
// inserts the supplied hits in its place. This is the public update path
// used by watchers on didSave.
func (i *Index) Refresh(file, language string, hits []Hit) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.clearFileLocked(file)
	i.insertLocked(file, language, hits)
}

// addHits inserts without clearing. Used by Build, which constructs from
// scratch and never re-visits a file.
func (i *Index) addHits(file, language string, hits []Hit) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.insertLocked(file, language, hits)
}

func (i *Index) clearFileLocked(file string) {
	for name, list := range i.sites {
		kept := make([]Site, 0, len(list))
		for _, s := range list {
			if s.File != file {
				kept = append(kept, s)
			}
		}
		if len(kept) == 0 {
			delete(i.sites, name)
		} else {
			i.sites[name] = kept
		}
	}
}

func (i *Index) insertLocked(file, language string, hits []Hit) {
	for _, h := range hits {
		i.sites[h.Name] = append(i.sites[h.Name], Site{
			File:       file,
			Line:       h.Line,
			Col:        h.Col,
			Language:   language,
			Confidence: ConfidenceLexical,
		})
	}
}

// Hit is one occurrence of a name, language-agnostic. Extractors return
// Hits; the Index attaches file + language + confidence.
type Hit struct {
	Name string
	Line int
	Col  int
}

// Extractor pulls Hits out of file contents. Implementations are expected
// to be deterministic and side-effect-free.
type Extractor interface {
	Extract(content []byte) []Hit
}

// LexicalExtractor matches `[A-Za-z_][A-Za-z0-9_]*` tokens line-by-line.
// Words in Keywords are dropped (language keywords / common builtins).
// Comments and string literals are NOT skipped — this is the Tier 1 noise
// budget. Tree-sitter will replace this per language in v0.3.
type LexicalExtractor struct {
	Keywords map[string]struct{}
}

var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

func (e *LexicalExtractor) Extract(content []byte) []Hit {
	var hits []Hit
	for lineIdx, line := range bytes.Split(content, []byte("\n")) {
		for _, span := range identRe.FindAllIndex(line, -1) {
			tok := string(line[span[0]:span[1]])
			if _, ok := e.Keywords[tok]; ok {
				continue
			}
			hits = append(hits, Hit{
				Name: tok,
				Line: lineIdx + 1,
				Col:  span[0] + 1,
			})
		}
	}
	return hits
}

// keywordSet is a helper to build a Keywords map literal.
func keywordSet(words ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(words))
	for _, w := range words {
		out[w] = struct{}{}
	}
	return out
}

// defaultExtractors covers the languages baked into config.Default().
// For data formats (yaml/json/markdown) we keep every word — that's the
// whole point of the cross-language index for string-literal sites.
var defaultExtractors = map[string]Extractor{
	"go": &LexicalExtractor{Keywords: keywordSet(
		"break", "case", "chan", "const", "continue", "default", "defer",
		"else", "fallthrough", "for", "func", "go", "goto", "if", "import",
		"interface", "map", "package", "range", "return", "select", "struct",
		"switch", "type", "var",
		"true", "false", "nil", "iota",
		"string", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "byte", "rune",
		"float32", "float64", "bool", "any", "error",
	)},
	"typescript": &LexicalExtractor{Keywords: keywordSet(
		"break", "case", "catch", "class", "const", "continue", "debugger",
		"default", "delete", "do", "else", "enum", "export", "extends",
		"false", "finally", "for", "function", "if", "import", "in",
		"instanceof", "new", "null", "return", "super", "switch", "this",
		"throw", "true", "try", "typeof", "var", "void", "while", "with",
		"yield", "let", "async", "await", "of", "as", "from", "interface",
		"type", "implements", "readonly", "public", "private", "protected",
		"static", "abstract", "declare",
		"string", "number", "boolean", "any", "unknown", "never", "object",
	)},
	"python": &LexicalExtractor{Keywords: keywordSet(
		"False", "None", "True", "and", "as", "assert", "async", "await",
		"break", "class", "continue", "def", "del", "elif", "else", "except",
		"finally", "for", "from", "global", "if", "import", "in", "is",
		"lambda", "nonlocal", "not", "or", "pass", "raise", "return", "try",
		"while", "with", "yield",
		"int", "float", "str", "bool", "bytes", "list", "dict", "tuple",
		"set", "frozenset", "type", "object", "print",
	)},
	"yaml":     &LexicalExtractor{},
	"json":     &LexicalExtractor{},
	"markdown": &LexicalExtractor{},
}

// DefaultExtractor returns the registered extractor for a language, or
// nil if none. Languages without an extractor are skipped during Build.
func DefaultExtractor(language string) Extractor {
	return defaultExtractors[language]
}

// skipDirs are directories never descended into during Build. Hardcoded
// for now; promote to config when a real workspace needs it.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"__pycache__":  true,
	"dist":         true,
	"build":        true,
	".idea":        true,
	".vscode":      true,
}

// maxFileSize caps any single file we'll index, in bytes. Files larger
// than this are silently skipped — generated bundles are not the target.
const maxFileSize = 1 << 20 // 1 MiB

// Build walks root recursively and indexes every file whose extension is
// registered in reg. Returns the populated Index. The walk is sequential;
// concurrency comes in Phase 3 alongside the stacked-branch index.
func Build(root string, reg *config.Registry) (*Index, error) {
	idx := NewIndex()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		ext := strings.TrimPrefix(filepath.Ext(path), ".")
		if ext == "" {
			return nil
		}
		lang := reg.LookupByExt(ext)
		if lang == nil {
			return nil
		}
		ex := DefaultExtractor(lang.Name)
		if ex == nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxFileSize {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		idx.addHits(path, lang.Name, ex.Extract(content))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return idx, nil
}
