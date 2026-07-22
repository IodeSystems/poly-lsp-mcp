package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// entry is the internal tree the structure tool builds before it
// serializes to one of three JSON shapes. The KEY of the serialized
// object is both the discriminator and the address:
//
//   - directory: {"dir": "<rel path>", "/": [ …children… ]}
//   - file:      {"file": "<rel path>", "lang": "go", "#": [ …symbols… ]}
//   - symbol:    {"sym": "Server.Start", "class": "method", "@": [22, 35]}
//
// A symbol's full address is <file>.file + "#" + <sym>.sym, e.g.
// "src/app.go#Server.Start".
type entry struct {
	kind     string  // "dir" | "file" | "sym"
	name     string  // dir/file: rel path; sym: dotted sym path
	lang     string  // file only
	class    string  // sym only
	at       [2]int  // sym only: [startLine, endLine]
	expanded bool    // file only: emit "#" (symbols were resolved)
	children []entry // dir: "/" (dirs+files); file: "#" (symbols)
}

func (e entry) toMap() map[string]any {
	switch e.kind {
	case "dir":
		kids := make([]any, len(e.children))
		for i, c := range e.children {
			kids[i] = c.toMap()
		}
		return map[string]any{"dir": e.name, "/": kids}
	case "file":
		m := map[string]any{"file": e.name}
		if e.lang != "" {
			m["lang"] = e.lang
		}
		if e.expanded {
			syms := make([]any, len(e.children))
			for i, c := range e.children {
				syms[i] = c.toMap()
			}
			m["#"] = syms
		}
		return m
	case "sym":
		return map[string]any{
			"sym":   e.name,
			"class": e.class,
			"@":     []int{e.at[0], e.at[1]},
		}
	}
	return map[string]any{}
}

// -------------------------------------------------------------- structure

func handleStructure(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var p struct {
		Path      string `json:"path"`
		Depth     *int   `json:"depth"`
		Grep      string `json:"grep"`
		NodeLimit *int   `json:"nodeLimit"`
	}
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, true, fmt.Errorf("bad arguments: %w", err)
		}
	}
	var grepRe *regexp.Regexp
	if p.Grep != "" {
		re, err := regexp.Compile(p.Grep)
		if err != nil {
			return nil, true, fmt.Errorf("invalid grep regex: %w", err)
		}
		grepRe = re
	}
	// Default depth: 1 for a plain listing; 32 when grep is set (the
	// agent wants a search, not a level view). depth counts directory
	// levels at dir level and max dot-count at file level (depth 1 =
	// top-level symbols only, no dots; depth 2 = one nesting level).
	depth := 1
	if grepRe != nil {
		depth = 32
	}
	if p.Depth != nil {
		depth = *p.Depth
		if depth < 0 {
			return nil, true, fmt.Errorf("depth must be >= 0")
		}
	}
	if p.Path == "" {
		p.Path = "."
	}
	autoNodeCap := p.NodeLimit == nil
	nodeLimit := defaultStructureNodeLimit
	if p.NodeLimit != nil {
		if *p.NodeLimit < 1 {
			return nil, true, fmt.Errorf("nodeLimit must be >= 1")
		}
		nodeLimit = *p.NodeLimit
	}

	abs, err := s.resolveFileArg(p.Path)
	if err != nil {
		return nil, true, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, true, fmt.Errorf("stat %s: %w", p.Path, err)
	}

	var root entry
	if info.IsDir() {
		if grepRe != nil {
			root = s.buildDirExpanded(abs, s.getRoot(), depth)
		} else {
			root = s.buildDir(abs, s.getRoot(), depth)
		}
	} else {
		root = s.buildFile(abs, s.languageForFile(abs), s.getRoot(), depth-1)
	}

	if grepRe != nil {
		pruned, ok := pruneEntry(root, grepRe)
		if !ok {
			return jsonContent(emptyEntry(root).toMap()), false, nil
		}
		root = pruned
	}

	total := countEntries(root)
	if total <= nodeLimit {
		return jsonContent(root.toMap()), false, nil
	}
	clipped := clipEntry(root, nodeLimit)
	m := clipped.toMap()
	m["truncated"] = true
	m["truncatedReason"] = chooseStructureReason(autoNodeCap)
	m["totalNodes"] = total
	m["shownNodes"] = countEntries(clipped)
	m["nodeLimit"] = nodeLimit
	m["hint"] = structureHint(autoNodeCap, nodeLimit, total, grepRe != nil)
	return jsonContent(m), false, nil
}

func chooseStructureReason(autoCap bool) string {
	if autoCap {
		return "auto"
	}
	return "nodeLimit"
}

func structureHint(autoCap bool, limit, total int, grep bool) string {
	if autoCap {
		if grep {
			return fmt.Sprintf("Auto-capped at %d/%d nodes. Narrow the regex or pass a larger nodeLimit.", limit, total)
		}
		return fmt.Sprintf("Auto-capped at %d/%d nodes. Use a deeper path, set grep to filter, or pass a larger nodeLimit.", limit, total)
	}
	return fmt.Sprintf("Trimmed to %d/%d nodes (your nodeLimit). Widen nodeLimit or narrow the path to see more.", limit, total)
}

// buildFile resolves a file's symbols into the file shape. maxDots caps
// symbol nesting by dot-count (maxDots = depth-1); negative means no
// cap. Files without a tree-sitter grammar get a single whole-file
// "text" symbol (sym "") so agents can address the whole file via
// "<file>#".
func (s *Server) buildFile(abs, lang, root string, maxDots int) entry {
	e := entry{kind: "file", name: relPath(abs, root), lang: lang, expanded: true}
	content, err := os.ReadFile(abs)
	if err != nil {
		return e
	}
	syms, err := symbols.FileSymbols(lang, content)
	if err != nil {
		endLine, _ := contentEndPosition(content)
		if endLine < 1 {
			endLine = 1
		}
		e.children = []entry{{kind: "sym", name: "", class: "text", at: [2]int{1, endLine}}}
		return e
	}
	for _, sym := range syms {
		if maxDots >= 0 && strings.Count(sym.Sym, ".") > maxDots {
			continue
		}
		e.children = append(e.children, entry{
			kind:  "sym",
			name:  sym.Sym,
			class: sym.Class,
			at:    [2]int{sym.DeclStartLine, sym.DeclEndLine},
		})
	}
	return e
}

// buildDir lists a directory's dirs + files (files NOT expanded to
// symbols — a plain level view). depth caps directory descent.
func (s *Server) buildDir(abs, root string, depth int) entry {
	e := entry{kind: "dir", name: relPath(abs, root)}
	if depth <= 0 {
		return e
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		return e
	}
	for _, de := range des {
		if skipScanDir(de.Name()) {
			continue
		}
		childAbs := filepath.Join(abs, de.Name())
		if de.IsDir() {
			e.children = append(e.children, s.buildDir(childAbs, root, depth-1))
		} else {
			e.children = append(e.children, entry{
				kind: "file",
				name: relPath(childAbs, root),
				lang: s.languageForFile(childAbs),
			})
		}
	}
	sortEntries(e.children)
	return e
}

// buildDirExpanded is the grep-mode variant: every file is expanded to
// its full symbol list so a regex over `sym` can hit nested symbols.
func (s *Server) buildDirExpanded(abs, root string, depth int) entry {
	e := entry{kind: "dir", name: relPath(abs, root)}
	if depth <= 0 {
		return e
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		return e
	}
	for _, de := range des {
		if skipScanDir(de.Name()) {
			continue
		}
		childAbs := filepath.Join(abs, de.Name())
		if de.IsDir() {
			e.children = append(e.children, s.buildDirExpanded(childAbs, root, depth-1))
		} else {
			e.children = append(e.children, s.buildFile(childAbs, s.languageForFile(childAbs), root, 1<<30))
		}
	}
	sortEntries(e.children)
	return e
}

func sortEntries(es []entry) {
	sort.Slice(es, func(i, j int) bool { return es[i].name < es[j].name })
}

// grepTarget is the string grep matches an entry against: the dotted
// sym for symbols, the basename for dirs/files.
func grepTarget(e entry) string {
	if e.kind == "sym" {
		return e.name
	}
	return filepath.Base(e.name)
}

// pruneEntry keeps entries whose own grep target matches, OR that have a
// surviving descendant. A matching file/symbol keeps all its children
// (so the agent can drill in without a second call); a matching
// directory keeps only its matching children.
func pruneEntry(e entry, re *regexp.Regexp) (entry, bool) {
	self := re.MatchString(grepTarget(e))
	var kept []entry
	for _, c := range e.children {
		if k, ok := pruneEntry(c, re); ok {
			kept = append(kept, k)
		}
	}
	if self {
		switch e.kind {
		case "file", "sym":
			return e, true
		case "dir":
			out := e
			out.children = kept
			return out, true
		}
	}
	if len(kept) > 0 {
		out := e
		out.children = kept
		return out, true
	}
	return entry{}, false
}

func emptyEntry(e entry) entry {
	out := e
	out.children = nil
	if out.kind == "file" {
		out.expanded = false
	}
	return out
}

func countEntries(e entry) int {
	n := 1
	for _, c := range e.children {
		n += countEntries(c)
	}
	return n
}

func clipEntry(e entry, limit int) entry {
	budget := limit
	out, _ := clipWithBudget(e, &budget)
	return out
}

func clipWithBudget(e entry, budget *int) (entry, bool) {
	if *budget <= 0 {
		return entry{}, false
	}
	*budget--
	out := e
	out.children = nil
	for _, c := range e.children {
		if *budget <= 0 {
			break
		}
		sub, ok := clipWithBudget(c, budget)
		if !ok {
			break
		}
		out.children = append(out.children, sub)
	}
	return out, true
}

// -------------------------------------------------------------- grouped matches

// matchItem is one hit destined for the grouped-by-file output shared by
// search and node_references.
type matchItem struct {
	file  string
	lang  string
	sym   string
	class string
	at    [2]int
}

// groupedMatches groups items by file into the file shape reused
// throughout the API: {"matches": [ {"file":…, "lang":…, "#":[ {sym,class,@}… ]}, … ]}.
// File order follows first appearance.
func groupedMatches(items []matchItem) map[string]any {
	var order []string
	byFile := map[string]*entry{}
	for _, it := range items {
		fe, ok := byFile[it.file]
		if !ok {
			fe = &entry{kind: "file", name: it.file, lang: it.lang, expanded: true}
			byFile[it.file] = fe
			order = append(order, it.file)
		}
		fe.children = append(fe.children, entry{kind: "sym", name: it.sym, class: it.class, at: it.at})
	}
	matches := make([]any, 0, len(order))
	for _, f := range order {
		matches = append(matches, byFile[f].toMap())
	}
	return map[string]any{"matches": matches}
}

// enclosingSymPath returns the dotted sym path of the deepest symbol
// whose declaration contains `line` in absFile, or "" if none. Parses
// via a per-call cache so a search over many hits in one file parses it
// once.
func (s *Server) enclosingSymPath(absFile string, line int, cache map[string][]symbols.Symbol) string {
	syms, ok := cache[absFile]
	if !ok {
		content, err := os.ReadFile(absFile)
		if err == nil {
			syms, _ = symbols.FileSymbols(s.languageForFile(absFile), content)
		}
		cache[absFile] = syms
	}
	best := ""
	bestSpan := 1 << 30
	bestDots := -1
	for _, sym := range syms {
		// `argument` and `return` are leaf DECLARATIONS, not enclosing
		// scopes: both sit on their callable's signature line with a
		// zero-line span, so without this they would out-compete the
		// callable itself for every hit on that line (a call or type in
		// `return B()` / the result type), stealing the site's
		// attribution and the ref's enclosing symbol. "Which symbol
		// encloses this line" should answer with the callable.
		if sym.Class == "argument" || sym.Class == "return" {
			continue
		}
		if sym.DeclStartLine <= line && line <= sym.DeclEndLine {
			span := sym.DeclEndLine - sym.DeclStartLine
			dots := strings.Count(sym.Sym, ".")
			if span < bestSpan || (span == bestSpan && dots > bestDots) {
				best, bestSpan, bestDots = sym.Sym, span, dots
			}
		}
	}
	return best
}

// -------------------------------------------------------------- node addressing

// resolvedNode carries the two ranges a "<file>#<sym>" address resolves
// to: declRange (the whole declaration — node_read/edit/delete) and
// nameRange (the identifier — node_references/refactor).
type resolvedNode struct {
	declRange rangeArgs
	nameRange rangeArgs
	sym       string // canonical dotted sym path resolved by matchSym; "" = whole file
}

// resolveNodeAddr resolves a "<file>#<sym>" node address into ranges.
// Split on the FIRST "#" → file path + dotted sym path. An empty sym
// path (or a bare file with no "#") addresses the whole file. Failure
// to resolve the sym returns a guided error naming the nearest symbols;
// the caller must NOT write.
func (s *Server) resolveNodeAddr(node string) (*resolvedNode, error) {
	file := node
	symPath := ""
	if h := strings.IndexByte(node, '#'); h >= 0 {
		file, symPath = node[:h], node[h+1:]
	}
	if file == "" {
		return nil, fmt.Errorf("node address needs a file: \"<file>#<sym>\"")
	}
	abs, err := s.resolveFileArg(file)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	if symPath == "" {
		endLine, endCol := contentEndPosition(content)
		r := rangeArgs{File: file, StartLine: 1, StartCol: 1, EndLine: endLine, EndCol: endCol}
		return &resolvedNode{declRange: r, nameRange: r, sym: ""}, nil
	}
	lang := s.languageForFile(abs)
	syms, err := symbols.FileSymbols(lang, content)
	if err != nil {
		return nil, fmt.Errorf("no symbol tree for %s (language %q); address it with a line/col range instead", file, lang)
	}
	for _, sym := range syms {
		if matchSym(symPath, sym.Sym) {
			return &resolvedNode{
				declRange: rangeArgs{File: file, StartLine: sym.DeclStartLine, StartCol: sym.DeclStartCol, EndLine: sym.DeclEndLine, EndCol: sym.DeclEndCol},
				nameRange: rangeArgs{File: file, StartLine: sym.NameStartLine, StartCol: sym.NameStartCol, EndLine: sym.NameEndLine, EndCol: sym.NameEndCol},
				sym:       sym.Sym,
			}, nil
		}
	}
	return nil, fmt.Errorf("no symbol %q in %s; did you mean: %s", symPath, file, nearestSyms(symPath, syms))
}

// matchSym reports whether a query sym path addresses a candidate sym.
// Comparison is segment-by-segment; each segment's "[n]" index is
// normalized so a bare name and its "[1]" form both address the first
// (bare `init` == `init[1]`).
func matchSym(query, cand string) bool {
	qs := strings.Split(query, ".")
	cs := strings.Split(cand, ".")
	if len(qs) != len(cs) {
		return false
	}
	for i := range qs {
		qn, qi := parseSeg(qs[i])
		cn, ci := parseSeg(cs[i])
		if qn != cn || norm1(qi) != norm1(ci) {
			return false
		}
	}
	return true
}

func norm1(i int) int {
	if i == 0 {
		return 1
	}
	return i
}

// parseSeg splits "name[3]" into ("name", 3). A bare segment yields
// index 0 (meaning "the only one / the first").
func parseSeg(s string) (name string, idx int) {
	if strings.HasSuffix(s, "]") {
		if open := strings.LastIndexByte(s, '['); open >= 0 {
			if n, err := strconv.Atoi(s[open+1 : len(s)-1]); err == nil {
				return s[:open], n
			}
		}
	}
	return s, 0
}

// nearestSyms renders a candidate list for a resolution-failure error,
// preferring syms whose last segment matches the query's last segment.
func nearestSyms(query string, syms []symbols.Symbol) string {
	qLast := lastSeg(query)
	var near, rest []string
	for _, s := range syms {
		if lastSeg(s.Sym) == qLast {
			near = append(near, s.Sym)
		} else {
			rest = append(rest, s.Sym)
		}
	}
	cands := append(near, rest...)
	if len(cands) > 20 {
		cands = cands[:20]
	}
	if len(cands) == 0 {
		return "(file has no symbols)"
	}
	return strings.Join(cands, ", ")
}

func lastSeg(sym string) string {
	if i := strings.LastIndex(sym, "."); i >= 0 {
		sym = sym[i+1:]
	}
	n, _ := parseSeg(sym)
	return n
}
