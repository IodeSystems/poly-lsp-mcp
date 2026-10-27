// Package symbols is the cross-language index that sits above multiplex.
// It answers "where does this name appear?" across every file regardless
// of which child LSP owns it — the unique value-add of poly-lsp-mcp vs any
// single-language LSP.
//
// Three tiers:
//
//   - Tier 1 (this file): lexical. Word-token extraction with optional
//     keyword filtering per language. Cheap, noisy, useful for
//     workspace/symbol and as a soft signal for textDocument/references.
//   - Tier 2: declared bindings from poly-lsp-mcp.yaml. Precise; drives safe
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
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/git"
	"github.com/iodesystems/poly-lsp-mcp/internal/pathpat"
	sitter "github.com/iodesystems/poly-lsp-mcp/internal/ts"
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

// javaIdentifierQuery covers Java names: variables, fields, methods,
// classes, interfaces, enums, annotations and type parameters. Java's
// grammar splits plain names (identifier) from type positions
// (type_identifier), and scoped names like com.termux.view come through
// as nested identifiers, so each segment is indexed on its own — which
// is what makes a fully-qualified class name written inside an Android
// XML attribute findable from the Java side.
const javaIdentifierQuery = `[
  (identifier)
  (type_identifier)
] @name`

// kotlinIdentifierQuery covers Kotlin names. The grammar has exactly two
// identifier nodes: simple_identifier for everything that isn't a type
// position (functions, properties, parameters, enum entries, call
// targets, package/import segments) and type_identifier for the rest.
// Keywords, modifiers and operators are distinct node types and never
// match.
const kotlinIdentifierQuery = `[
  (simple_identifier)
  (type_identifier)
] @name`

// groovyIdentifierQuery — the Groovy grammar has ONE identifier node for
// every name: declarations, types, call targets, property access, and the
// segments of a package or import path. Primitive types are `builtintype`
// nodes and never match. String CONTENTS are not names, which for a
// build script is a real loss (a Gradle dependency coordinate lives in a
// string literal) — the same tradeoff Java makes, and the Android
// binding is the pattern for buying it back where it matters.
const groovyIdentifierQuery = `(identifier) @name`

// swiftIdentifierQuery covers Swift names. Like Kotlin's grammar (the
// same lineage), there are two identifier nodes: simple_identifier for
// every non-type name (functions, properties, parameters and their
// labels, enum cases, call targets, import segments, names inside a
// string interpolation) and type_identifier for types. Keywords and
// operators are distinct node types and never match.
const swiftIdentifierQuery = `[
  (simple_identifier)
  (type_identifier)
] @name`

// cIdentifierQuery covers C names: variables, functions, parameters,
// struct/union/enum tags, field names, typedef names and goto labels.
// Macro BODIES are deliberately invisible — the grammar hands them back
// as one opaque preproc_arg token, so an identifier that only ever
// appears inside a `#define` expansion is not indexed. That is the one
// place where C's tree-sitter tier sees less than a lexical pass would.
const cIdentifierQuery = `[
  (identifier)
  (type_identifier)
  (field_identifier)
  (statement_identifier)
] @name`

// cppIdentifierQuery is cIdentifierQuery plus namespace_identifier,
// which the C++ grammar uses for both `namespace app {` and the scope
// half of a qualified name (std::string, Widget::area). Indexing the
// scope segment separately is what makes `Widget` findable from an
// out-of-line definition.
const cppIdentifierQuery = `[
  (identifier)
  (type_identifier)
  (field_identifier)
  (namespace_identifier)
  (statement_identifier)
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
// Higher values win on same-position dedup in Lookup.
type Confidence int

const (
	ConfidenceComment  Confidence = iota // soft reference from @see / @link in a comment
	ConfidenceLexical                    // word-token match; high recall, low precision
	ConfidenceDeclared                   // user-declared binding (Tier 2/3) or @ref / x-ref marker
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
	commentSites  map[string][]Site
	// pluginEdges holds sites produced by convention plugins (F2), keyed
	// by the plugin's kind name. Separate from the three stores above so
	// that zero plugins → zero plugin edges → the index is unchanged.
	pluginEdges map[string][]Site

	// gen bumps on every mutation. Derived state (the query-cost
	// estimator's tallies, the inversion cache) memoizes against
	// it: same gen → reuse, otherwise recompute. A no-op mutation may
	// bump it too — a spurious recompute is cheap; stale tallies are not.
	gen uint64

	// invByFile caches the file → sites inversion (SitesByFile), keyed on
	// invGen. Every edge query wants this shape; building it is O(all
	// occurrences), so memoizing it here means the second edge query
	// against an unchanged index pays nothing. nil until first asked.
	invByFile map[string][]InvSite
	invGen    uint64
}

// InvSite is one occurrence in the file → sites inversion: a name at a
// position, before any per-file classification. Line/Col are 1-based.
type InvSite struct {
	Name string
	Line int
	Col  int
}

// Generation returns the index's mutation counter — the memo key for any
// derived-from-the-index state.
func (i *Index) Generation() uint64 {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.gen
}

// Cardinality is the O(1) count of distinct lexical names — a broad
// proxy for "how many symbols" when a query planner needs to know an
// element is NOT selective without walking the tree for an exact class
// count.
func (i *Index) Cardinality() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.sites)
}

// NameFreq is an O(1) a-priori estimate of how often a name occurs — the
// raw site tally across the three stores, no per-position dedup (Lookup's
// job). It is the cheap selectivity signal the query-cost estimator and
// the pushdown reason about: a rare name narrows hard, a common one does
// not. An estimate, deliberately not the exact deduped count.
func (i *Index) NameFreq(name string) int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.sites[name]) + len(i.declaredSites[name]) +
		len(i.commentSites[name]) + len(i.pluginEdges[name])
}

func NewIndex() *Index {
	return &Index{
		sites:         map[string][]Site{},
		declaredSites: map[string][]Site{},
		commentSites:  map[string][]Site{},
		pluginEdges:   map[string][]Site{},
	}
}

// Lookup returns every site for name from all stores (declared, lexical,
// comment, plugin) with same-(file, line, col) dedup — higher
// confidence wins. The returned slice is a copy; callers may mutate
// it freely.
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
		k := key{s.File, s.Line, s.Col}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	for _, s := range i.commentSites[name] {
		if seen[key{s.File, s.Line, s.Col}] {
			continue
		}
		out = append(out, s)
	}
	for _, s := range i.pluginEdges[name] {
		if seen[key{s.File, s.Line, s.Col}] {
			continue
		}
		out = append(out, s)
	}
	return out
}

// fileExists reports whether path is present on disk.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// LookupExisting is Lookup with a self-healing pass: any returned site
// whose File no longer exists on disk is dropped from the result AND
// evicted from the index. The index is built once at startup and only
// updated per-file on edits the tool performs itself, so a file removed
// by an outside process (git checkout, git mv, another editor) otherwise
// leaves orphan sites that surface as dead paths — the headless MCP
// server has no filesystem watcher to catch the removal. Stat-ing at
// query time keeps node_references / rename honest without one.
func (i *Index) LookupExisting(name string) []Site {
	all := i.Lookup(name) // a fresh copy — safe to compact in place
	live := all[:0]
	var dead []string
	seen := map[string]bool{}
	for _, s := range all {
		if fileExists(s.File) {
			live = append(live, s)
			continue
		}
		if !seen[s.File] {
			seen[s.File] = true
			dead = append(dead, s.File)
		}
	}
	if len(dead) > 0 {
		i.RemoveFiles(dead)
	}
	return live
}

// SitesByFile inverts the index — name → sites becomes file → sites, the
// shape every edge query wants — and MEMOIZES the result on gen. The
// build sweeps every occurrence once (O(total sites)) and stats each
// unique file once for liveness, evicting any that vanished. Because it
// is index-owned derived state (like the estimator tallies), the second
// edge query against an unchanged index reuses it for free instead of
// re-inverting; the engine used to rebuild this per query, which measured
// as ~85% of an anchored edge query's work budget.
//
// Keys are ABSOLUTE file paths (the index's native form); the caller maps
// to workspace-relative as it likes. The returned map is shared and must
// be treated read-only — a mutation builds a fresh map under the next gen.
func (i *Index) SitesByFile() map[string][]InvSite {
	i.mu.RLock()
	if i.invByFile != nil && i.invGen == i.gen {
		m := i.invByFile
		i.mu.RUnlock()
		return m
	}
	i.mu.RUnlock()

	i.mu.Lock()
	defer i.mu.Unlock()
	// Re-check: another goroutine may have built it between the unlock
	// and the relock.
	if i.invByFile != nil && i.invGen == i.gen {
		return i.invByFile
	}
	inv := i.buildInversionLocked()
	// buildInversionLocked may have evicted vanished files, bumping gen;
	// cache against the CURRENT (post-eviction) gen so the next call hits.
	i.invByFile = inv
	i.invGen = i.gen
	return inv
}

// buildInversionLocked constructs the file → sites map, assuming the
// write lock is held. It dedups by (file, line, col) — a position holds
// exactly one name, so a name declared AND lexically seen at the same
// spot counts once, matching Lookup — and drops (plus evicts) sites whose
// file no longer exists, the self-heal LookupExisting does per query.
func (i *Index) buildInversionLocked() map[string][]InvSite {
	type pos struct {
		file string
		line int
		col  int
	}
	seen := map[pos]bool{}
	alive := map[string]bool{}
	var dead []string
	inv := map[string][]InvSite{}
	add := func(name string, s Site) {
		p := pos{s.File, s.Line, s.Col}
		if seen[p] {
			return
		}
		seen[p] = true
		ok, known := alive[s.File]
		if !known {
			ok = fileExists(s.File)
			alive[s.File] = ok
			if !ok {
				dead = append(dead, s.File)
			}
		}
		if !ok {
			return
		}
		inv[s.File] = append(inv[s.File], InvSite{Name: name, Line: s.Line, Col: s.Col})
	}
	// declared → lexical → comment, matching Lookup's precedence so the
	// first-seen dedup keeps the same occurrence.
	for name, sites := range i.declaredSites {
		for _, s := range sites {
			add(name, s)
		}
	}
	for name, sites := range i.sites {
		for _, s := range sites {
			add(name, s)
		}
	}
	for name, sites := range i.commentSites {
		for _, s := range sites {
			add(name, s)
		}
	}
	for name, sites := range i.pluginEdges {
		for _, s := range sites {
			add(name, s)
		}
	}
	if len(dead) > 0 {
		i.removeFilesLocked(dead)
	}
	return inv
}

// RemoveFiles evicts every site (lexical, declared, comment) whose File
// is in files, across all names — the multi-store, multi-name companion
// to clearFileLocked. Used to reconcile the index when files disappear
// from disk outside the tool's edit path.
func (i *Index) RemoveFiles(files []string) {
	if len(files) == 0 {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.removeFilesLocked(files)
}

// removeFilesLocked is RemoveFiles' body, assuming the write lock is
// held. Split out so the inversion build — which already holds the lock
// — can evict vanished files it discovers without re-locking.
func (i *Index) removeFilesLocked(files []string) {
	if len(files) == 0 {
		return
	}
	gone := make(map[string]bool, len(files))
	for _, f := range files {
		gone[f] = true
	}
	i.gen++
	for _, store := range []map[string][]Site{i.sites, i.declaredSites, i.commentSites, i.pluginEdges} {
		for name, list := range store {
			kept := make([]Site, 0, len(list))
			for _, s := range list {
				if !gone[s.File] {
					kept = append(kept, s)
				}
			}
			if len(kept) == 0 {
				delete(store, name)
			} else {
				store[name] = kept
			}
		}
	}
}

// InsertDeclared registers a declared binding site. Idempotent: a
// second call with the same (name, file, line, col) is a no-op so
// callers that union multiple sources (user bindings + schemas) don't
// produce duplicate edits at rename time.
func (i *Index) InsertDeclared(name, file, language string, line, col int) {
	i.mu.Lock()
	i.gen++
	defer i.mu.Unlock()
	for _, s := range i.declaredSites[name] {
		if s.File == file && s.Line == line && s.Col == col {
			return
		}
	}
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
	i.gen++
	defer i.mu.Unlock()
	i.declaredSites = map[string][]Site{}
}

// InsertComment registers a soft reference produced by a comment-level
// marker (@see / @link). Idempotent at (name, file, line, col).
// Comment sites are visible to node_references but skipped by rename's
// default policy.
func (i *Index) InsertComment(name, file, language string, line, col int) {
	i.mu.Lock()
	i.gen++
	defer i.mu.Unlock()
	for _, s := range i.commentSites[name] {
		if s.File == file && s.Line == line && s.Col == col {
			return
		}
	}
	i.commentSites[name] = append(i.commentSites[name], Site{
		File:       file,
		Line:       line,
		Col:        col,
		Language:   language,
		Confidence: ConfidenceComment,
	})
}

// RefreshCommentsForFile replaces every comment-confidence site
// associated with file. Called after node_edit / node_delete so the
// new file content's @see / @link / @ref markers replace the prior
// snapshot.
func (i *Index) RefreshCommentsForFile(file string) {
	i.mu.Lock()
	i.gen++
	defer i.mu.Unlock()
	i.clearCommentFileLocked(file)
}

func (i *Index) clearCommentFileLocked(file string) {
	for name, list := range i.commentSites {
		kept := make([]Site, 0, len(list))
		for _, s := range list {
			if s.File != file {
				kept = append(kept, s)
			}
		}
		if len(kept) == 0 {
			delete(i.commentSites, name)
		} else {
			i.commentSites[name] = kept
		}
	}
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

// DeclaredNames returns every name with at least one declared site,
// sorted. Lexical-only names are excluded — this is the binding
// catalog Tier 2 + Tier 3 produced for the workspace.
func (i *Index) DeclaredNames() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]string, 0, len(i.declaredSites))
	for name, sites := range i.declaredSites {
		if len(sites) > 0 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Names returns every indexed name (lexical, declared, or comment),
// sorted. Comment-only names are included so a marker like @see Foo in
// a markdown file still surfaces in workspace symbol enumeration.
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
	for k := range i.commentSites {
		set[k] = struct{}{}
	}
	for k := range i.pluginEdges {
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
	i.gen++
	defer i.mu.Unlock()
	i.clearFileLocked(file)
	i.insertLocked(file, language, hits)
}

// addHits inserts without clearing. Used by Build, which constructs from
// scratch and never re-visits a file.
func (i *Index) addHits(file, language string, hits []Hit) {
	i.mu.Lock()
	i.gen++
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

// cKeywords is shared by the c and cpp extractors: the stdlib names both
// dialects surface as identifier / type_identifier nodes and that carry
// no signal in a cross-file index.
var cKeywords = keywordSet(
	"NULL", "nullptr", "true", "false",
	"size_t", "ssize_t", "ptrdiff_t", "wchar_t", "bool",
	"int8_t", "int16_t", "int32_t", "int64_t",
	"uint8_t", "uint16_t", "uint32_t", "uint64_t",
	"intptr_t", "uintptr_t",
)

// defaultExtractors covers the languages baked into config.Default().
// For data formats (yaml/json/markdown) we keep every word — that's the
// whole point of the cross-language index for string-literal sites.
var defaultExtractors = map[string]Extractor{
	// Go uses tree-sitter so identifier-shaped tokens inside string
	// literals and comments stop polluting the index. Keywords are
	// kept for builtin types (int64, string, etc.) which the grammar
	// reports as type_identifier nodes.
	"go": mustTreeSitterExtractor(sitter.Go(), goIdentifierQuery, keywordSet(
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
	// Wrapped so GraphQL embedded in graphql(`...`) / gql`...` template literals
	// (the graphql-codegen / graphql-request pattern) is indexed too — otherwise a
	// GraphQL field reference inside a TS query string is invisible (the body is an
	// opaque string node), which breaks cross-language rename of gat/codegen ops.
	"typescript": newEmbeddedGraphQLExtractor(
		mustTreeSitterExtractor(sitter.TSX(), tsxIdentifierQuery, keywordSet(
			"string", "number", "boolean", "any", "unknown", "never",
			"object", "void", "undefined", "null",
		)),
		sitter.TSX(),
	),
	"sql": mustTreeSitterExtractor(sitter.SQL(), sqlIdentifierQuery, nil),
	// Python via tree-sitter. Keywords filter is trimmed to the builtins
	// the grammar surfaces as identifier nodes — proper Python keywords
	// (def/class/import/…) are non-identifier nodes and would never match
	// the query.
	"python": mustTreeSitterExtractor(sitter.Python(), pythonIdentifierQuery, keywordSet(
		"int", "float", "str", "bool", "bytes", "list", "dict", "tuple",
		"set", "frozenset", "type", "object", "print", "len", "range",
		"True", "False", "None",
	)),
	// Java via tree-sitter. The keyword filter only subtracts the
	// java.lang types the grammar surfaces as type_identifier —
	// `int`/`boolean`/etc. are primitive-type nodes and never match the
	// query, and annotations (@Override) are deliberately kept so they
	// stay queryable.
	"java": mustTreeSitterExtractor(sitter.Java(), javaIdentifierQuery, keywordSet(
		"String", "Object", "Integer", "Boolean", "Long", "Double",
		"Float", "Byte", "Short", "Character", "Void",
	)),
	// C / C++ via tree-sitter. `int`/`char`/`void`/`bool` are
	// primitive_type nodes and never match the query, so the filter only
	// subtracts the stdlib spellings that DO arrive as identifiers —
	// mostly stdint/stddef typedefs and the null/bool literals.
	"c":   mustTreeSitterExtractor(sitter.C(), cIdentifierQuery, cKeywords),
	"cpp": mustTreeSitterExtractor(sitter.Cpp(), cppIdentifierQuery, cKeywords),

	// Kotlin via tree-sitter. The filter subtracts the stdlib types the
	// grammar reports as type_identifier plus `it` — the implicit lambda
	// parameter, which is file-local by construction and would otherwise
	// be one of the most frequent names in any Kotlin repo.
	"kotlin": mustTreeSitterExtractor(sitter.Kotlin(), kotlinIdentifierQuery, keywordSet(
		"String", "Int", "Long", "Short", "Byte", "Boolean", "Char",
		"Float", "Double", "Unit", "Any", "Nothing", "Array",
		"List", "Map", "Set", "Pair", "it", "true", "false", "null",
	)),

	// Groovy via tree-sitter. The filter subtracts the java.lang types
	// the grammar reports as plain identifiers; `def`, `class` and the
	// modifiers are distinct node types and never match.
	"groovy": mustTreeSitterExtractor(sitter.Groovy(), groovyIdentifierQuery, keywordSet(
		"String", "Object", "Integer", "Boolean", "Long", "Double",
		"Float", "Byte", "Short", "Character", "List", "Map", "Set",
		"true", "false", "null", "it", "this",
	)),

	// Swift via tree-sitter. The filter subtracts the stdlib types the
	// grammar reports as type_identifier, the `_` that marks an unlabeled
	// parameter, and the implicit accessor / closure names, which are
	// local by construction and among the most frequent names in any
	// Swift file.
	"swift": mustTreeSitterExtractor(sitter.Swift(), swiftIdentifierQuery, keywordSet(
		"String", "Int", "Double", "Float", "Bool", "Character", "Void",
		"Any", "AnyObject", "Never", "Self", "Array", "Dictionary", "Set",
		"Optional", "_", "newValue", "oldValue", "$0", "$1", "$2",
	)),

	"yaml": &LexicalExtractor{},
	"json": &LexicalExtractor{},
	// XML gets a real parser, not the lexical fallback. The cross-language
	// edge on Android is a resource NAME in an attribute value — @+id/x,
	// name="x", app:key="x" — and indexing every token instead buried those
	// under android:/layout_*/match_parent noise (331 hits vs 33 on one
	// layout). See XMLExtractor for why encoding/xml and not the html grammar.
	"xml": XMLExtractor{},
	// Markdown indexes HEADINGS, fenced code and inline code spans only,
	// not prose — see MarkdownExtractor for the measurements behind it.
	"markdown": &MarkdownExtractor{},
	// Proto and GraphQL SDL deliberately use the lexical extractor —
	// not the bindings-side proto schema parser. The walker's job is
	// to surface every identifier-shaped token AND let the
	// comment-marker scanner pick up @ref / @see annotations.
	// Generated artifacts (gat-rendered .graphql, codegen .proto)
	// frequently embed cross-language back-references in comments.
	"proto":   &LexicalExtractor{},
	"graphql": &LexicalExtractor{},
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
	// Gradle's per-project cache. It was harmless while we indexed no
	// C/C++: on reposilite it holds a downloaded Node toolchain whose
	// 2,290 bundled .h files are 1.5M lexical sites — 35x the rest of
	// the repo put together, none of it the user's code.
	".gradle": true,
	// iOS / Swift. CocoaPods and Carthage vendor dependencies (Pods/ is
	// the iOS node_modules, often committed), SwiftPM checks them out
	// under .build/ and keeps Xcode state in .swiftpm/, Xcode writes
	// DerivedData/ (a project-local one when so configured) and per-user
	// UI state in xcuserdata/.
	"Pods":        true,
	"Carthage":    true,
	".build":      true,
	".swiftpm":    true,
	"DerivedData": true,
	"xcuserdata":  true,
}

// skipDirSuffixes are directory-name suffixes skipped like skipDirs. An
// asset catalog (Assets.xcassets) is a tree of Contents.json files, one
// per image set, each naming "images", "idiom", "scale" and "filename":
// JSON the index would read as hundreds of identical names.
var skipDirSuffixes = []string{".xcassets"}

// IsSkipDir reports whether a directory name is one the index skips. The
// node_query walk and the proactive open skip the same directories.
func IsSkipDir(name string) bool {
	if skipDirs[name] {
		return true
	}
	for _, s := range skipDirSuffixes {
		if len(name) > len(s) && strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// maxFileSize caps any single file we'll index, in bytes. A file larger
// than this is skipped and reported as LeftOverSize — generated bundles are
// not the target. --include indexes one anyway.
const maxFileSize = 1 << 20 // 1 MiB

// lfsPointer is how every Git LFS pointer file begins. A repository cloned
// without `git lfs pull` holds these ~130-byte stubs in place of the
// content; indexing one indexes "version", "oid" and "sha256".
var lfsPointer = []byte("version https://git-lfs.github.com/spec/v1")

// DefaultSymlinkDepth bounds nested symlink hops: a link inside a followed
// dir link is two deep. Four covers a monorepo's shared-package links
// without letting a link farm multiply the tree.
const DefaultSymlinkDepth = 4

// minified reports whether a base name is a *.min.* file (foo.min.js,
// foo.min.css): one line of generated code, every name in it a duplicate
// of its source or a mangled one-letter local.
func minified(base string) bool {
	i := strings.Index(base, ".min.")
	return i > 0 && i+len(".min.") < len(base)
}

// BuildOption tunes Build. Variadic so callers without special needs
// can keep using the bare two-argument form.
type BuildOption func(*buildConfig)

type buildConfig struct {
	cache *ParseCache
	// noGitignore disables the .gitignore filter. Default is to honour
	// it; this is the escape hatch for a workspace that deliberately
	// ignores files it still wants indexed.
	noGitignore bool
	// plugins are convention extractors (F2) that run after the lexical
	// walk and add typed, confidence-labelled edges to the index.
	plugins []Plugin
	// cost, if set, records what each directory cost the walk.
	cost *BuildCost
	// ignores, if set, replaces the set Build would load from git — so a
	// caller's excludes apply, and the set is loaded once per caller.
	ignores *git.IgnoreSet
	// budget bounds the walk. See WithBudget.
	budget Budget
	// manifest, if set, is the file set to index instead of a walk.
	manifest *git.Manifest
	// include overrides the heuristic exclusions. See WithInclude.
	include *pathpat.Set
	// symlinkDepth bounds nested symlink hops; set by WithSymlinkDepth,
	// else DefaultSymlinkDepth.
	symlinkDepth    int
	symlinkDepthSet bool
}

// WithManifest indexes the files git lists instead of walking root. nil
// keeps the walk (not a repo).
func WithManifest(m *git.Manifest) BuildOption {
	return func(cfg *buildConfig) { cfg.manifest = m }
}

// WithInclude indexes what inc covers even when a heuristic would leave it
// out: over size, minified, LFS pointer, a skipped dir, a nested repository
// the manifest lists, a symlink whose target leaves the root. It does not
// override .gitignore or --exclude: those are the repository's and the
// client's own decisions, not a guess.
func WithInclude(inc *pathpat.Set) BuildOption {
	return func(cfg *buildConfig) { cfg.include = inc }
}

// WithSymlinkDepth follows symlinks at most n deep; 0 follows none.
func WithSymlinkDepth(n int) BuildOption {
	return func(cfg *buildConfig) { cfg.symlinkDepth, cfg.symlinkDepthSet = n, true }
}

// WithCache plumbs a ParseCache through Build. Files whose
// (language, content) is cached are reused without re-running the
// extractor. Misses populate the cache as a side effect so subsequent
// builds reuse them. Pass the same *ParseCache across Build calls to
// realize the cache hit across refreshes.
func WithCache(c *ParseCache) BuildOption {
	return func(cfg *buildConfig) { cfg.cache = c }
}

// WithoutGitignore indexes gitignored files too. Build honours
// `.gitignore` by default — see ignoreSet for the measurement that
// motivated it — and this turns that off for a workspace that ignores
// files it nevertheless wants in the index.
func WithoutGitignore() BuildOption {
	return func(cfg *buildConfig) { cfg.noGitignore = true }
}

// WithIgnores uses ign as the ignore filter instead of loading one from
// git. Ignored under WithoutGitignore.
func WithIgnores(ign *git.IgnoreSet) BuildOption {
	return func(cfg *buildConfig) { cfg.ignores = ign }
}

// Budget bounds a Build, in node_query's terms: Time is wall-clock, Ops is
// files read and parsed (deterministic). Zero fields are unbounded.
type Budget struct {
	Time time.Duration
	Ops  int
}

func (b Budget) String() string {
	if b.Ops > 0 {
		return fmt.Sprintf("%dops", b.Ops)
	}
	return fmt.Sprintf("%dms", b.Time.Milliseconds())
}

func (b Budget) set() bool { return b.Time > 0 || b.Ops > 0 }

// WithBudget makes Build fail with a *BudgetError when it cannot index the
// workspace within b.
func WithBudget(b Budget) BuildOption {
	return func(cfg *buildConfig) { cfg.budget = b }
}

// After the budget is spent the walk goes on counting files and bytes,
// without reading them, so the trace shows where the rest of the workspace
// is. That counting is bounded too — budgetCountFactor times the files read,
// and no more time than the budget itself — so a root at $HOME still
// returns.
const budgetCountFactor = 20

// BudgetError is a build that blew its budget. Report is the trace: every
// file read within the budget, and every file counted after it.
type BudgetError struct {
	Budget Budget
	Read   int // files read and parsed within the budget
	Report CostReport
	// Stopped: the counting walk itself ran out, so Report.Files is a lower
	// bound.
	Stopped bool
}

func (e *BudgetError) Error() string {
	n := fmt.Sprintf("%d", e.Report.Files)
	if e.Stopped {
		n = "at least " + n
	}
	return fmt.Sprintf("index budget %s spent after %d of %s indexable files", e.Budget, e.Read, n)
}

// Build walks root recursively and indexes every file whose extension is
// registered in reg. Returns the populated Index. The walk is sequential;
// concurrent walks are a possible future optimization.
func Build(root string, reg *config.Registry, opts ...BuildOption) (*Index, error) {
	var cfg buildConfig
	for _, o := range opts {
		o(&cfg)
	}
	idx := NewIndex()
	// Resolved once: one git invocation per build, not per entry. nil
	// when root is not a repo or git is unavailable, in which case the
	// walk proceeds exactly as before.
	var ign *git.IgnoreSet
	switch {
	case cfg.noGitignore:
	case cfg.ignores != nil:
		ign = cfg.ignores
	default:
		ign = git.LoadIgnores(root)
	}
	maxDepth := DefaultSymlinkDepth
	if cfg.symlinkDepthSet {
		maxDepth = cfg.symlinkDepth
	}
	inc := cfg.include
	var pluginFiles []PluginFile
	if cfg.budget.set() && cfg.cost == nil {
		cfg.cost = NewBuildCost() // the trace needs one
	}
	walkStart := time.Now()
	var spentAt time.Time
	if cfg.cost != nil {
		cfg.cost.begin(root)
		cfg.cost.symlinkDepth = maxDepth
		defer cfg.cost.end()
	}
	read, counted, over, stopped := 0, 0, false, false
	dupes := 0 // files a link led back to, already indexed
	shadow := reg.NewShadowChecker()

	// realRoot is root with its symlinks resolved, so a link's target can be
	// placed inside or outside it.
	realRoot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = r
	}
	// indexed holds every file read or counted, by key: its path for a file
	// reached without a link; for one reached through a link, its target's
	// path under root when inside it (so it matches that file's own key),
	// else the target itself. A link to a file already in it is not indexed
	// twice.
	indexed := map[string]bool{}
	keyOf := func(real string) string {
		if r, ok := within(realRoot, real); ok {
			return filepath.Join(root, r)
		}
		return real
	}
	relOf := func(p string) string {
		r, err := filepath.Rel(root, p)
		if err != nil {
			return ""
		}
		return filepath.ToSlash(r)
	}

	// visit indexes one file, at path (as indexed) and rel (root-relative,
	// slash-separated). key is its dedupe key (see indexed). info is asked
	// for only once the cheap checks pass. fs.SkipAll ends the build's file
	// loop (budget spent and the trace's count run out).
	visit := func(path, rel, key string, info func() (fs.FileInfo, error)) error {
		// .git is never indexed, include or not. A skipped dir is entered
		// only for what an include pattern covers.
		if skip := skipDirOf(rel); skip != "" {
			repoState := skip[strings.LastIndexByte(skip, '/')+1:] == ".git"
			if repoState || !inc.Covers(rel) {
				if !repoState {
					cfg.cost.skippedDir(skip)
				}
				return nil
			}
		}
		// KnownIgnored, not FileIgnored: every path a build visits existed
		// when LoadIgnores ran, so the snapshot already has the answer.
		// FileIgnored's git fallback spawned one `git check-ignore` per
		// non-ignored file — on dun, 20k files under untracked worktrees, it
		// was 48 of the 52 s initialize took.
		if ign.KnownIgnored(root, path) {
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
		// A lesser-typed file beside its stronger sibling (foo.js beside
		// foo.ts) would define every name twice. See config.ShadowRule.
		if rule, ok := shadow.Shadowed(path); ok {
			if cfg.cost != nil {
				cfg.cost.shadow(rule)
			}
			return nil
		}
		forced := inc.Covers(rel)
		if !forced && minified(filepath.Base(path)) {
			cfg.cost.LeaveOut(rel, LeftMinified)
			return nil
		}
		fi, err := info()
		if err != nil || !fi.Mode().IsRegular() {
			return nil // gone since git listed it, or not a file
		}
		// An included file is indexed whatever its size. The budget still
		// bounds the build: it is checked before each read, though a file
		// once started is read whole.
		if !forced && fi.Size() > maxFileSize {
			cfg.cost.LeaveOut(rel, LeftOverSize)
			return nil
		}
		if indexed[key] {
			dupes++ // reached once already, through a link
			return nil
		}
		if cfg.cost != nil {
			cfg.cost.lang(lang.Name)
		}
		if !over && cfg.budget.set() &&
			((cfg.budget.Ops > 0 && read >= cfg.budget.Ops) ||
				(cfg.budget.Time > 0 && time.Since(walkStart) > cfg.budget.Time)) {
			over, spentAt = true, time.Now()
		}
		if over {
			// Spent: count, do not read, so the trace shows where the rest
			// of the files are.
			counted++
			if counted > budgetCountFactor*max(read, 1) ||
				(cfg.budget.Time > 0 && time.Since(spentAt) > cfg.budget.Time) {
				stopped = true
				return fs.SkipAll
			}
			indexed[key] = true
			cfg.cost.file(path, fi.Size(), 0, false)
			return nil
		}
		fileStart := time.Now()
		content, err := os.ReadFile(path)
		if err != nil {
			// Skipped and named, not fatal: one unreadable file must not
			// cost the whole index.
			cfg.cost.unreadable(path)
			return nil
		}
		// Checked on the read already done: a pointer is a few hundred
		// bytes, so no separate open.
		if !forced && bytes.HasPrefix(content, lfsPointer) {
			cfg.cost.LeaveOut(rel, LeftLFS)
			return nil
		}
		indexed[key] = true
		read++
		pluginFiles = append(pluginFiles, PluginFile{
			Path:     path,
			Content:  content,
			Language: lang.Name,
		})
		var hits []Hit
		h, cached := cfg.cache.Get(lang.Name, content)
		if cached {
			hits = h
		} else {
			hits = ex.Extract(content)
			cfg.cache.Put(lang.Name, content, hits)
		}
		idx.addHits(path, lang.Name, hits)

		// Universal comment-marker pass — @see / {@link} → comment
		// confidence, @ref / x-ref → declared. Runs on every file
		// we walked, regardless of language.
		for _, ref := range ExtractCommentRefs(content) {
			switch ref.Confidence {
			case ConfidenceDeclared:
				idx.InsertDeclared(ref.Name, path, lang.Name, ref.Line, ref.Col)
			default:
				idx.InsertComment(ref.Name, path, lang.Name, ref.Line, ref.Col)
			}
		}
		if cfg.cost != nil {
			cfg.cost.file(path, fi.Size(), time.Since(fileStart), cached)
		}
		return nil
	}

	// Symlinks are followed after the files reached without one, so that a
	// link to a file or directory already indexed under its own path is
	// known as one and not indexed twice.
	type link struct{ path, phys string }
	var links []link

	// ownPath reports whether a directory inside the root, given relative to
	// it, is indexed under its own path: not under a skipped dir and not
	// ignored, excluded or a nested repository. A link to such a directory
	// would only index its files a second time.
	ownPath := func(rel string) bool {
		if rel == "." {
			return true
		}
		if skipDirOf(rel+"/x") != "" && !inc.Covers(rel) {
			return false
		}
		return !ign.DirIgnored(root, filepath.Join(root, filepath.FromSlash(rel)))
	}

	// walk walks phys, indexing each file under mapped (the same path,
	// except under a followed dir link, where phys is the target and mapped
	// the link). depth is how many links deep phys is; branch the real
	// directories open on its path, for loop detection. need, if set, must
	// pass for a file to be visited; own, if set, is the ignore set of a
	// nested repository walked as part of this one, rooted at phys.
	var walk func(phys, mapped string, depth int, branch map[string]bool, need func(string) bool, own *git.IgnoreSet) error
	var follow func(path, phys string, depth int, branch map[string]bool) error

	walk = func(phys, mapped string, depth int, branch map[string]bool, need func(string) bool, own *git.IgnoreSet) error {
		err := filepath.WalkDir(phys, func(p string, d fs.DirEntry, err error) error {
			mp := mapped + p[len(phys):]
			if err != nil {
				// An unreadable directory is skipped and named, not fatal.
				if d != nil && d.IsDir() && p != phys {
					cfg.cost.unreadable(mp)
					return fs.SkipDir
				}
				return nil
			}
			rel := relOf(mp)
			if d.IsDir() {
				if p == phys {
					return nil
				}
				name := d.Name()
				if name == ".git" {
					return fs.SkipDir
				}
				if IsSkipDir(name) && !inc.Covers(rel) && !inc.OnWay(rel) {
					cfg.cost.skippedDir(rel)
					return fs.SkipDir
				}
				// Under a link, the target's own path is ignored too: a link
				// into a tree must not bring back what .gitignore drops there.
				if ign.DirIgnored(root, mp) || own.DirIgnored(phys, p) || (depth > 0 && ign.DirIgnored(root, keyOf(p))) {
					return fs.SkipDir
				}
				if depth == 0 && need == nil && cfg.cost != nil {
					cfg.cost.dir(mp)
				}
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				if ign.KnownIgnored(root, mp) || own.KnownIgnored(phys, p) {
					return nil
				}
				if depth == 0 {
					links = append(links, link{mp, p})
					return nil
				}
				// Inside a followed link: follow now, with every real
				// directory between the target and this link open.
				return follow(mp, p, depth+1, openTo(branch, phys, filepath.Dir(p)))
			}
			if need != nil && !need(rel) {
				return nil
			}
			if own.KnownIgnored(phys, p) {
				return nil
			}
			key := mp
			if depth > 0 {
				key = keyOf(p) // p is real: phys is, and WalkDir follows no link
				if ign.KnownIgnored(root, key) {
					return nil
				}
			}
			return visit(mp, rel, key, d.Info)
		})
		if stopped {
			return fs.SkipAll
		}
		return err
	}

	// follow follows one symlink at path (as indexed); phys is where it is
	// on disk. depth counts it.
	follow = func(path, phys string, depth int, branch map[string]bool) error {
		rel := relOf(path)
		if depth > maxDepth {
			cfg.cost.LeaveOut(rel, LeftLinkDepth)
			return nil
		}
		real, err := filepath.EvalSymlinks(phys)
		if err != nil {
			cfg.cost.LeaveOut(rel, LeftLinkBroken)
			return nil
		}
		trel, inside := within(realRoot, real)
		if !inside && !inc.Covers(rel) {
			cfg.cost.LeaveOut(rel, LeftLinkLeaves)
			return nil
		}
		fi, err := os.Stat(real)
		if err != nil {
			cfg.cost.LeaveOut(rel, LeftLinkBroken)
			return nil
		}
		// A target .gitignore drops is not brought back through a link.
		if inside && (ign.KnownIgnored(root, keyOf(real)) || (fi.IsDir() && ign.DirIgnored(root, keyOf(real)))) {
			cfg.cost.LeaveOut(rel, LeftIgnored)
			return nil
		}
		if !fi.IsDir() {
			key := keyOf(real)
			if indexed[key] {
				cfg.cost.LeaveOut(rel, LeftLinkIndexed)
				return nil
			}
			return visit(path, rel, key, func() (fs.FileInfo, error) { return fi, nil })
		}
		if branch[real] {
			cfg.cost.LeaveOut(rel, LeftLinkLoop)
			return nil
		}
		if inside && ownPath(filepath.ToSlash(trel)) {
			cfg.cost.LeaveOut(rel, LeftLinkIndexed)
			return nil
		}
		open := make(map[string]bool, len(branch)+1)
		for k := range branch {
			open[k] = true
		}
		open[real] = true
		dupes0, files0 := dupes, read+counted
		err = walk(real, path, depth, open, nil, nil)
		// Everything it led to was indexed already (another link to the
		// same place came first): name it rather than drop it unseen.
		if dupes > dupes0 && read+counted == files0 {
			cfg.cost.LeaveOut(rel, LeftLinkIndexed)
		}
		return err
	}

	if cfg.manifest != nil {
		// git already knows the file set: iterate it, walk nothing.
		for _, rel := range cfg.manifest.Files {
			path := filepath.Join(root, filepath.FromSlash(rel))
			if visit(path, rel, path, func() (fs.FileInfo, error) { return os.Lstat(path) }) == fs.SkipAll {
				break
			}
		}
		// A nested repository an include pattern names or leads to is
		// walked — git lists it as one entry — under its own ignores, and
		// only for what the patterns cover.
		for _, n := range cfg.manifest.Nested {
			if stopped || !(inc.Covers(n.Dir) || inc.OnWay(n.Dir)) {
				continue
			}
			dir := filepath.Join(root, filepath.FromSlash(n.Dir))
			_ = walk(dir, dir, 0, nil, inc.Covers, git.LoadIgnores(dir))
		}
		for _, l := range cfg.manifest.Links {
			p := filepath.Join(root, filepath.FromSlash(l))
			links = append(links, link{p, p})
		}
	} else {
		// From the real root, mapped to root: WalkDir does not follow a
		// root that is itself a link, which would come back as a link to
		// the root, a loop.
		_ = walk(realRoot, root, 0, nil, nil, nil)
	}
	for _, l := range links {
		if stopped {
			break
		}
		// git lists a link under an excluded or skipped dir like any file.
		rel := relOf(l.path)
		if ign.KnownIgnored(root, l.path) {
			continue
		}
		if skip := skipDirOf(rel); skip != "" && !inc.Covers(rel) {
			cfg.cost.skippedDir(skip)
			continue
		}
		// A link's own directories are open: a link to one of them is a
		// loop. Real, since no link was followed to reach it.
		open := map[string]bool{realRoot: true}
		for d := filepath.Dir(relOf(l.path)); d != "." && d != "/"; d = filepath.Dir(d) {
			open[filepath.Join(realRoot, d)] = true
		}
		if follow(l.path, l.phys, 1, open) == fs.SkipAll {
			break
		}
	}
	if over {
		cfg.cost.end()
		return nil, &BudgetError{Budget: cfg.budget, Read: read, Report: cfg.cost.Report(), Stopped: stopped}
	}
	// Convention plugins (F2) run after the lexical walk, over the files
	// it indexed. Zero plugins → this loop is a no-op → the index is
	// byte-for-byte what it is without plugins.
	for _, p := range cfg.plugins {
		if p.Extract == nil {
			continue
		}
		for _, e := range p.Extract(pluginFiles) {
			idx.InsertPlugin(e)
		}
	}
	return idx, nil
}

// within reports whether p is root or under it, and its path relative to
// root. Both are real (symlink-free) absolute paths.
func within(root, p string) (string, bool) {
	if p == root {
		return ".", true
	}
	if r, ok := strings.CutPrefix(p, root+string(filepath.Separator)); ok {
		return r, true
	}
	return "", false
}

// openTo returns branch plus every directory from top down to dir, which
// lies under top.
func openTo(branch map[string]bool, top, dir string) map[string]bool {
	out := make(map[string]bool, len(branch)+4)
	for k := range branch {
		out[k] = true
	}
	for d := dir; ; d = filepath.Dir(d) {
		out[d] = true
		if d == top || filepath.Dir(d) == d {
			break
		}
	}
	return out
}

// skipDirOf is the outermost skipped directory a root-relative,
// slash-separated path runs through; "" when none. git may track a vendor/
// or dist/ tree; the index leaves it out the same as a walk would.
func skipDirOf(rel string) string {
	off := 0
	for {
		i := strings.IndexByte(rel[off:], '/')
		if i < 0 {
			return ""
		}
		start := strings.LastIndexByte(rel[:off+i], '/') + 1
		if IsSkipDir(rel[start : off+i]) {
			return rel[:off+i]
		}
		off += i + 1
	}
}
