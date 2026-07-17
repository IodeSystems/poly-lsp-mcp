package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// This file is the MODERN selector engine: a real-CSS-shaped grammar
// (.class / #id / pseudo-classes / combinators) evaluated over ONE
// unified node tree that crosses the dir → file → symbol → argument
// boundary. The LEGACY engine in node_query.go (bare `type[name=x]`
// grammar, per-file symbol lists only) is untouched and still backs the
// legacy 9-tool surface.
//
// The unified tree:
//
//	.project#<basename-of-root>     <- matched by :root
//	  .dir#<relpath>                (nested arbitrarily)
//	    .file#<relpath>
//	      <symbol classes, dotted-nested>
//	        .argument#<param-name>
//
// :root is a PSEUDO-CLASS, not a type selector — exactly as in CSS,
// where :root matches the document's root ELEMENT (html). Here the
// workspace's root element is the single synthesized .project node, so
// `:root` and `.project` select the same one node. That is what makes
// the canonical `:root > *` mean "top-level dirs + files".
//
// Exactly ONE .project node is always synthesized, even though this
// server only ever has one configured root (s.root is a single string).
// The level exists so the tree SHAPE is uniform regardless of how many
// roots exist today or later; no multi-root config is plumbed.

// ----------------------------------------------------------- node tree

// treeNode is one node in the unified tree. Every node carries two
// matchable ids so `#id` can name a node the way a caller naturally
// would (see nodeIDs):
//
//   - leaf — a symbol's last dotted segment, or a dir/file's basename.
//   - full — a symbol's dotted path, or a dir/file's workspace-relative
//     path.
type treeNode struct {
	class string // "project"|"dir"|"file"|<symbol class>|"argument"
	leaf  string
	full  string

	file string // workspace-relative file path ("" for project/dir)
	sym  string // dotted sym path ("" for project/dir/file)
	at   [2]int // [startLine, endLine]; zero for project/dir (no span)

	parent   *treeNode
	children []*treeNode

	// depth from the project node (project itself = 0).
	depth int

	// loaded guards lazy symbol parsing for file nodes: a file's
	// symbol children are only parsed when something actually walks
	// into the file (see engine.kids). This is what keeps the
	// canonical `:root > *` tour from parsing the whole workspace.
	loaded bool
	abs    string // absolute path, for lazy loading + source reads
}

// addr renders the node's address — the exact string node_read /
// node_edit accept. Dirs get their relpath (addressing one is a clear
// error at read/edit time, not here).
func (n *treeNode) addr() string {
	switch n.class {
	case "project":
		return n.full
	case "dir", "file":
		return n.file
	}
	if n.sym == "" {
		return n.file
	}
	return n.file + "#" + n.sym
}

// nodeIDs returns every string `#id` may match for this node. A symbol
// answers to its leaf name, its dotted path, AND its full
// "<file>#<sym>" address (so `:references(#'app.go#Start')` works); a
// dir/file answers to its basename AND its workspace-relative path
// (both are legitimate ways to name it — basename matching can be
// ambiguous across dirs, which is fine: that ambiguity is resolved as
// an error at addressing time, not silently).
func (n *treeNode) nodeIDs() []string {
	switch n.class {
	case "project":
		return []string{n.full}
	case "dir", "file":
		return []string{n.leaf, n.full}
	}
	return []string{n.leaf, n.full, n.file + "#" + n.sym}
}

// engine holds one query's tree and the server it reads through.
type engine struct {
	s       *Server
	project *treeNode

	// refCache memoizes :references(...) site sets for the life of one
	// query, keyed by the inner selector's AST identity — the predicate
	// is otherwise re-evaluated once per candidate node.
	refCache map[string]map[string]bool
}

// kids returns n's children, parsing a file's symbols on first use.
func (e *engine) kids(n *treeNode) []*treeNode {
	if n.class == "file" && !n.loaded {
		n.loaded = true
		e.loadFileSymbols(n)
	}
	return n.children
}

// loadFileSymbols parses one file and hangs its symbol tree off the
// file node. Symbols arrive FLAT with dotted paths; nesting is rebuilt
// by attaching each symbol to the deepest already-created ancestor
// prefix (FileSymbols emits owners before their members, so the parent
// always exists by the time a child is attached).
func (e *engine) loadFileSymbols(f *treeNode) {
	lang := e.s.languageForFile(f.abs)
	if lang == "" {
		return
	}
	content, err := os.ReadFile(f.abs)
	if err != nil {
		return
	}
	syms, err := symbols.FileSymbols(lang, content)
	if err != nil || len(syms) == 0 {
		return
	}
	bySym := make(map[string]*treeNode, len(syms))
	for _, sym := range syms {
		parent := f
		if i := strings.LastIndex(sym.Sym, "."); i >= 0 {
			if p, ok := bySym[sym.Sym[:i]]; ok {
				parent = p
			}
		}
		n := &treeNode{
			class:  sym.Class,
			leaf:   lastSeg(sym.Sym),
			full:   sym.Sym,
			file:   f.file,
			sym:    sym.Sym,
			at:     [2]int{sym.DeclStartLine, sym.DeclEndLine},
			parent: parent,
			depth:  parent.depth + 1,
			abs:    f.abs,
		}
		bySym[sym.Sym] = n
		parent.children = append(parent.children, n)
	}
}

// buildTree synthesizes the project node and walks the workspace into
// dir/file nodes. File symbols are NOT parsed here (see kids).
func (s *Server) buildTree() (*engine, error) {
	root := s.getRoot()
	if root == "" {
		return nil, fmt.Errorf("no workspace root configured")
	}
	project := &treeNode{
		class: "project",
		leaf:  filepath.Base(root),
		full:  filepath.Base(root),
		abs:   root,
	}
	e := &engine{s: s, project: project}
	e.walkDir(root, project)
	return e, nil
}

func (e *engine) walkDir(abs string, parent *treeNode) {
	des, err := os.ReadDir(abs)
	if err != nil {
		return
	}
	sort.Slice(des, func(i, j int) bool { return des[i].Name() < des[j].Name() })
	for _, de := range des {
		if skipScanDir(de.Name()) {
			continue
		}
		childAbs := filepath.Join(abs, de.Name())
		rel := relPath(childAbs, e.s.getRoot())
		if de.IsDir() {
			d := &treeNode{
				class: "dir", leaf: de.Name(), full: rel, file: rel,
				parent: parent, depth: parent.depth + 1, abs: childAbs,
			}
			parent.children = append(parent.children, d)
			e.walkDir(childAbs, d)
			continue
		}
		f := &treeNode{
			class: "file", leaf: de.Name(), full: rel, file: rel,
			parent: parent, depth: parent.depth + 1, abs: childAbs,
		}
		f.at = [2]int{1, countFileLines(childAbs)}
		parent.children = append(parent.children, f)
	}
}

func countFileLines(abs string) int {
	content, err := os.ReadFile(abs)
	if err != nil {
		return 0
	}
	return len(splitNodeReadLines(content))
}

// allNodes returns every node in the tree in pre-order (deterministic:
// dirs/files sorted by name, symbols in source order), pruned at
// maxDepth when it is >= 0. Pruning is what lets a depth-bounded
// selector avoid parsing file symbols at all.
func (e *engine) allNodes(maxDepth int) []*treeNode {
	var out []*treeNode
	var walk func(n *treeNode)
	walk = func(n *treeNode) {
		out = append(out, n)
		if maxDepth >= 0 && n.depth >= maxDepth {
			return
		}
		for _, c := range e.kids(n) {
			walk(c)
		}
	}
	walk(e.project)
	return out
}

// nodeSource returns the node's OWN source text and the 1-based file
// line its first line sits on. Project/dir nodes have no source text.
func (e *engine) nodeSource(n *treeNode) (lines []string, startLine int, ok bool) {
	switch n.class {
	case "project", "dir":
		return nil, 0, false
	}
	content, err := os.ReadFile(n.abs)
	if err != nil {
		return nil, 0, false
	}
	all := splitNodeReadLines(content)
	if n.class == "file" {
		return all, 1, true
	}
	s, en := n.at[0], n.at[1]
	if s < 1 || s > len(all) {
		return nil, 0, false
	}
	if en > len(all) {
		en = len(all)
	}
	return all[s-1 : en], s, true
}

// ----------------------------------------------------------- selector AST

type selOp int

const (
	selExact    selOp = iota // =
	selPrefix                // ^=
	selSuffix                // $=
	selContains              // *=
)

type selAttr struct {
	op    selOp
	value string
}

type selCompound struct {
	anyType bool   // `*`, or implied universal (only ids/attrs/pseudos)
	class   string // type selector (.func → "func"), if !anyType
	root    bool   // :root — matches the single .project node

	attrs      []selAttr
	has        []selectorList
	hasParent  []selectorList
	references []selectorList
	contains   []*grepSpec

	// depth is an optional :depth(m,n) override. It REPLACES the
	// default range of the relation to this compound's left — the
	// preceding combinator, or (for the leftmost compound) the range
	// the whole complex is anchored by (:root at top level; the outer
	// subject inside :has/:has_parent).
	depth *[2]int
}

type selComb int

const (
	selDescendant selComb = iota // whitespace → [1,∞]
	selChild                     // '>'        → [1,1]
)

// selComplex is a chain of compounds joined by combinators; the LAST
// compound is the subject.
type selComplex struct {
	compounds []selCompound
	combs     []selComb
}

type selectorList []selComplex

// ----------------------------------------------------------- parser

func parseModernSelector(input string) (selectorList, error) {
	p := &modSelParser{s: []rune(input)}
	list, err := p.parseList()
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if !p.eof() {
		return nil, p.errf("a combinator, ',' or end of selector")
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("empty selector")
	}
	return list, nil
}

type modSelParser struct {
	s []rune
	i int
}

func (p *modSelParser) eof() bool { return p.i >= len(p.s) }

func (p *modSelParser) peek() rune {
	if p.eof() {
		return 0
	}
	return p.s[p.i]
}

func (p *modSelParser) skipWS() bool {
	consumed := false
	for !p.eof() {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
			consumed = true
		default:
			return consumed
		}
	}
	return consumed
}

// errf renders a guided parse error: what was expected, where, plus the
// full grammar dump (the deep grammar is paid for only when a selector
// actually fails to parse — never on the every-turn tool description).
func (p *modSelParser) errf(expected string) error {
	rest := string(p.s[p.i:])
	if len(rest) > 24 {
		rest = rest[:24] + "…"
	}
	if rest == "" {
		rest = "end of input"
	} else {
		rest = "'" + rest + "'"
	}
	return fmt.Errorf("bad selector near %s: expected %s\n\n%s", rest, expected, selectorGrammarHelp)
}

func (p *modSelParser) parseList() (selectorList, error) {
	var list selectorList
	for {
		p.skipWS()
		cx, err := p.parseComplex()
		if err != nil {
			return nil, err
		}
		list = append(list, cx)
		p.skipWS()
		if p.peek() == ',' {
			p.i++
			continue
		}
		return list, nil
	}
}

func (p *modSelParser) parseComplex() (selComplex, error) {
	var cx selComplex
	first, err := p.parseCompound()
	if err != nil {
		return cx, err
	}
	cx.compounds = append(cx.compounds, first)
	for {
		sawWS := p.skipWS()
		c := p.peek()
		if p.eof() || c == ',' || c == ')' {
			return cx, nil
		}
		comb := selDescendant
		if c == '>' {
			p.i++
			p.skipWS()
			comb = selChild
		} else if !sawWS {
			return cx, p.errf("a combinator, ',' or end of selector")
		}
		next, err := p.parseCompound()
		if err != nil {
			return cx, err
		}
		cx.combs = append(cx.combs, comb)
		cx.compounds = append(cx.compounds, next)
	}
}

func (p *modSelParser) parseCompound() (selCompound, error) {
	var comp selCompound
	sawType := false
	switch {
	case p.peek() == '*':
		p.i++
		comp.anyType = true
		sawType = true
	case p.peek() == '.':
		// Tags are canonical (see below), but `.func` is ACCEPTED for a known
		// type. Measured: even with the description stating tags twice, the
		// model kept writing `.file` — a CSS prior beats a schema line, every
		// time. Rejecting it bought nothing and cost ~4 turns of round-trips
		// per task fixing a spelling that was never ambiguous.
		//
		// The invention guard is unaffected, because it was never about the
		// dot: `.cache` fails on `cache` not being a TYPE, and still says
		// "try #cache". So we accept what is unambiguous and reject only what
		// is actually wrong.
		p.i++
		name := p.readIdent()
		if !knownSelectorClass(name) {
			return comp, dotIsNotAClassErr(name)
		}
		comp.class = name
		sawType = true
	case isSelIdentStart(p.peek()):
		// A bare word is the TAG — the node's intrinsic type, as `div` is an
		// element's type. Closed set: a model that knows CSS knows it cannot
		// invent one, which is the whole point of the move.
		comp.class = p.readIdent()
		if !knownSelectorClass(comp.class) {
			return comp, unknownTypeErr(comp.class)
		}
		sawType = true
	default:
		comp.anyType = true // implied universal; needs an id/attr/pseudo
	}

	for {
		switch p.peek() {
		case '#':
			p.i++
			id, err := p.readID()
			if err != nil {
				return comp, err
			}
			// `#id` is exactly sugar for `[name=id]`.
			comp.attrs = append(comp.attrs, selAttr{op: selExact, value: id})
		case '[':
			a, err := p.parseAttr()
			if err != nil {
				return comp, err
			}
			comp.attrs = append(comp.attrs, a)
		case ':':
			if err := p.parsePseudo(&comp); err != nil {
				return comp, err
			}
		default:
			if !sawType && !comp.root && len(comp.attrs) == 0 && len(comp.has) == 0 &&
				len(comp.hasParent) == 0 && len(comp.references) == 0 &&
				len(comp.contains) == 0 && comp.depth == nil {
				return comp, p.errf("a class selector ('.func'), '*', '#id', '[name…]' or a pseudo-class")
			}
			return comp, nil
		}
	}
}

// readID reads `#bare` or `#"quoted"`. Quote only when the id isn't a
// bare identifier — the CSS rule. There is deliberately NO backslash
// escaping: if it needs escaping, it needs quoting instead.
// readID reads a bare or quoted id. BOTH quote characters are accepted, as in
// real CSS — but ' is the one to document and use.
//
// A selector arrives inside a JSON string, so a double-quoted id needs escaping
// at the JSON layer too:
//
//	"selector": "file#\"store.go\" #Save"     ← two escaping layers
//	"selector": "file#'store.go' #Save"       ← one
//
// Quoting is not optional for the common case, either: every filename is
// tag.class to a CSS parser (`store.go` = tag `store`, class `go`), so ANY path
// must be quoted to survive. Making the required construct also the one that
// needs escaping was a self-inflicted wound — CSS allowed ' the whole time.
func (p *modSelParser) readID() (string, error) {
	if q := p.peek(); q == '"' || q == '\'' {
		p.i++
		start := p.i
		for !p.eof() && p.s[p.i] != q {
			p.i++
		}
		if p.eof() {
			return "", p.errf(fmt.Sprintf("a closing %c for the quoted id", q))
		}
		v := string(p.s[start:p.i])
		p.i++
		return v, nil
	}
	start := p.i
	for !p.eof() && isModIDPart(p.s[p.i]) {
		p.i++
	}
	if p.i == start {
		return "", p.errf("an id after '#' (bare, or quoted like #'a b')")
	}
	return string(p.s[start:p.i]), nil
}

func (p *modSelParser) parseAttr() (selAttr, error) {
	var a selAttr
	p.i++ // '['
	p.skipWS()
	name := p.readIdent()
	if name == "" {
		return a, p.errf("an attribute name")
	}
	if name != "name" {
		return a, fmt.Errorf("unknown attribute %q: only [name] is supported (ops: = ^= $= *=)\n\n%s", name, selectorGrammarHelp)
	}
	switch p.peek() {
	case '=':
		p.i++
		a.op = selExact
	case '^':
		p.i++
		if p.peek() != '=' {
			return a, p.errf("'=' (to complete '^=')")
		}
		p.i++
		a.op = selPrefix
	case '$':
		p.i++
		if p.peek() != '=' {
			return a, p.errf("'=' (to complete '$=')")
		}
		p.i++
		a.op = selSuffix
	case '*':
		p.i++
		if p.peek() != '=' {
			return a, p.errf("'=' (to complete '*=')")
		}
		p.i++
		a.op = selContains
	default:
		return a, p.errf("one of = ^= $= *=")
	}
	a.value = p.readAttrValue()
	if p.peek() != ']' {
		return a, p.errf("']'")
	}
	p.i++
	return a, nil
}

// parsePseudo parses one ":name" / ":name(...)" and folds it into comp.
func (p *modSelParser) parsePseudo(comp *selCompound) error {
	p.i++ // ':'
	name := p.readIdent()
	switch name {
	case "root":
		comp.root = true
		return nil
	case "has", "has_parent", "references":
		if p.peek() != '(' {
			return p.errf("'(' after :" + name)
		}
		p.i++
		inner, err := p.parseList()
		if err != nil {
			return err
		}
		p.skipWS()
		if p.peek() != ')' {
			return p.errf("')' to close :" + name)
		}
		p.i++
		switch name {
		case "has":
			comp.has = append(comp.has, inner)
		case "has_parent":
			comp.hasParent = append(comp.hasParent, inner)
		case "references":
			comp.references = append(comp.references, inner)
		}
		return nil
	case "contains":
		if p.peek() != '(' {
			return p.errf("'(' after :contains")
		}
		p.i++
		p.skipWS()
		// Both quotes, ' preferred: the selector rides inside a JSON string, so
		// :contains("x") costs an escaping layer that :contains('x') does not.
		q := p.peek()
		if q != '"' && q != '\'' {
			return p.errf("a quoted string, e.g. :contains('TODO')")
		}
		p.i++
		start := p.i
		for !p.eof() && p.s[p.i] != q {
			p.i++
		}
		if p.eof() {
			return p.errf(fmt.Sprintf("a closing %c for :contains", q))
		}
		text := string(p.s[start:p.i])
		p.i++
		p.skipWS()
		if p.peek() != ')' {
			return p.errf("')' to close :contains")
		}
		p.i++
		// Same grep-flag vocabulary, same matcher — :contains is just
		// the boolean any-match projection of it.
		g, err := parseContainsSpec(text)
		if err != nil {
			return fmt.Errorf("bad :contains(%q): %w", text, err)
		}
		comp.contains = append(comp.contains, g)
		return nil
	case "depth":
		if p.peek() != '(' {
			return p.errf("'(' after :depth")
		}
		p.i++
		p.skipWS()
		lo, ok := p.readNonNegInt()
		if !ok {
			return p.errf("a non-negative integer (min)")
		}
		p.skipWS()
		if p.peek() != ',' {
			return p.errf("',' in :depth(min,max)")
		}
		p.i++
		p.skipWS()
		hi, ok := p.readNonNegInt()
		if !ok {
			return p.errf("a non-negative integer (max)")
		}
		p.skipWS()
		if p.peek() != ')' {
			return p.errf("')' to close :depth")
		}
		p.i++
		if hi < lo {
			return fmt.Errorf("bad :depth(%d,%d): max must be >= min", lo, hi)
		}
		if comp.depth != nil {
			return fmt.Errorf("only one :depth(...) per compound")
		}
		comp.depth = &[2]int{lo, hi}
		return nil
	default:
		return fmt.Errorf("unknown pseudo-class %q\n\n%s", ":"+name, selectorGrammarHelp)
	}
}

func (p *modSelParser) readNonNegInt() (int, bool) {
	start := p.i
	for !p.eof() && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		p.i++
	}
	if p.i == start {
		return 0, false
	}
	n, err := strconv.Atoi(string(p.s[start:p.i]))
	return n, err == nil
}

func (p *modSelParser) readIdent() string {
	start := p.i
	for !p.eof() && isModIdentPart(p.s[p.i]) {
		p.i++
	}
	return string(p.s[start:p.i])
}

func (p *modSelParser) readAttrValue() string {
	if c := p.peek(); c == '"' || c == '\'' {
		quote := c
		p.i++
		start := p.i
		for !p.eof() && p.s[p.i] != quote {
			p.i++
		}
		v := string(p.s[start:p.i])
		if !p.eof() {
			p.i++
		}
		return v
	}
	start := p.i
	for !p.eof() && p.s[p.i] != ']' {
		p.i++
	}
	return strings.TrimSpace(string(p.s[start:p.i]))
}

func isModIdentPart(c rune) bool {
	return c == '_' || c == '-' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// isModIDPart matches the bare-id charset: [A-Za-z_][A-Za-z0-9_.-]*.
// Anything else (spaces, slashes, '#') must be quoted.
func isModIDPart(c rune) bool { return isModIdentPart(c) || c == '.' }

// selectorClasses is the class vocabulary a `.class` selector accepts:
// the EXISTING symbol classes this repo emits, plus the three
// node-model levels. Kept as the real strings (".func", not
// ".function") so a selector's class matches the `class` in output rows.
var selectorClasses = map[string]bool{
	"project": true, "dir": true, "file": true,
	"func": true, "method": true, "type": true, "struct": true,
	"interface": true, "class": true, "const": true, "var": true,
	"field": true, "enum": true, "ctor": true, "module": true,
	"import": true, "argument": true,
	// NB: no "text" — that class is the legacy structure tool's
	// whole-file fallback for grammar-less files, never emitted by
	// FileSymbols. In this tree such a file is simply a .file node with
	// no symbol children, so `.text` is genuinely an unknown class.
}

func knownSelectorClass(c string) bool { return selectorClasses[c] }

// isSelIdentStart reports whether c can begin a bare TAG name.
func isSelIdentStart(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

// selectorTypeList returns the fixed tag vocabulary, sorted, for errors.
func selectorTypeList() []string {
	out := make([]string, 0, len(selectorClasses))
	for c := range selectorClasses {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Both selector errors answer THE mistake instead of reprinting the language.
//
// Measured on a live run: ~12 bad selectors, each returning the full grammar —
// 1764 chars, ~441 tokens — which then rode along in the conversation and was
// re-billed on every following turn: ≈5.3k tokens, compounding. And the model
// still didn't learn, because a grammar dump never mentions `cache`; it just
// guessed the other sigil next time. Name the fix, get out. The full grammar
// stays for malformed SYNTAX (caller lost about shape) and on demand via "?".

// unknownTypeErr: a bare word that isn't one of our tags. Almost always a
// workspace NAME used where a type belongs — `cache` for the cache/ dir.
func unknownTypeErr(name string) error {
	return fmt.Errorf(
		"no node type %q. Types are a fixed set (like CSS element names): %s. Anything NAMED in the workspace — a dir, file or symbol — is an id: try #%s (e.g. \"#%s > file\"). Pass selector \"?\" for the grammar.",
		name, strings.Join(selectorTypeList(), " "), name, name)
}

// dotIsNotAClassErr: `.foo`. There are no classes — the node's kind is its
// tag. This exists because `.func` USED to be the spelling, so both a stale
// habit and a CSS-trained guess land here, and both want the same answer.
func dotIsNotAClassErr(name string) error {
	if name == "" {
		return fmt.Errorf("selectors have no classes: a node's type is a bare tag (%s), a name is an #id. Pass selector \"?\" for the grammar.",
			strings.Join(selectorTypeList(), " "))
	}
	if knownSelectorClass(name) {
		return fmt.Errorf("%q is a node TYPE, so write it bare: %q (types are tags, like CSS's div — there are no classes here).", "."+name, name)
	}
	return fmt.Errorf(
		"no classes in this grammar, and %q is not a node type. Types are tags, a fixed set: %s. A workspace name (dir/file/symbol) is an id: try #%s.",
		name, strings.Join(selectorTypeList(), " "), name)
}

const selectorGrammarHelp = `Selector grammar (real CSS over the unified node tree).

The ONE thing to get right: a node's TYPE is a bare tag, like CSS's div/span —
a fixed set you cannot invent. Anything the workspace NAMES — a dir, a file, a
symbol — is an #id. So the cache/ directory is #cache, never "cache". There are
no classes; "." is not used.

  TYPES  project dir file
         func method type struct interface class const var field enum ctor module import argument
         * = any type.
  TREE   project#<root-name> > dir#<relpath> > file#<relpath> > <symbols, dotted-nested> > argument#<param>
         :root is a pseudo-class matching the single project node (as CSS's :root matches <html>).
  ID     #bare  (charset [A-Za-z_][A-Za-z0-9_.-]*)  or  #'anything else'  (spaces, slashes, …). Quote instead of escaping; there is no backslash escape.
         A symbol answers to its leaf name, its dotted path, and its full "<file>#<sym>" address.
         A dir/file answers to its basename AND its workspace-relative path.
  ATTR   [name=X] exact  [name^=X] prefix  [name$=X] suffix  [name*=X] contains.  #id is sugar for [name=id].
  PSEUDO :root
         :has(<sel>)         a DESCENDANT at any depth matches <sel>
         :has_parent(<sel>)  an ANCESTOR at any depth matches <sel>
         :references(<sel>)  this node references a symbol matching <sel>  (reverse lookup: *:references(#'app.go#Start'))
         :contains('text')   this node's own source contains text (same flags as grep: -i -w -E -F -v)
         :depth(m,n)         m..n levels from the PREVIOUS target in the chain (0 = that target itself);
                             with no preceding target, measured from :root. Overrides the combinator's default range.
  COMB   space = descendant [1,∞]   '>' = direct child [1,1]
  COMMA  union: "func, method"
Examples: file = EVERY file, nested included  |  #cache > file = files directly in dir cache/  |  :root > * = ONLY top level
          func:has_parent(#'some_file.ts')  |  func[name^=Test]  |  func:has(argument#'say it')  |  *:references(#'app.go#Start')  |  struct method:depth(0,2)`

// ----------------------------------------------------------- depth

// inTreeDepth reports whether node n sits within [min,max] levels
// below ancestor anc (0 = anc itself; max < 0 = unlimited). This is the
// SINGLE shared depth evaluator: :depth(m,n), the combinators' default
// ranges, and :has / :has_parent's "any depth" (which is really
// [1,∞]) all route through it, so their semantics cannot drift.
func inTreeDepth(anc, n *treeNode, min, max int) bool {
	levels, ok := treeDepthBetween(anc, n)
	if !ok {
		return false
	}
	return levels >= min && (max < 0 || levels <= max)
}

// treeDepthBetween returns how many levels n sits below anc, and
// whether n is anc's self-or-descendant at all.
func treeDepthBetween(anc, n *treeNode) (int, bool) {
	d := 0
	for cur := n; cur != nil; cur = cur.parent {
		if cur == anc {
			return d, true
		}
		d++
	}
	return 0, false
}

// combRange is the depth range a combinator implies on the compound to
// its right when that compound carries no :depth override.
func combRange(c selComb) (min, max int) {
	if c == selChild {
		return 1, 1
	}
	return 1, -1
}

// anchorSpec pins what a complex's LEFTMOST compound is measured
// against. inverse flips the direction: normally the matched node must
// sit within range BELOW node; with inverse the matched node must be an
// ANCESTOR such that `node` sits within range below IT (that's
// :has_parent).
type anchorSpec struct {
	node     *treeNode
	min, max int
	inverse  bool
}

func (a anchorSpec) ok(matched *treeNode) bool {
	if a.node == nil {
		return true
	}
	if a.inverse {
		return inTreeDepth(matched, a.node, a.min, a.max)
	}
	return inTreeDepth(a.node, matched, a.min, a.max)
}

// ----------------------------------------------------------- evaluation

func (e *engine) matchList(n *treeNode, list selectorList, anchor anchorSpec) bool {
	for _, cx := range list {
		if e.matchChain(n, cx, len(cx.compounds)-1, anchor) {
			return true
		}
	}
	return false
}

// matchChain walks the combinator chain right-to-left. Each link
// resolves a depth range (the compound's own :depth override, else the
// combinator's default) and searches every ancestor within it —
// uniformly for child, descendant and :depth links.
func (e *engine) matchChain(n *treeNode, cx selComplex, idx int, anchor anchorSpec) bool {
	if !e.matchCompound(n, cx.compounds[idx]) {
		return false
	}
	if idx == 0 {
		a := anchor
		if dr := cx.compounds[0].depth; dr != nil {
			a.min, a.max = dr[0], dr[1]
		}
		return a.ok(n)
	}
	minD, maxD := combRange(cx.combs[idx-1])
	if dr := cx.compounds[idx].depth; dr != nil {
		minD, maxD = dr[0], dr[1]
	}
	for a, d := n, 0; a != nil; a, d = a.parent, d+1 {
		if maxD >= 0 && d > maxD {
			break
		}
		if d < minD {
			continue
		}
		if e.matchChain(a, cx, idx-1, anchor) {
			return true
		}
	}
	return false
}

func (e *engine) matchCompound(n *treeNode, comp selCompound) bool {
	if comp.root && n != e.project {
		return false
	}
	if !comp.anyType && n.class != comp.class {
		return false
	}
	for _, a := range comp.attrs {
		if !matchSelAttr(n, a) {
			return false
		}
	}
	for _, g := range comp.contains {
		if !e.nodeContains(n, g) {
			return false
		}
	}
	for _, inner := range comp.has {
		if !e.hasDescendantMatch(n, inner) {
			return false
		}
	}
	for _, inner := range comp.hasParent {
		if !e.hasAncestorMatch(n, inner) {
			return false
		}
	}
	for _, inner := range comp.references {
		if !e.referencesMatch(n, inner) {
			return false
		}
	}
	return true
}

// matchSelAttr tests a [name …] filter against every id the node
// answers to (see nodeIDs).
func matchSelAttr(n *treeNode, a selAttr) bool {
	for _, id := range n.nodeIDs() {
		switch a.op {
		case selExact:
			if id == a.value {
				return true
			}
		case selPrefix:
			if strings.HasPrefix(id, a.value) {
				return true
			}
		case selSuffix:
			if strings.HasSuffix(id, a.value) {
				return true
			}
		case selContains:
			if strings.Contains(id, a.value) {
				return true
			}
		}
	}
	return false
}

// hasDescendantMatch: any descendant at any depth matches inner. The
// inner selector's leftmost compound is anchored at n with the default
// [1,∞] range, so `:has(.argument:depth(1,1))` narrows to a DIRECT
// child without any special-casing.
func (e *engine) hasDescendantMatch(n *treeNode, inner selectorList) bool {
	anchor := anchorSpec{node: n, min: 1, max: -1}
	found := false
	var walk func(c *treeNode)
	walk = func(c *treeNode) {
		if found {
			return
		}
		if c != n && e.matchList(c, inner, anchor) {
			found = true
			return
		}
		for _, k := range e.kids(c) {
			walk(k)
		}
	}
	walk(n)
	return found
}

// hasAncestorMatch: any ancestor at any depth matches inner (NOT just
// the direct parent — that's what lets `.func:has_parent(#"a.ts")`
// reach functions nested inside classes/namespaces). Narrow to the
// direct parent with `:has_parent(<sel>:depth(1,1))`.
func (e *engine) hasAncestorMatch(n *treeNode, inner selectorList) bool {
	anchor := anchorSpec{node: n, min: 1, max: -1, inverse: true}
	for a := n.parent; a != nil; a = a.parent {
		if e.matchList(a, inner, anchor) {
			return true
		}
	}
	return false
}

// nodeContains is the :contains predicate — the boolean any-match
// projection of the SAME matcher grep uses.
func (e *engine) nodeContains(n *treeNode, g *grepSpec) bool {
	lines, _, ok := e.nodeSource(n)
	if !ok {
		return false // project/dir nodes have no source text of their own
	}
	for _, l := range lines {
		if g.matchLine(l) {
			return true
		}
	}
	return false
}

// referencesMatch implements X:references(Y) — OUTGOING direction: X
// references a symbol matching Y. The classic reverse lookup ("what
// references Start") is `*:references(#'app.go#Start')`.
//
// Y's matches are resolved to symbol NAMES, each name's reference sites
// are pulled from the index, and each SITE's node identity is its
// ENCLOSING symbol (a site inside no symbol belongs to the file node).
// X matches when it IS one of those enclosing nodes.
func (e *engine) referencesMatch(n *treeNode, inner selectorList) bool {
	set := e.referenceSet(inner)
	if n.class == "file" {
		return set[n.file+"#"]
	}
	if n.sym == "" {
		return false
	}
	return set[n.file+"#"+n.sym]
}

// referenceSet is memoized per query: the inner selector's reference
// sites don't change while one query runs, and :references is evaluated
// once per candidate node.
func (e *engine) referenceSet(inner selectorList) map[string]bool {
	// %p on a slice is its backing-array address: stable across calls
	// because the parsed AST is built once and only ever copied by
	// header, never reallocated.
	key := fmt.Sprintf("%p", inner)
	if e.refCache == nil {
		e.refCache = map[string]map[string]bool{}
	}
	if v, ok := e.refCache[key]; ok {
		return v
	}
	out := map[string]bool{}
	idx := e.s.getIndex()
	if idx == nil {
		e.refCache[key] = out
		return out
	}
	// Step 1: the symbol NAMES the inner selector matches.
	names := map[string]bool{}
	for _, cand := range e.allNodes(-1) {
		if cand.sym == "" {
			continue
		}
		if e.matchList(cand, inner, anchorSpec{node: e.project, min: 0, max: -1}) {
			names[cand.leaf] = true
		}
	}
	// Step 2: every site of those names → its enclosing symbol, MINUS the
	// declarations themselves.
	//
	// The index is lexical: it holds every occurrence of an identifier,
	// including the one in `func Save(...)`. That site's enclosing symbol is
	// Save, so Save came back as referencing itself and "who calls Save?"
	// answered "Caller, and also Save" — noise on the one query that most
	// justifies this over grep.
	//
	// A declaration is exactly the site at the symbol's own NAME position,
	// which the symbol table already knows. Excluding it separates the two
	// cases cleanly: a non-recursive Save drops out, while a recursive Walk
	// stays — its `Walk(n-1)` call is a real site somewhere other than its
	// name. (Same distinction LSP draws with references' includeDeclaration.)
	symCache := map[string][]symbols.Symbol{}
	for name := range names {
		for _, site := range idx.LookupExisting(name) {
			rel := relPath(site.File, e.s.getRoot())
			if e.isDeclSite(site.File, site.Line, site.Col, name, symCache) {
				continue
			}
			enclosing := e.s.enclosingSymPath(site.File, site.Line, symCache)
			out[rel+"#"+enclosing] = true
		}
	}
	e.refCache[key] = out
	return out
}

// isDeclSite reports whether a lexical site IS a declaration — the identifier
// occurrence at a symbol's own name position — rather than a use of it.
//
// The index can't tell them apart (it stores occurrences, not roles), but the
// symbol table can: Symbol.Name* is exactly the declaration's identifier span.
// Shares enclosingSymPath's cache — same files, same parse, keyed by abs path.
func (e *engine) isDeclSite(absFile string, line, col int, name string, cache map[string][]symbols.Symbol) bool {
	syms, ok := cache[absFile]
	if !ok {
		if content, err := os.ReadFile(absFile); err == nil {
			syms, _ = symbols.FileSymbols(e.s.languageForFile(absFile), content)
		}
		cache[absFile] = syms
	}
	for _, sym := range syms {
		if lastSeg(sym.Sym) == name && sym.NameStartLine == line && sym.NameStartCol == col {
			return true
		}
	}
	return false
}

// subjectMaxDepth returns the deepest tree level the subject of this
// selector list could possibly occupy, or -1 when unbounded. Purely an
// optimization: it lets `:root > *` (the canonical tour) prune the walk
// at depth 1 and therefore never parse a single file's symbols.
func subjectMaxDepth(list selectorList) int {
	worst := 0
	for _, cx := range list {
		d, ok := complexMaxDepth(cx)
		if !ok {
			return -1
		}
		if d > worst {
			worst = d
		}
	}
	return worst
}

func complexMaxDepth(cx selComplex) (int, bool) {
	var d int
	switch {
	case cx.compounds[0].root:
		d = 0 // :root matches only the project node
	case cx.compounds[0].depth != nil:
		if cx.compounds[0].depth[1] < 0 {
			return 0, false
		}
		d = cx.compounds[0].depth[1]
	default:
		return 0, false // unanchored: the subject may live anywhere
	}
	for i := range cx.combs {
		_, max := combRange(cx.combs[i])
		if dr := cx.compounds[i+1].depth; dr != nil {
			max = dr[1]
		}
		if max < 0 {
			return 0, false
		}
		d += max
	}
	return d, true
}

// evaluate runs a parsed selector over the tree and returns the
// matching nodes in deterministic pre-order.
func (e *engine) evaluate(list selectorList) []*treeNode {
	anchor := anchorSpec{node: e.project, min: 0, max: -1}
	var out []*treeNode
	for _, n := range e.allNodes(subjectMaxDepth(list)) {
		if e.matchList(n, list, anchor) {
			out = append(out, n)
		}
	}
	return out
}

// ----------------------------------------------------------- grep

// grepSpec is the parsed form of the `grep` field's GNU-grep-style flag
// string — and, identically, of a :contains(...) argument. Everything
// compiles down to ONE regexp so the two callers cannot drift:
// literal patterns are quoted, -w wraps in word boundaries, -i prepends
// the inline flag.
//
// Deliberate deviation from GNU grep: the DEFAULT is a literal
// substring (as if -F), not a basic regex. -E opts into a regex (Go's
// RE2 ~ ERE). This keeps `grep`'s default identical to :contains's
// documented "substring by default", and BRE is not worth implementing.
type grepSpec struct {
	pattern       string
	regex         bool // -E
	fixed         bool // -F
	ignoreCase    bool // -i
	word          bool // -w
	invert        bool // -v
	before, after int  // -B / -A / -C

	re *regexp.Regexp
}

func (g *grepSpec) matchLine(line string) bool {
	return g.re.MatchString(line) != g.invert
}

func (g *grepSpec) compile() error {
	pat := g.pattern
	if !g.regex {
		pat = regexp.QuoteMeta(pat)
	}
	if g.word {
		pat = `\b(?:` + pat + `)\b`
	}
	if g.ignoreCase {
		pat = `(?i)` + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return fmt.Errorf("invalid pattern %q: %w", g.pattern, err)
	}
	g.re = re
	return nil
}

// applyBoolFlag applies one boolean short flag, reporting whether it
// was recognized. Shared by grep and :contains so the flag vocabulary
// can't drift between them.
func applyBoolFlag(g *grepSpec, c byte) bool {
	switch c {
	case 'i':
		g.ignoreCase = true
	case 'w':
		g.word = true
	case 'E':
		g.regex = true
	case 'F':
		g.fixed = true
	case 'v':
		g.invert = true
	case 'n':
		// Accepted and ignored: hits always carry line numbers.
	default:
		return false
	}
	return true
}

func unsupportedFlagErr(c byte) error {
	return fmt.Errorf("grep: unsupported flag %q — supported: -i -w -E -F -v -n -A<n> -B<n> -C<n>. The selector scopes the search, so -r and file arguments are never accepted", "-"+string(c))
}

// finalize validates the mode combination and compiles the matcher.
func (g *grepSpec) finalize() error {
	if g.regex && g.fixed {
		return fmt.Errorf("grep: -E and -F are mutually exclusive")
	}
	if g.fixed {
		g.regex = false
	}
	if g.pattern == "" {
		return fmt.Errorf("grep: a pattern is required (e.g. \"-i derp\")")
	}
	return g.compile()
}

// parseGrepSpec parses "-i -A2 derp" / "-E 'foo|bar'" — a bounded flag
// set plus exactly one trailing pattern. Unsupported flags (notably -r
// and bare file arguments) are REJECTED with a guided error: the
// selector already does the scoping, so grep must never walk the
// filesystem itself.
func parseGrepSpec(s string) (*grepSpec, error) {
	toks, err := tokenizeGrep(s)
	if err != nil {
		return nil, err
	}
	g := &grepSpec{}
	var pattern *string
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t == "--" {
			if i+1 < len(toks) {
				v := toks[i+1]
				pattern = &v
				i++
			}
			continue
		}
		if len(t) < 2 || t[0] != '-' {
			if pattern != nil {
				return nil, fmt.Errorf("grep: unexpected extra argument %q — pass ONE pattern; the selector does the file scoping, grep never takes file arguments", t)
			}
			v := t
			pattern = &v
			continue
		}
		// A bundle of short flags: -i, -iw, -A2, -iA2 …
		rest := t[1:]
		for len(rest) > 0 {
			c := rest[0]
			rest = rest[1:]
			if applyBoolFlag(g, c) {
				continue
			}
			switch c {
			case 'A', 'B', 'C':
				num := rest
				rest = ""
				if num == "" {
					if i+1 >= len(toks) {
						return nil, fmt.Errorf("grep: -%c needs a number (e.g. -%c2)", c, c)
					}
					i++
					num = toks[i]
				}
				v, err := strconv.Atoi(num)
				if err != nil || v < 0 {
					return nil, fmt.Errorf("grep: -%c needs a non-negative number, got %q", c, num)
				}
				switch c {
				case 'A':
					g.after = v
				case 'B':
					g.before = v
				case 'C':
					g.before, g.after = v, v
				}
			default:
				return nil, unsupportedFlagErr(c)
			}
		}
	}
	if pattern != nil {
		g.pattern = *pattern
	}
	if err := g.finalize(); err != nil {
		return nil, err
	}
	return g, nil
}

// parseContainsSpec parses a :contains("…") argument: optional LEADING
// boolean flags, then the rest of the string VERBATIM as the pattern.
//
// The pattern is deliberately not re-tokenized the way grep's is —
// :contains("_ = 2") must match the literal "_ = 2", spaces and all,
// because "substring by default" is the whole point of the predicate.
// The flags, matcher and literal-vs-regex duality are otherwise
// identical to grep's (same grepSpec, same compile).
//
// Context flags (-A/-B/-C) are rejected: :contains is a yes/no
// predicate, so there is nothing to give context to.
func parseContainsSpec(text string) (*grepSpec, error) {
	g := &grepSpec{}
	rest := text
	for {
		trimmed := strings.TrimLeft(rest, " \t")
		if len(trimmed) < 2 || trimmed[0] != '-' {
			rest = trimmed
			break
		}
		tok := trimmed
		if i := strings.IndexAny(trimmed, " \t"); i >= 0 {
			tok, rest = trimmed[:i], trimmed[i:]
		} else {
			rest = ""
		}
		for _, c := range []byte(tok[1:]) {
			if applyBoolFlag(g, c) {
				continue
			}
			switch c {
			case 'A', 'B', 'C':
				return nil, fmt.Errorf(":contains: -%c doesn't apply — :contains is a yes/no predicate. Use node_query's grep field for context lines", c)
			default:
				return nil, unsupportedFlagErr(c)
			}
		}
		if rest == "" {
			break
		}
	}
	g.pattern = rest
	if err := g.finalize(); err != nil {
		return nil, err
	}
	return g, nil
}

// tokenizeGrep splits on whitespace, honoring simple single/double
// quoting for the pattern (as in "-E 'foo|bar'"). No backslash escapes
// — same rule as the selector's ids.
func tokenizeGrep(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	var quote rune
	started := false
	flush := func() {
		if started {
			out = append(out, cur.String())
			cur.Reset()
			started = false
		}
	}
	for _, c := range s {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
				continue
			}
			cur.WriteRune(c)
		case c == '\'' || c == '"':
			quote = c
			started = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		default:
			cur.WriteRune(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("grep: unbalanced quote")
	}
	flush()
	return out, nil
}

// grepHit is one matched line plus its context, in the before/after
// convention symbols.Search already uses.
type grepHit struct {
	Line   int      `json:"line"`
	Text   string   `json:"text"`
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}

// grepNode projects a node's own source through the matcher: every
// matching line, with -A/-B/-C context. Context is clipped to the
// node's own span — a node's hits never leak lines from its neighbours.
func (e *engine) grepNode(n *treeNode, g *grepSpec) []grepHit {
	lines, startLine, ok := e.nodeSource(n)
	if !ok {
		return nil
	}
	var out []grepHit
	for i, l := range lines {
		if !g.matchLine(l) {
			continue
		}
		h := grepHit{Line: startLine + i, Text: l}
		if g.before > 0 {
			lo := i - g.before
			if lo < 0 {
				lo = 0
			}
			if lo < i {
				h.Before = append([]string(nil), lines[lo:i]...)
			}
		}
		if g.after > 0 {
			hi := i + 1 + g.after
			if hi > len(lines) {
				hi = len(lines)
			}
			if i+1 < hi {
				h.After = append([]string(nil), lines[i+1:hi]...)
			}
		}
		out = append(out, h)
	}
	return out
}
