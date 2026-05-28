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

	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/sql"
	"github.com/smacker/go-tree-sitter/typescript/tsx"

	"github.com/iodesystems/tslsmcp/internal/config"
)

// goIdentifierQuery captures every node Go's grammar uses to name a
// program entity. Comments, string literals, and keywords are not
// matched — that's the precision win over LexicalExtractor.
const goIdentifierQuery = `[
  (identifier)
  (field_identifier)
  (type_identifier)
  (package_identifier)
] @name`

// tsxIdentifierQuery covers TypeScript (and TSX/JSX) names: variables,
// type names, object properties, and JSX attribute keys. The tsx
// grammar is a superset of typescript so it parses both .ts and .tsx.
// shorthand_property_identifier is included so `{userId}` in object
// literals lands in the index.
const tsxIdentifierQuery = `[
  (identifier)
  (type_identifier)
  (property_identifier)
  (shorthand_property_identifier)
] @name`

// sqlIdentifierQuery — the SQL grammar doesn't distinguish identifier
// kinds, so one capture covers table names, column names, and index
// names. Data types (BIGINT, TEXT, …) and DDL keywords (CREATE, TABLE,
// NOT, NULL, PRIMARY, KEY) are non-identifier nodes — they don't match.
const sqlIdentifierQuery = `(identifier) @name`

// pythonIdentifierQuery — Python's grammar uses one identifier node for
// all the cases that matter to us (functions, classes, variables,
// parameters, type annotations, attribute accesses, decorator names,
// and f-string interpolations). Keywords (def/class/if/for/…) are
// non-identifier nodes; the keyword filter is for the builtins
// (int/str/print/…) that the grammar reports as identifier nodes.
const pythonIdentifierQuery = `(identifier) @name`

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

// Index maps name → []Site. Safe for concurrent reads; writes serialize
// so per-file incremental updates can run from a single watcher.
//
// Two backing stores share the same name space: `sites` holds lexical
// hits from extractors and is rebuilt on every Refresh, while
// `declaredSites` holds Tier-2 declared bindings and is rebuilt only on
// config reloads. Lookup merges both, with declared sites overriding
// lexical at the same (file, line, col).
type Index struct {
	mu sync.RWMutex

	sites         map[string][]Site
	declaredSites map[string][]Site
}

func NewIndex() *Index {
	return &Index{
		sites:         map[string][]Site{},
		declaredSites: map[string][]Site{},
	}
}

// Lookup returns every site for name, declared first then lexical, with
// duplicates at the same (file, line, col) dropped (declared wins). The
// returned slice is a copy; callers may mutate it freely.
func (i *Index) Lookup(name string) []Site {
	i.mu.RLock()
	defer i.mu.RUnlock()
	type key struct {
		file string
		line int
		col  int
	}
	seen := map[key]bool{}
	var out []Site
	for _, s := range i.declaredSites[name] {
		seen[key{s.File, s.Line, s.Col}] = true
		out = append(out, s)
	}
	for _, s := range i.sites[name] {
		if seen[key{s.File, s.Line, s.Col}] {
			continue
		}
		out = append(out, s)
	}
	return out
}

// InsertDeclared registers a declared binding site. The caller has
// already resolved the position from a config.Binding; this method just
// stores it. Confidence on the stored Site is forced to
// ConfidenceDeclared regardless of what the caller passes — declared
// status is the whole point of using this entry point.
func (i *Index) InsertDeclared(name, file, language string, line, col int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.declaredSites[name] = append(i.declaredSites[name], Site{
		File:       file,
		Line:       line,
		Col:        col,
		Language:   language,
		Confidence: ConfidenceDeclared,
	})
}

// ClearDeclared removes every declared binding. Called on config reload
// before re-applying the new bindings from disk.
func (i *Index) ClearDeclared() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.declaredSites = map[string][]Site{}
}

// Languages returns the distinct language names with at least one site
// in the index, sorted. Considers both lexical and declared sites; used
// by the multiplex layer to decide which child LSPs are worth spawning.
func (i *Index) Languages() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	set := map[string]struct{}{}
	for _, sites := range i.sites {
		for _, s := range sites {
			set[s.Language] = struct{}{}
		}
	}
	for _, sites := range i.declaredSites {
		for _, s := range sites {
			set[s.Language] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Names returns every indexed name (lexical or declared), sorted.
func (i *Index) Names() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	set := map[string]struct{}{}
	for k := range i.sites {
		set[k] = struct{}{}
	}
	for k := range i.declaredSites {
		set[k] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
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
	// Go uses tree-sitter so identifier-shaped tokens inside string
	// literals and comments stop polluting the index. Keywords are
	// kept for builtin types (int64, string, etc.) which the grammar
	// reports as type_identifier nodes.
	"go": mustTreeSitterExtractor(golang.GetLanguage(), goIdentifierQuery, keywordSet(
		"string", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "byte", "rune",
		"float32", "float64", "bool", "any", "error",
		"true", "false", "nil", "iota",
	)),
	// TypeScript (and .tsx) via tsx grammar. The keyword filter is
	// shorter than the lexical set because tree-sitter already drops
	// `function`/`return`/`export`/etc. as non-identifier nodes — we
	// only need to subtract the type-name builtins the grammar surfaces
	// as type_identifier.
	"typescript": mustTreeSitterExtractor(tsx.GetLanguage(), tsxIdentifierQuery, keywordSet(
		"string", "number", "boolean", "any", "unknown", "never",
		"object", "void", "undefined", "null",
	)),
	"sql": mustTreeSitterExtractor(sql.GetLanguage(), sqlIdentifierQuery, nil),
	// Python via tree-sitter. Keywords filter is trimmed to the builtins
	// the grammar surfaces as identifier nodes — proper Python keywords
	// (def/class/import/…) are non-identifier nodes and would never match
	// the query.
	"python": mustTreeSitterExtractor(python.GetLanguage(), pythonIdentifierQuery, keywordSet(
		"int", "float", "str", "bool", "bytes", "list", "dict", "tuple",
		"set", "frozenset", "type", "object", "print", "len", "range",
		"True", "False", "None",
	)),
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
