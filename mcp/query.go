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

	// Pre-order sort key. fileOrd is the creation sequence of the
	// project/dir/file walk (symbols inherit their file's); symOrd is the
	// symbol's source-order sequence within its file (0 for non-symbols).
	// Sorting by (fileOrd, symOrd) reproduces a full pre-order walk
	// WITHOUT forcing every file's symbols to load just to order results.
	fileOrd int
	symOrd  int

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

	// ordSeq feeds treeNode.fileOrd during the workspace walk.
	ordSeq int

	// fileByRel indexes file nodes by workspace-relative path — how a
	// reference site (which is file+line) becomes a tree node again.
	fileByRel map[string]*treeNode

	// Per-query memos. All are keyed for the life of ONE query: the tree
	// and the index don't change under a running evaluation.
	//
	// matchSetCache: full-tree match set per inner selector AST (used by
	// :parents to test "does this referrer match sel"), keyed by the AST's
	// backing-array pointer plus the relaxation flag.
	// refByName: referrer nodes per symbol NAME (one reference hop).
	// symCache: parsed symbols per abs file, shared by decl-site checks
	// and enclosing-symbol lookups.
	matchSetCache map[string]map[*treeNode]bool
	refByName     map[string][]*treeNode
	symCache      map[string][]symbols.Symbol
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
	for i, sym := range syms {
		parent := f
		if j := strings.LastIndex(sym.Sym, "."); j >= 0 {
			if p, ok := bySym[sym.Sym[:j]]; ok {
				parent = p
			}
		}
		n := &treeNode{
			class:   sym.Class,
			leaf:    lastSeg(sym.Sym),
			full:    sym.Sym,
			file:    f.file,
			sym:     sym.Sym,
			at:      [2]int{sym.DeclStartLine, sym.DeclEndLine},
			parent:  parent,
			depth:   parent.depth + 1,
			abs:     f.abs,
			fileOrd: f.fileOrd,
			symOrd:  i + 1,
		}
		bySym[sym.Sym] = n
		parent.children = append(parent.children, n)
	}
}

// nodeByAddr resolves a workspace-relative file path plus dotted sym
// path back to its tree node (sym "" = the file node itself). This is
// how a reference SITE re-enters the tree during a :parents move.
func (e *engine) nodeByAddr(rel, sym string) *treeNode {
	f := e.fileByRel[rel]
	if f == nil || sym == "" {
		return f
	}
	var find func(n *treeNode) *treeNode
	find = func(n *treeNode) *treeNode {
		for _, c := range e.kids(n) {
			if c.sym == sym {
				return c
			}
			if strings.HasPrefix(sym, c.sym+".") {
				return find(c)
			}
		}
		return nil
	}
	return find(f)
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
	e := &engine{s: s, project: project, fileByRel: map[string]*treeNode{}}
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
		e.ordSeq++
		if de.IsDir() {
			d := &treeNode{
				class: "dir", leaf: de.Name(), full: rel, file: rel,
				parent: parent, depth: parent.depth + 1, abs: childAbs,
				fileOrd: e.ordSeq,
			}
			parent.children = append(parent.children, d)
			e.walkDir(childAbs, d)
			continue
		}
		f := &treeNode{
			class: "file", leaf: de.Name(), full: rel, file: rel,
			parent: parent, depth: parent.depth + 1, abs: childAbs,
			fileOrd: e.ordSeq,
		}
		f.at = [2]int{1, countFileLines(childAbs)}
		parent.children = append(parent.children, f)
		e.fileByRel[rel] = f
	}
}

func countFileLines(abs string) int {
	content, err := os.ReadFile(abs)
	if err != nil {
		return 0
	}
	return len(splitNodeReadLines(content))
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
	implied bool   // the universal was IMPLIED (nothing written) — scope binding keys off this
	class   string // type selector (func), if !anyType
	root    bool   // :root — matches the single .project node

	attrs []selAttr

	// depth is an optional :depth(m,n) override. It REPLACES the
	// default range of the relation to this compound's left — the
	// preceding combinator, or (for the leftmost compound) the range
	// the whole complex is anchored by.
	depth *[2]int

	// pseudos hold the compound's semantic pseudo-classes IN WRITTEN
	// ORDER. Order matters because :parents MOVES the tip: filters
	// before a move test the pre-move node, filters after it test the
	// referrers it moved to.
	pseudos []selPseudo
}

// hasMove reports whether the compound re-roots (carries a :parents).
func (c *selCompound) hasMove() bool {
	for _, ps := range c.pseudos {
		if ps.kind == pseudoParents {
			return true
		}
	}
	return false
}

type selPseudoKind int

const (
	pseudoContains selPseudoKind = iota // :contains('text')  — filter on own source
	pseudoParents                       // :parents(sel){m,n} — MOVE to referrers
	pseudoWhere                         // :where(sel)        — filter: a path matches
	pseudoAny                           // :any(sel)          — ∃ claim
	pseudoAll                           // :all(sel)          — ∀ claim
	pseudoEmpty                         // :empty(sel)        — ∄ claim
)

type selPseudo struct {
	kind  selPseudoKind
	grep  *grepSpec    // pseudoContains only
	inner selectorList // every other kind

	// pseudoParents only: how many reference hops the move may take.
	// max < 0 = unbounded (fixpoint). Default {1,1}.
	min, max int
}

type selComb int

const (
	selDescendant selComb = iota // whitespace → [1,∞]
	selChild                     // '>'        → [1,1]
)

// selRel is how a complex's LEFTMOST compound anchors to the node it is
// evaluated from. Top-level selectors anchor to the project self-or-
// below. Inside :where/:any/:all/:empty the selector is RELATIVE (as in
// CSS :has): descendants by default, children after a leading '>', or
// the anchor node ITSELF when the first compound is pseudo-only (that
// is what lets `:any(:parents(#'main'))` ask about THIS node's
// referrers rather than a descendant's).
type selRel int

const (
	relTop        selRel = iota // [0,∞] below the anchor (top level, :parents inner)
	relDescendant               // [1,∞]
	relChild                    // [1,1]
	relScope                    // [0,0] — the anchor itself
)

func (r selRel) rng() (min, max int) {
	switch r {
	case relDescendant:
		return 1, -1
	case relChild:
		return 1, 1
	case relScope:
		return 0, 0
	}
	return 0, -1
}

// selComplex is a chain of compounds joined by combinators; the LAST
// compound is the subject.
type selComplex struct {
	compounds []selCompound
	combs     []selComb
	rel       selRel
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
		comp.implied = true
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
			if !sawType && !comp.root && len(comp.attrs) == 0 &&
				len(comp.pseudos) == 0 && comp.depth == nil {
				return comp, p.errf("a type tag ('func'), '*', '#id', '[name…]' or a pseudo-class")
			}
			return comp, nil
		}
	}
}

// scopeBound reports whether a compound, used as the FIRST compound of a
// relative selector, binds to the anchor node itself: nothing positional
// was written (no tag, no '*', no id/attr), only pseudo-classes.
func (c *selCompound) scopeBound() bool {
	return c.implied && !c.root && len(c.attrs) == 0
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
	case "parents":
		inner, err := p.parsePseudoArg(name, false)
		if err != nil {
			return err
		}
		ps := selPseudo{kind: pseudoParents, inner: inner, min: 1, max: 1}
		if p.peek() == '{' {
			if err := p.parseHopRange(&ps); err != nil {
				return err
			}
		}
		comp.pseudos = append(comp.pseudos, ps)
		return nil
	case "where", "any", "all", "empty":
		inner, err := p.parsePseudoArg(name, true)
		if err != nil {
			return err
		}
		kind := map[string]selPseudoKind{
			"where": pseudoWhere, "any": pseudoAny, "all": pseudoAll, "empty": pseudoEmpty,
		}[name]
		comp.pseudos = append(comp.pseudos, selPseudo{kind: kind, inner: inner})
		return nil
	case "has", "has_parent", "references":
		return removedPseudoErr(name)
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
		comp.pseudos = append(comp.pseudos, selPseudo{kind: pseudoContains, grep: g})
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

// parsePseudoArg parses "(<selector list>)". relative=true applies the
// relative-selector anchoring rules (see selRel); false leaves the list
// global (relTop) — that's :parents's inner, which describes the
// referrer in workspace terms.
func (p *modSelParser) parsePseudoArg(name string, relative bool) (selectorList, error) {
	if p.peek() != '(' {
		return nil, p.errf("'(' after :" + name)
	}
	p.i++
	var list selectorList
	var err error
	if relative {
		list, err = p.parseRelativeList()
	} else {
		list, err = p.parseList()
	}
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.peek() != ')' {
		return nil, p.errf("')' to close :" + name)
	}
	p.i++
	return list, nil
}

// parseRelativeList parses a selector list whose complexes anchor
// RELATIVE to the node under test, CSS-:has-style: descendants by
// default, children after a leading '>', the node itself when the first
// compound is pseudo-only.
func (p *modSelParser) parseRelativeList() (selectorList, error) {
	var list selectorList
	for {
		p.skipWS()
		rel := relDescendant
		if p.peek() == '>' {
			p.i++
			p.skipWS()
			rel = relChild
		}
		cx, err := p.parseComplex()
		if err != nil {
			return nil, err
		}
		if rel == relDescendant && cx.compounds[0].scopeBound() {
			rel = relScope
		}
		cx.rel = rel
		list = append(list, cx)
		p.skipWS()
		if p.peek() == ',' {
			p.i++
			continue
		}
		return list, nil
	}
}

// parseHopRange parses :parents(...)'s "{m}", "{m,}" or "{m,n}" suffix.
func (p *modSelParser) parseHopRange(ps *selPseudo) error {
	p.i++ // '{'
	p.skipWS()
	lo, ok := p.readNonNegInt()
	if !ok {
		return p.errf("a hop count, e.g. :parents(func){1,}")
	}
	if lo < 1 {
		return fmt.Errorf(":parents(...){%d,…}: hops start at 1 (0 hops is the node itself — just drop the :parents)", lo)
	}
	ps.min, ps.max = lo, lo
	p.skipWS()
	if p.peek() == ',' {
		p.i++
		p.skipWS()
		if p.peek() == '}' {
			ps.max = -1 // {m,} — unbounded, evaluated as a fixpoint
		} else {
			hi, ok := p.readNonNegInt()
			if !ok {
				return p.errf("a max hop count or '}' (as in {1,} = unbounded)")
			}
			if hi < lo {
				return fmt.Errorf(":parents(...){%d,%d}: max must be >= min", lo, hi)
			}
			ps.max = hi
			p.skipWS()
		}
	}
	if p.peek() != '}' {
		return p.errf("'}' to close the {m,n} hop range")
	}
	p.i++
	return nil
}

// removedPseudoErr answers the three retired pseudos with their modern
// spelling — terse, naming the fix (see unknownTypeErr for why).
func removedPseudoErr(name string) error {
	switch name {
	case "has":
		return fmt.Errorf(":has is now :any — file:any(func#test) = files with such a descendant. :all/:empty quantify the same way. Pass selector \"?\" for the grammar.")
	case "has_parent":
		return fmt.Errorf(":has_parent is gone: write the ancestor BEFORE the node — func:has_parent(#'a.ts') is now \"#'a.ts' func\". Pass selector \"?\" for the grammar.")
	}
	return fmt.Errorf(":references is inverted into :parents — callers of X are \"#'a.go#X':parents(*)\"; transitive: :parents(*){1,}; \"what does X call\": func:any(:parents(#'X')). Pass selector \"?\" for the grammar.")
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

const selectorGrammarHelp = `Selector grammar (CSS over the unified node tree, plus ONE move that leaves it).

At every point in a selector there is a set of paths; each segment builds new
paths from the last tip. Combinators walk DOWN through containment — that half
is literally CSS. :parents(sel) INVERTS: the tip becomes the nodes matching sel
that REFERENCE the current tip. That is the graph; it may happen at any point
in the chain, and the next segment continues from the new tip.

A node's TYPE is a bare tag, like CSS's div/span — a fixed set you cannot
invent. Anything the workspace NAMES — a dir, a file, a symbol — is an #id. So
the cache/ directory is #cache, never "cache". There are no classes.

  TYPES  project dir file
         func method type struct interface class const var field enum ctor module import argument
         * = any type.
  TREE   project#<root-name> > dir#<relpath> > file#<relpath> > <symbols, dotted-nested> > argument#<param>
         :root is a pseudo-class matching the single project node (as CSS's :root matches <html>).
  ID     #bare  (charset [A-Za-z_][A-Za-z0-9_.-]*)  or  #'anything else'  (spaces, slashes, …). Quote instead of escaping; there is no backslash escape.
         A symbol answers to its leaf name, its dotted path, and its full "<file>#<sym>" address.
         A dir/file answers to its basename AND its workspace-relative path.
  ATTR   [name=X] exact  [name^=X] prefix  [name$=X] suffix  [name*=X] contains.  #id is sugar for [name=id].
  MOVE   :parents(<sel>)      tip := nodes matching <sel> that reference the tip.  #'a.go#Save':parents(*) = Save's callers.
         :parents(<sel>){m,n} m..n reference hops; every hop must match <sel>. {1,} = transitive: :parents(func){1,} = all callers, callers' callers, …
  SET    :where(<sel>)  FILTER: keep tips a path of <sel> connects to (flows on)
         :any(<sel>) ∃  :all(<sel>) ∀  :empty(<sel>) ∄  CLAIM the paths of <sel> connect / all connect / none do.
         <sel> here is RELATIVE (as in CSS :has): descendants by default, '>' = children,
         and a form starting with :parents(...) starts AT the tip — func:any(:parents(#'main')) = the funcs main calls.
  PSEUDO :contains('text')   this node's own source contains text (same flags as grep: -i -w -E -F -v)
         :depth(m,n)         m..n levels from the PREVIOUS target in the chain (0 = that target itself);
                             with no preceding target, measured from :root. Overrides the combinator's default range.
  COMB   space = descendant [1,∞]   '>' = direct child [1,1]
  COMMA  union: "func, method"
Examples: file = EVERY file, nested included  |  #cache > file = files directly in dir cache/  |  :root > * = ONLY top level
          #'store.go' func = funcs in that file  |  func[name^=Test]  |  #'store.go#Save':parents(*) = who calls Save
          #'Save':parents(func){1,} argument = args of every transitive caller  |  func:empty(:parents(*)) = dead code
          file:any(func#test)  |  method:all(argument[name*=ctx])`

// ----------------------------------------------------------- evaluation
//
// Selectors are evaluated FORWARD, left to right, as the grammar reads:
// at every point in the chain there is a SET of nodes (the tips of the
// paths built so far). A combinator extends every path downward through
// containment — that half is a DAG and is literally CSS. A :parents
// move re-roots the tip at the referrers pointing at it — that is where
// the graph (cycles, fan-in) enters, and it may happen at ANY point in
// the chain; the next segment simply continues from the new tips.
//
// The old evaluator matched right-to-left, one candidate node at a
// time, which cannot express a mid-chain re-root at all.

// combRange is the depth range a combinator implies on the compound to
// its right when that compound carries no :depth override.
func combRange(c selComb) (min, max int) {
	if c == selChild {
		return 1, 1
	}
	return 1, -1
}

// evaluate runs a parsed selector over the tree and returns the
// matching nodes in deterministic pre-order.
func (e *engine) evaluate(list selectorList) []*treeNode {
	set := e.evalList(list, e.project, false)
	out := make([]*treeNode, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].fileOrd != out[j].fileOrd {
			return out[i].fileOrd < out[j].fileOrd
		}
		return out[i].symOrd < out[j].symOrd
	})
	return out
}

// evalList evaluates every complex in the union from `anchor` and
// returns the merged tip set.
//
// relaxSubject drives :all's ∀: evaluated once as written (the matched
// set) and once with the SUBJECT's own constraints dropped (the
// domain). ∀ holds iff the two sets are equal — every node the
// structure can reach also passes the subject's tests. When the subject
// re-roots, the relaxation applies to its last move's inner selector
// (recursively), because that inner is what names the reached nodes.
func (e *engine) evalList(list selectorList, anchor *treeNode, relaxSubject bool) map[*treeNode]bool {
	out := map[*treeNode]bool{}
	for _, cx := range list {
		for n := range e.evalComplex(cx, anchor, relaxSubject) {
			out[n] = true
		}
	}
	return out
}

func (e *engine) evalComplex(cx selComplex, anchor *treeNode, relaxSubject bool) map[*treeNode]bool {
	last := len(cx.compounds) - 1
	comp := &cx.compounds[0]
	min, max := cx.rel.rng()
	if comp.depth != nil {
		min, max = comp.depth[0], comp.depth[1]
	}
	relaxed := relaxSubject && last == 0
	tips := e.collectMatches(anchor, min, max, comp, relaxed && !comp.hasMove())
	tips = e.applyPseudos(tips, comp, relaxed)
	for i, comb := range cx.combs {
		comp = &cx.compounds[i+1]
		cmin, cmax := combRange(comb)
		if comp.depth != nil {
			cmin, cmax = comp.depth[0], comp.depth[1]
		}
		relaxed = relaxSubject && i+1 == last
		next := map[*treeNode]bool{}
		for t := range tips {
			for m := range e.collectMatches(t, cmin, cmax, comp, relaxed && !comp.hasMove()) {
				next[m] = true
			}
		}
		tips = e.applyPseudos(next, comp, relaxed)
	}
	return tips
}

// collectMatches walks anchor's subtree within [min,max] levels (0 =
// the anchor itself; max < 0 = unbounded) and returns the nodes whose
// POSITIONAL part (tag, :root, ids/attrs) matches comp. relaxed skips
// the positional tests — that's :all's domain pass.
//
// Two prunings keep the DAG half cheap: a depth bound stops the walk,
// and a compound that can only name project/dir/file nodes never loads
// a file's symbols at all (everything below a file is a symbol).
func (e *engine) collectMatches(anchor *treeNode, min, max int, comp *selCompound, relaxed bool) map[*treeNode]bool {
	out := map[*treeNode]bool{}
	if comp.root {
		// :root can only ever be the single project node, at depth 0.
		if anchor == e.project && min <= 0 {
			out[e.project] = true
		}
		return out
	}
	needSyms := relaxed || compNeedsSymbols(comp)
	var walk func(n *treeNode, d int)
	walk = func(n *treeNode, d int) {
		if d >= min && (relaxed || e.positionalMatch(n, comp)) {
			out[n] = true
		}
		if max >= 0 && d >= max {
			return
		}
		if n.class == "file" && !needSyms {
			return
		}
		for _, c := range e.kids(n) {
			walk(c, d+1)
		}
	}
	walk(anchor, 0)
	return out
}

// compNeedsSymbols reports whether the compound could match a node that
// lives INSIDE a file. Only then is parsing a file's symbols worth it.
func compNeedsSymbols(comp *selCompound) bool {
	if comp.anyType {
		return true
	}
	switch comp.class {
	case "project", "dir", "file":
		return false
	}
	return true
}

func (e *engine) positionalMatch(n *treeNode, comp *selCompound) bool {
	if !comp.anyType && n.class != comp.class {
		return false
	}
	for _, a := range comp.attrs {
		if !matchSelAttr(n, a) {
			return false
		}
	}
	return true
}

// applyPseudos runs a compound's pseudo-classes over the tip set IN
// WRITTEN ORDER. Filters (:contains/:where/:any/:all/:empty) narrow the
// set; :parents MOVES it — so filters written before a move test the
// pre-move node, filters after it test the referrers.
//
// relaxed (the :all domain pass) drops the filters — they are the
// property under test, not the domain — and relaxes the LAST move's
// inner selector, which is what names the nodes the move reaches.
func (e *engine) applyPseudos(set map[*treeNode]bool, comp *selCompound, relaxed bool) map[*treeNode]bool {
	lastMove := -1
	for i := range comp.pseudos {
		if comp.pseudos[i].kind == pseudoParents {
			lastMove = i
		}
	}
	cur := set
	for i := range comp.pseudos {
		ps := &comp.pseudos[i]
		if len(cur) == 0 {
			return cur
		}
		if ps.kind == pseudoParents {
			cur = e.moveParents(cur, ps, relaxed && i == lastMove)
			continue
		}
		if relaxed {
			continue
		}
		next := map[*treeNode]bool{}
		for n := range cur {
			if e.pseudoHolds(n, ps) {
				next[n] = true
			}
		}
		cur = next
	}
	return cur
}

// pseudoHolds evaluates one filter pseudo against one node. The inner
// selector is RELATIVE to n (see selRel), so :any(func) asks about n's
// descendants and :any(:parents(S)) about n's own referrers.
func (e *engine) pseudoHolds(n *treeNode, ps *selPseudo) bool {
	switch ps.kind {
	case pseudoContains:
		return e.nodeContains(n, ps.grep)
	case pseudoWhere, pseudoAny:
		// :where and :any coincide while the set is tested tip-by-tip;
		// :where is documented as the filter (subset flows on), :any as
		// the ∃ claim. Kept distinct so path-level filtering can later
		// diverge without a grammar change.
		return len(e.evalList(ps.inner, n, false)) > 0
	case pseudoEmpty:
		return len(e.evalList(ps.inner, n, false)) == 0
	case pseudoAll:
		// ∀: everything the structure REACHES (domain: subject
		// constraints relaxed) must also MATCH as written. The domain is
		// a reachability set, never an enumeration of paths — cycles
		// cost nothing, and ∀ needs no bound (see the icebox trade note:
		// one non-matching node kills the ∀ even if a clean path exists
		// beside it; that is what never enumerating costs).
		matched := e.evalList(ps.inner, n, false)
		domain := e.evalList(ps.inner, n, true)
		if len(domain) != len(matched) {
			return false
		}
		for d := range domain {
			if !matched[d] {
				return false
			}
		}
		return true
	}
	return false
}

// moveParents is THE move: tip := the nodes matching ps.inner that hold
// a reference to the current tip. Repeated min..max hops; every hop
// must match the inner selector (that is what "through" means — the
// intermediates are named, hence constrained).
func (e *engine) moveParents(set map[*treeNode]bool, ps *selPseudo, relaxInner bool) map[*treeNode]bool {
	referrers := e.globalMatchSet(ps.inner, relaxInner)
	out := map[*treeNode]bool{}
	frontier := set
	if ps.max < 0 {
		// Unbounded {m,}: a fixpoint over the reference graph. The
		// visited set is what bounds cycles (Walk → Walk): re-reaching a
		// node never grows the frontier, so the loop terminates when the
		// subgraph is stable. Each node lands at its SHORTEST hop.
		visited := map[*treeNode]bool{}
		for hop := 1; len(frontier) > 0; hop++ {
			next := map[*treeNode]bool{}
			for t := range frontier {
				for _, r := range e.referrersOf(t) {
					if visited[r] || !referrers[r] {
						continue
					}
					visited[r] = true
					next[r] = true
				}
			}
			if hop >= ps.min {
				for n := range next {
					out[n] = true
				}
			}
			frontier = next
		}
		return out
	}
	for hop := 1; hop <= ps.max && len(frontier) > 0; hop++ {
		next := map[*treeNode]bool{}
		for t := range frontier {
			for _, r := range e.referrersOf(t) {
				if referrers[r] {
					next[r] = true
				}
			}
		}
		if hop >= ps.min {
			for n := range next {
				out[n] = true
			}
		}
		frontier = next
	}
	return out
}

// globalMatchSet memoizes "every node matching this selector list" per
// inner AST for the life of one query — :parents tests each referrer
// against it, potentially once per hop.
//
// %p on a slice is its backing-array address: stable across calls
// because the parsed AST is built once and only ever copied by header.
func (e *engine) globalMatchSet(list selectorList, relaxed bool) map[*treeNode]bool {
	key := fmt.Sprintf("%p:%t", list, relaxed)
	if e.matchSetCache == nil {
		e.matchSetCache = map[string]map[*treeNode]bool{}
	}
	if v, ok := e.matchSetCache[key]; ok {
		return v
	}
	v := e.evalList(list, e.project, relaxed)
	e.matchSetCache[key] = v
	return v
}

// referrersOf returns the nodes holding a reference to t — each
// reference SITE of t's name resolved to its enclosing symbol (or its
// file, for a site inside no symbol), minus t's own declaration.
// Memoized per NAME: the lexical index is name-keyed, so two symbols
// sharing a name share referrers (a known index limitation, unchanged
// from :references).
func (e *engine) referrersOf(t *treeNode) []*treeNode {
	if t.sym == "" {
		return nil // project/dir/file nodes have no indexed name
	}
	name := t.leaf
	if e.refByName == nil {
		e.refByName = map[string][]*treeNode{}
	}
	if v, ok := e.refByName[name]; ok {
		return v
	}
	if e.symCache == nil {
		e.symCache = map[string][]symbols.Symbol{}
	}
	var out []*treeNode
	seen := map[*treeNode]bool{}
	if idx := e.s.getIndex(); idx != nil {
		for _, site := range idx.LookupExisting(name) {
			// A declaration is not a reference: the site at the symbol's
			// own NAME position is excluded, so a non-recursive Save has
			// no referrer named Save while a recursive Walk keeps its
			// real Walk(n-1) site. (LSP's includeDeclaration line.)
			if e.isDeclSite(site.File, site.Line, site.Col, name, e.symCache) {
				continue
			}
			rel := relPath(site.File, e.s.getRoot())
			n := e.nodeByAddr(rel, e.s.enclosingSymPath(site.File, site.Line, e.symCache))
			if n != nil && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	e.refByName[name] = out
	return out
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
