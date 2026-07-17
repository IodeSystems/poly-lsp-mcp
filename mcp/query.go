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

	// Ref nodes (class == "ref") — a reified reference edge, generated
	// under its innermost enclosing symbol (.out: under the SOURCE;
	// .in: under the TARGET). `at` is the SITE line, `file`/`abs` the
	// site's file, and the far end(s) are this node's only children.
	// Generated: `*` never matches one and walks never enter one — only
	// naming ::in/::out does (the CSS pseudo-element contract).
	refDir  string      // "in" | "out"
	refKind string      // "call" | "type" | "import" | ""
	refConf string      // "lexical" today; "lsp" when a child LSP resolves it
	refFar  []*treeNode // the far end(s) — >1 only under name collisions

	// refs are the node's generated ref children, materialized lazily
	// (refsLoaded) and kept OUT of children so containment walks and
	// `*` never see them.
	refs       []*treeNode
	refsLoaded bool
}

// addr renders the node's address — the exact string node_read /
// node_edit accept. Dirs get their relpath (addressing one is a clear
// error at read/edit time, not here). A ref node addresses its SITE:
// "<file>@<line>" — reading it is the site line, editing it edits the
// call site.
func (n *treeNode) addr() string {
	switch n.class {
	case "project":
		return n.full
	case "dir", "file":
		return n.file
	case "ref":
		return fmt.Sprintf("%s@%d", n.file, n.at[0])
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
	case "ref":
		// A ref answers to its FAR END's ids — ::out.call#'Save'
		// names the edge by what it points at (or, for .in, points from).
		var ids []string
		for _, f := range n.refFar {
			ids = append(ids, f.nodeIDs()...)
		}
		return ids
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
	// siteCache: classified non-declaration sites per file (ref-node
	// materialization).
	// declsByName: every declared node per name — the far ends of
	// outgoing edges.
	// symCache: parsed symbols per abs file, shared by decl-site checks
	// and enclosing-symbol lookups.
	siteCache   map[string][]refSite
	declsByName map[string][]*treeNode
	symCache    map[string][]symbols.Symbol
}

// kids returns n's children, parsing a file's symbols on first use.
// A ref node's children are its FAR END(s) — that is how a chain
// continues through a named gate ("::out.call > #'B'").
func (e *engine) kids(n *treeNode) []*treeNode {
	if n.class == "ref" {
		return n.refFar
	}
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
	implied bool   // the universal was IMPLIED (nothing written)
	selfRef bool   // `&` — the node under test itself (relative lists only)
	class   string // type selector (func), if !anyType
	root    bool   // :root — matches the single .project node

	// isRef marks an `::in` / `::out` pseudo-element compound — a
	// reified reference edge, named by its DIRECTION. Ref nodes are
	// generated: `*` never matches them and walks never enter them —
	// only naming the element does (the CSS pseudo-element contract).
	// refClasses are the validated KIND classes (.call/.type/.import);
	// omitting the kind matches every kind, classified or not.
	isRef      bool
	refDir     string // "in" | "out"
	refClasses []string

	// langClass scopes a real-node compound to one language: file.go,
	// func.ts. Languages are a closed vocabulary (the registry), which
	// is what makes a class selector safe here.
	langClass string

	attrs []selAttr

	// ordSel picks from the compound's matches PER ANCHOR in document
	// order — :first / :last (jQuery-level selection, scoped to the
	// position's local candidate set).
	ordSel int // 0 none, 1 first, 2 last

	// pseudos hold the compound's semantic pseudo-classes IN WRITTEN
	// ORDER. Order matters because :parents MOVES the tip: filters
	// before a move test the pre-move set, filters after it test the
	// upstream set.
	pseudos []selPseudo

	// positionClaims are BARE :any/:all/:empty with no open :parents
	// excursion: they judge the ARRIVAL SET at this chain position and
	// decide the enclosing :where/:any(...) subject. Terminal, and only
	// legal inside a relative list.
	positionClaims []selPseudoKind
}

// selElem is one chain element — a compound or a parenthesized group —
// with regex-style repetition. Instances repeat CHILD-joined
// (b{2} = b > b); a ref element repeats by edge HOPS (each hop crosses
// the ref to its far end and takes the far end's next matching ref),
// which is the (::out > *){k} > ::out expansion — the element always
// ends AT a ref, so a following '>' is the far end.
type selElem struct {
	comb     selComb // relation of the FIRST instance to the previous element
	comp     *selCompound
	group    *selComplex // parenthesized sub-chain; comp == nil
	min, max int         // {m,n}; max < 0 = unbounded; default {1,1}
}

// hasMove reports whether the compound re-roots (carries a move).
func (c *selCompound) hasMove() bool {
	for _, ps := range c.pseudos {
		if ps.kind.isMove() {
			return true
		}
	}
	return false
}

type selPseudoKind int

const (
	pseudoContains selPseudoKind = iota // :contains('text') — filter on own source
	pseudoParents                       // :parents[(sel)]   — MOVE upstream (the one inverse)
	pseudoWhere                         // :where(sel)       — filter: a path matches
	pseudoAny                           // :any[(sel)]       — ∃ claim
	pseudoAll                           // :all[(sel)]       — ∀ claim
	pseudoEmpty                         // :empty[(sel)]     — ∄ claim
)

func (k selPseudoKind) isMove() bool  { return k == pseudoParents }
func (k selPseudoKind) isClaim() bool { return k == pseudoAny || k == pseudoAll || k == pseudoEmpty }

type selPseudo struct {
	kind  selPseudoKind
	grep  *grepSpec    // pseudoContains only
	inner selectorList // nil on a bare :parents (= :parents(*)) or a bare claim
}

// A compound's pseudo chain is a PIPELINE over the node it positionally
// matched: :parents opens an excursion (tips = everything upstream — the
// containment ancestors plus, transitively, the sources of incoming
// reference edges — filtered to the roots of the inner selector),
// parenthesized pseudos filter the current tips, and a BARE claim closes
// the excursion — it validates the excursion set and collapses back to
// the subject. `*:parents:empty` therefore matches only the workspace
// root: everything else has SOMETHING upstream.

type selComb int

const (
	selDescendant selComb = iota // whitespace → [1,∞]
	selChild                     // '>'        → [1,1]
)

// selRel is how a complex's LEFTMOST compound anchors to the node it is
// evaluated from. Top-level selectors anchor to the project self-or-
// below. Inside :where/:any/:all/:empty the selector is RELATIVE (as in
// CSS :has): descendants by default, children after a leading '>', or
// the anchor node ITSELF when the complex starts with `&` — CSS
// nesting's self reference, spelled out rather than inferred
// (`:where(&:parents:empty)`).
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

// selComplex is a chain of elements; the LAST element is the subject.
type selComplex struct {
	elems []selElem
	rel   selRel
}

// subjectComp returns the complex's subject compound (a group's subject
// is its own last element's, recursively), or nil for an empty complex.
func (cx *selComplex) subjectComp() *selCompound {
	if len(cx.elems) == 0 {
		return nil
	}
	e := &cx.elems[len(cx.elems)-1]
	if e.comp != nil {
		return e.comp
	}
	return e.group.subjectComp()
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
		if err := validateGlobalComplex(&cx); err != nil {
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

// validateGlobalComplex rejects relative-only constructs ('&', position
// claims) in a GLOBAL selector — the top level and :parents inners.
func validateGlobalComplex(cx *selComplex) error {
	for i := range cx.elems {
		el := &cx.elems[i]
		if el.group != nil {
			if err := validateGlobalComplex(el.group); err != nil {
				return err
			}
			continue
		}
		if el.comp.selfRef {
			return fmt.Errorf("'&' names the node under test, so it only makes sense inside :where/:any/:all/:empty (e.g. :where(&:parents:empty))")
		}
		if len(el.comp.positionClaims) > 0 {
			return fmt.Errorf("a bare claim judges a position inside :where/:any/:all/:empty — write func:where(::in:empty), or open a :parents excursion first")
		}
	}
	return nil
}

// parseComplex parses a chain of ELEMENTS: compounds, parenthesized
// groups, and ::in/::out pseudo-elements, each with optional {m,n}
// repetition. `X::out` binds the ref to X (child-tight); a standalone
// `::out` gets an implied universal host, so `X ::out` is a
// descendant's ref — exactly CSS's `#a::before` vs `#a ::before`.
func (p *modSelParser) parseComplex() (selComplex, error) {
	var cx selComplex
	firstElem := true
	for {
		comb := selDescendant
		if firstElem {
			// The complex-level anchoring (rel) covers the first element.
		} else {
			sawWS := p.skipWS()
			c := p.peek()
			if p.eof() || c == ',' || c == ')' {
				return cx, nil
			}
			if c == '>' {
				p.i++
				p.skipWS()
				comb = selChild
			} else if !sawWS {
				return cx, p.errf("a combinator, ',' or end of selector")
			}
		}

		// A claimed position is terminal: nothing may follow it.
		if n := len(cx.elems); n > 0 {
			if c := cx.elems[n-1].comp; c != nil && len(c.positionClaims) > 0 {
				return cx, fmt.Errorf("a bare claim closes its position — nothing can follow it in the chain")
			}
		}

		switch {
		case p.peek() == '(':
			// Group: a parenthesized sub-chain, usually repeated.
			p.i++
			sub, err := p.parseComplex()
			if err != nil {
				return cx, err
			}
			p.skipWS()
			if p.peek() != ')' {
				return cx, p.errf("')' to close the group")
			}
			p.i++
			el := selElem{comb: comb, group: &sub, min: 1, max: 1}
			if p.peek() == '{' {
				if el.min, el.max, err = p.parseBraceRange(); err != nil {
					return cx, err
				}
			}
			cx.elems = append(cx.elems, el)
		case p.peekIsPseudoElement():
			// Standalone ::in/::out — implied universal host.
			host := selCompound{anyType: true, implied: true}
			cx.elems = append(cx.elems, selElem{comb: comb, comp: &host, min: 1, max: 1})
			if err := p.appendRefElem(&cx); err != nil {
				return cx, err
			}
		default:
			comp, err := p.parseCompound()
			if err != nil {
				return cx, err
			}
			el := selElem{comb: comb, comp: &comp, min: 1, max: 1}
			if p.peek() == '{' {
				if el.min, el.max, err = p.parseBraceRange(); err != nil {
					return cx, err
				}
			}
			cx.elems = append(cx.elems, el)
			// A tight `X::out` — the ref binds to this compound.
			if p.peekIsPseudoElement() {
				if err := p.appendRefElem(&cx); err != nil {
					return cx, err
				}
			}
		}
		firstElem = false
	}
}

func (p *modSelParser) peekIsPseudoElement() bool {
	return p.peek() == ':' && p.i+1 < len(p.s) && p.s[p.i+1] == ':'
}

// appendRefElem parses one `::in`/`::out` pseudo-element (plus its optional
// {m,n} hop range) and appends it CHILD-joined to the last element —
// the host. Repetition of a ref element counts edges crossed.
func (p *modSelParser) appendRefElem(cx *selComplex) error {
	comp, err := p.parsePseudoElement()
	if err != nil {
		return err
	}
	el := selElem{comb: selChild, comp: &comp, min: 1, max: 1}
	if p.peek() == '{' {
		if el.min, el.max, err = p.parseBraceRange(); err != nil {
			return err
		}
		if el.min < 1 {
			return fmt.Errorf("::%s{%d,…}: edge hops start at 1 (0 edges crossed is the host itself — drop the element)", comp.refDir, el.min)
		}
	}
	cx.elems = append(cx.elems, el)
	return nil
}

// parsePseudoElement parses "::in" / "::out" plus its kind classes and
// the usual ids/attrs/pseudos. Ref nodes are the reified reference
// edges — see selCompound.isRef.
func (p *modSelParser) parsePseudoElement() (selCompound, error) {
	var comp selCompound
	p.i += 2 // '::'
	name := p.readIdent()
	switch name {
	case "in", "out":
		comp.refDir = name
	case "ref":
		return comp, fmt.Errorf("the reference elements are named by DIRECTION: ::in (who points here) and ::out (what this points at) — e.g. ::out.call, ::in.type")
	default:
		return comp, fmt.Errorf("unknown pseudo-element ::%s — the reference edges are ::in and ::out (kind as class: ::out.call, ::in.type, ::out.import)", name)
	}
	comp.isRef = true
	comp.class = "ref"
	for {
		switch p.peek() {
		case '.':
			p.i++
			cls := p.readIdent()
			if cls == "in" || cls == "out" {
				return comp, fmt.Errorf("direction IS the element (::%s); the class is the KIND: ::%s.call, ::%s.type, ::%s.import", cls, comp.refDir, comp.refDir, comp.refDir)
			}
			if !validRefClass(cls) {
				return comp, fmt.Errorf("::%s.%s: reference kinds are .call/.type/.import; omit the kind to match all (unclassified references match only the bare element)", comp.refDir, cls)
			}
			comp.refClasses = append(comp.refClasses, cls)
		case '#':
			p.i++
			id, err := p.readID()
			if err != nil {
				return comp, err
			}
			comp.attrs = append(comp.attrs, selAttr{op: selExact, value: id})
		case '[':
			a, err := p.parseAttr()
			if err != nil {
				return comp, err
			}
			comp.attrs = append(comp.attrs, a)
		case ':':
			if p.peekIsPseudoElement() {
				return comp, fmt.Errorf("an ::in/::out has no pseudo-elements of its own; continue the chain instead (::out.call > #'target')")
			}
			if err := p.parsePseudo(&comp); err != nil {
				return comp, err
			}
		default:
			return comp, nil
		}
	}
}

func validRefClass(c string) bool {
	switch c {
	case "call", "type", "import":
		return true
	}
	return false
}

// languageClassAliases maps class spellings to registry language names.
// Short forms are the ones people (and models) actually write.
var languageClassAliases = map[string]string{
	"go": "go", "typescript": "typescript", "ts": "typescript", "tsx": "typescript",
	"python": "python", "py": "python", "markdown": "markdown", "md": "markdown",
	"yaml": "yaml", "json": "json", "sql": "sql", "proto": "proto",
	"graphql": "graphql", "gql": "graphql",
}

func knownLanguageClass(c string) bool { _, ok := languageClassAliases[c]; return ok }

func knownLanguageClasses() []string {
	out := make([]string, 0, len(languageClassAliases))
	for c := range languageClassAliases {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func (p *modSelParser) parseCompound() (selCompound, error) {
	var comp selCompound
	sawType := false
	switch {
	case p.peek() == '&':
		// CSS nesting's self reference: the node under test itself.
		// Only meaningful inside a relative list (validated there).
		p.i++
		comp.anyType = true
		comp.selfRef = true
		sawType = true
	case p.peek() == '*':
		p.i++
		comp.anyType = true
		sawType = true
	case p.peek() == '.':
		// A leading dot is either the OLD `.func` spelling for a known
		// type (accepted: a CSS prior beats a schema line, measured) or
		// a language class on the implied universal (`.go` = any Go
		// node). Workspace NAMES are neither — the guided error routes
		// them to #ids.
		p.i++
		name := p.readIdent()
		switch {
		case knownSelectorClass(name):
			comp.class = name
		case knownLanguageClass(name):
			comp.anyType = true
			comp.langClass = name
		default:
			return comp, dotIsNotAClassErr(name)
		}
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
		if p.peekIsPseudoElement() {
			// `X::out` — the caller (parseComplex) owns the pseudo-element.
			return comp, nil
		}
		switch p.peek() {
		case '.':
			// After a tag: the LANGUAGE class — file.go, func.ts. A
			// closed vocabulary (the registry), unlike workspace names.
			p.i++
			cls := p.readIdent()
			if !knownLanguageClass(cls) {
				return comp, fmt.Errorf("no language %q: a class after a tag scopes it to a language (file.go, func.ts — one of %s). A workspace NAME is an id: #%s", cls, strings.Join(knownLanguageClasses(), " "), cls)
			}
			if comp.langClass != "" {
				return comp, fmt.Errorf("only one language class per compound")
			}
			comp.langClass = cls
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
				len(comp.pseudos) == 0 && len(comp.positionClaims) == 0 {
				return comp, p.errf("a type tag ('func'), '*', '#id', '[name…]' or a pseudo-class")
			}
			return comp, nil
		}
	}
}

// pseudoOnly reports whether nothing positional was written (no tag, no
// '*', no '&', no id/attr) — only pseudo-classes. As the first compound
// of a RELATIVE selector this gets an implicit '&' (the CSS nesting
// rule: a nested selector starting with a pseudo attaches to &).
func (c *selCompound) pseudoOnly() bool {
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
		// The one inverse move: everything upstream (containment
		// ancestors ∪ sources of incoming references, transitively),
		// filtered to the ROOTS of the inner selector. Bare = :parents(*).
		ps := selPseudo{kind: pseudoParents}
		if p.peek() == '(' {
			inner, err := p.parsePseudoArg(name, false)
			if err != nil {
				return err
			}
			ps.inner = inner
		}
		if p.peek() == '{' {
			return fmt.Errorf(":parents is always transitive (the whole upstream); bound reference hops on the edge instead: ::in.call{1,3}")
		}
		comp.pseudos = append(comp.pseudos, ps)
		return nil
	case "first", "last":
		if comp.ordSel != 0 {
			return fmt.Errorf("only one of :first/:last per compound")
		}
		if name == "first" {
			comp.ordSel = 1
		} else {
			comp.ordSel = 2
		}
		return nil
	case "where":
		inner, err := p.parsePseudoArg(name, true)
		if err != nil {
			return err
		}
		comp.pseudos = append(comp.pseudos, selPseudo{kind: pseudoWhere, inner: inner})
		return nil
	case "any", "all", "empty":
		kind := map[string]selPseudoKind{"any": pseudoAny, "all": pseudoAll, "empty": pseudoEmpty}[name]
		if p.peek() == '(' {
			inner, err := p.parsePseudoArg(name, true)
			if err != nil {
				return err
			}
			comp.pseudos = append(comp.pseudos, selPseudo{kind: kind, inner: inner})
			return nil
		}
		// BARE claim. With an open :parents excursion it closes it;
		// otherwise it is a POSITION claim — it judges the arrival set
		// at this chain position (validated relative-only in parseList).
		if hasOpenMove(comp.pseudos) {
			comp.pseudos = append(comp.pseudos, selPseudo{kind: kind})
			return nil
		}
		comp.positionClaims = append(comp.positionClaims, kind)
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
		return fmt.Errorf(":depth is gone — {m,n} REPEATS (regex semantics): func{2} = func > func; 'within 1..3 levels' = \"> *{0,2} > func\". Pass selector \"?\" for the grammar.")
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
// RELATIVE to the node under test. The start is ASSUMED to be '&' — the
// CSS nesting rule, so a CSS prior is correct: a leading pseudo attaches
// to the node itself (:where(:parents:empty) ≡ :where(&:parents:empty)),
// a leading type/*/#id means a descendant (& func), a leading '>' a
// child. The one exception is :root, which re-anchors at the workspace
// root. An explicit '&' is always allowed and always means the node
// under test.
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
		if len(cx.elems) == 0 {
			return nil, p.errf("a selector")
		}
		for i := 1; i < len(cx.elems); i++ {
			if c := cx.elems[i].comp; c != nil && c.selfRef {
				return nil, fmt.Errorf("'&' can only START a selector inside :where/:any/:all/:empty — it names the node under test")
			}
		}
		switch first := cx.elems[0].comp; {
		case first == nil: // a group leads — plain relative anchoring
		case first.selfRef:
			if rel == relChild {
				return nil, fmt.Errorf("'> &' contradicts itself: '&' IS the node under test; drop the '>'")
			}
			rel = relScope
		case first.root:
			rel = relTop // :root re-anchors at the workspace root
		case rel == relDescendant && first.pseudoOnly() && !first.isRef:
			rel = relScope // implicit &: a leading pseudo attaches to the node itself
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

// parseBraceRange parses "{m}", "{m,}" or "{m,n}" — the ONE range
// syntax: REPETITION of the element it follows (regex semantics).
// Instances chain child-joined; on an ::in/::out element it counts edges
// crossed.
func (p *modSelParser) parseBraceRange() (lo, hi int, err error) {
	p.i++ // '{'
	p.skipWS()
	lo, ok := p.readNonNegInt()
	if !ok {
		return 0, 0, p.errf("a count, e.g. {1,} or {0,3}")
	}
	hi = lo
	p.skipWS()
	if p.peek() == ',' {
		p.i++
		p.skipWS()
		if p.peek() == '}' {
			hi = -1 // {m,} — unbounded
		} else {
			hi, ok = p.readNonNegInt()
			if !ok {
				return 0, 0, p.errf("a max count or '}' (as in {1,} = unbounded)")
			}
			if hi < lo {
				return 0, 0, fmt.Errorf("{%d,%d}: max must be >= min", lo, hi)
			}
			p.skipWS()
		}
	}
	if p.peek() != '}' {
		return 0, 0, p.errf("'}' to close the {m,n} range")
	}
	p.i++
	return lo, hi, nil
}

// removedPseudoErr answers the retired pseudos with their modern
// spelling — terse, naming the fix (see unknownTypeErr for why).
func removedPseudoErr(name string) error {
	switch name {
	case "has":
		return fmt.Errorf(":has is now :any — file:any(func#test) = files with such a descendant. :all/:empty quantify the same way. Pass selector \"?\" for the grammar.")
	case "references":
		return fmt.Errorf(":references is now a NODE — a reified edge: X's outgoing calls are \"#'X'::out.call\", its callers \"#'X'::in.call > *\". Pass selector \"?\" for the grammar.")
	}
	return fmt.Errorf(":has_parent is gone: write the ancestor BEFORE the node — func:has_parent(#'a.ts') is now \"#'a.ts' func\". Pass selector \"?\" for the grammar.")
}

// hasOpenMove reports whether the pseudo chain has a move that a bare
// claim has not yet consumed — i.e. an excursion is open at this point.
func hasOpenMove(pseudos []selPseudo) bool {
	open := false
	for i := range pseudos {
		switch {
		case pseudos[i].kind.isMove():
			open = true
		case pseudos[i].kind.isClaim() && pseudos[i].inner == nil:
			open = false
		}
	}
	return open
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

const selectorGrammarHelp = `Selector grammar (CSS over the unified node tree; references are pseudo-element NODES).

A node's TYPE is a bare tag, like CSS's div/span — a fixed set you cannot
invent. Anything the workspace NAMES — a dir, a file, a symbol — is an #id. So
the cache/ directory is #cache, never "cache". A class after a tag scopes it to
a LANGUAGE: file.go, func.ts. The reference graph is reified as generated edge
nodes (::in / ::out): '*' never matches one and walks never enter one — you
cross an edge only by naming it.

  TYPES  project dir file
         func method type struct interface class const var field enum ctor module import argument
         * = any type (never an edge).
  TREE   project#<root-name> > dir#<relpath> > file#<relpath> > <symbols, dotted-nested> > argument#<param>
         :root is a pseudo-class matching the single project node (as CSS's :root matches <html>).
  ID     #bare  (charset [A-Za-z_][A-Za-z0-9_.-]*)  or  #'anything else'  (spaces, slashes, …). Quote instead of escaping; there is no backslash escape.
         A symbol answers to its leaf name, its dotted path, and its full "<file>#<sym>". An EDGE answers to its far end's ids.
  ATTR   [name=X] exact  [name^=X] prefix  [name$=X] suffix  [name*=X] contains.  #id is sugar for [name=id].
  EDGES  ::in = references pointing HERE.  ::out = what this node's own body points at.  Kind as class:
         ::out.call  ::in.type  ::out.import  — bare matches every kind (incl. unclassified).
         Attached to the INNERMOST enclosing symbol: X::out = X's own edges (as CSS #a::before),
         X ::out = nested symbols' too (as #a ::before). The far end is the edge's only child:
         #'A'::out.call > #'B' = A calls B. An edge's address is its SITE ("file@line") — node_read /
         node_edit touch the call site. ::out.call{1,16} = crossing 1..16 call edges; {1,} = transitive.
         Qualified refs to EXTERNAL packages resolve to the file's IMPORT node (named for the package —
         alias honored, /vN skipped): import#huma::in.call = every huma call in that file.
  MOVE   :parents(sel) — tip := the ROOTS of sel with a path down/out to the tip: everything upstream,
         containment ancestors ∪ incoming-reference sources, transitive. Bare = :parents(*).
         *:parents:empty = only :root — everything else has something upstream.
  CLAIM  a BARE :any / :all / :empty judges the set at its position, deciding the node under test:
         func:where(::in:empty) = dead code. After :parents it closes the excursion (*:parents:empty).
         Parenthesized :where/:any/:all/:empty(sel) take a RELATIVE selector (CSS nesting): a leading
         pseudo/::element binds to the node itself; a leading tag/*/#id means a descendant
         (file:any(func#test)); '>' = child; :root re-anchors; an explicit & is the node under test.
  ORDER  :first / :last — pick from this position's matches, per anchor, in document order.
  REP    {m,n} REPEATS an element or (group), child-joined: func{2} = func > func; (a b){2} = a b > a b.
         {m,} = unbounded, cycle-safe. Zero reps make the element vanish: "> *{0,2} > b" = b within 1..3 levels.
  COMB   space = descendant   '>' = direct child
  COMMA  union: "func, method"
Examples: #cache > file = files directly in cache/  |  :root > * = ONLY top level  |  file.go = Go files
          #'store.go#Save'::in.call = the calls to Save (rows carry from:)  |  ::in.call{1,} > * = every transitive caller
          #'main'::out.call > * = what main calls  |  func:where(::out.call:empty) = calls nothing
          method:where(::out #'parseAttr') = methods whose body mentions it  |  file > func:first = each file's first func`

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

// nodeLess is the document order: pre-order over (fileOrd, symOrd),
// with the site line + direction breaking ties among a host's ref nodes.
func nodeLess(a, b *treeNode) bool {
	if a.fileOrd != b.fileOrd {
		return a.fileOrd < b.fileOrd
	}
	if a.symOrd != b.symOrd {
		return a.symOrd < b.symOrd
	}
	if a.at[0] != b.at[0] {
		return a.at[0] < b.at[0]
	}
	if a.refDir != b.refDir {
		return a.refDir < b.refDir
	}
	return a.leaf < b.leaf
}

// evaluate runs a parsed selector over the tree and returns the
// matching nodes in deterministic pre-order.
func (e *engine) evaluate(list selectorList) []*treeNode {
	set := e.evalList(list, e.project, false)
	out := make([]*treeNode, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return nodeLess(out[i], out[j]) })
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
	if len(cx.elems) == 0 {
		return nil
	}
	if cx.rel == relTop {
		// relTop complexes are GLOBAL: top-level selectors, :parents
		// inners, and :root-led complexes inside relative lists (":root
		// re-anchors" is the one exception to the assumed '&' start).
		anchor = e.project
	}
	min0, max0 := cx.rel.rng()
	start := map[*treeNode]bool{anchor: true}
	tips := e.evalElems(cx.elems, start, min0, max0, relaxSubject)

	// Position claims on the subject: judge the arrival set, collapse to
	// the anchor (they decide the enclosing :where/:any subject).
	if sub := cx.subjectComp(); sub != nil && len(sub.positionClaims) > 0 {
		if relaxSubject {
			return tips // claims are the tested property, not the domain
		}
		for _, k := range sub.positionClaims {
			ok := false
			switch k {
			case pseudoAny:
				ok = len(tips) > 0
			case pseudoEmpty:
				ok = len(tips) == 0
			case pseudoAll:
				// ∀ at a position: everything the structure reaches also
				// matches as written — the relaxed-domain compare.
				domain := e.evalElems(cx.elems, start, min0, max0, true)
				ok = setsEqual(tips, domain)
			}
			if !ok {
				return nil
			}
		}
		return map[*treeNode]bool{anchor: true}
	}
	return tips
}

// evalElems runs an element chain from a tip set. min0/max0 anchor the
// FIRST element's first instance (the complex's rel, or child for group
// repetitions); every other instance joins as written (its combinator),
// with repeated instances child-joined. A {0,…} element simply vanishes
// on the skip path — its combinator vanishes with it.
func (e *engine) evalElems(elems []selElem, tips map[*treeNode]bool, min0, max0 int, relaxSubject bool) map[*treeNode]bool {
	last := len(elems) - 1
	for i := range elems {
		el := &elems[i]
		relaxed := relaxSubject && i == last
		imin, imax := min0, max0
		if i > 0 {
			imin, imax = combRange(el.comb)
		}
		next := e.evalRepeat(tips, el, imin, imax, relaxed)
		if el.min == 0 {
			for t := range tips {
				next[t] = true // the skip path: the element vanishes
			}
		}
		tips = next
		if len(tips) == 0 {
			return tips
		}
	}
	return tips
}

// evalRepeat evaluates one element's {m,n} repetition. Instance 1 joins
// via [min1,max1]; instances 2..n join CHILD (each a direct child of
// the previous — the regex-over-child-steps reading), except an ::in/::out
// element, which repeats by edge HOPS: cross the ref to its far end and
// take the far end's next matching ref, so the element always ends AT a
// ref. Unbounded {m,} is a fixpoint — the visited set bounds cycles.
func (e *engine) evalRepeat(tips map[*treeNode]bool, el *selElem, min1, max1 int, relaxed bool) map[*treeNode]bool {
	out := map[*treeNode]bool{}
	if el.max == 0 {
		return out // {0}: only the skip path exists (the element vanishes)
	}
	frontier := e.evalInstance(tips, el, min1, max1, relaxed)
	if el.min <= 1 {
		for n := range frontier {
			out[n] = true
		}
	}
	var visited map[*treeNode]bool
	if el.max < 0 {
		visited = map[*treeNode]bool{}
		for n := range frontier {
			visited[n] = true
		}
	}
	for count := 2; (el.max < 0 || count <= el.max) && len(frontier) > 0; count++ {
		var next map[*treeNode]bool
		if el.comp != nil && el.comp.isRef {
			next = e.refHop(frontier, el.comp, relaxed)
		} else {
			next = e.evalInstance(frontier, el, 1, 1, relaxed)
		}
		if visited != nil {
			pruned := map[*treeNode]bool{}
			for n := range next {
				if !visited[n] {
					visited[n] = true
					pruned[n] = true
				}
			}
			next = pruned
		}
		if count >= el.min {
			for n := range next {
				out[n] = true
			}
		}
		frontier = next
	}
	return out
}

// evalInstance evaluates ONE instance of an element from each tip.
func (e *engine) evalInstance(tips map[*treeNode]bool, el *selElem, min, max int, relaxed bool) map[*treeNode]bool {
	if el.group != nil {
		out := map[*treeNode]bool{}
		for t := range tips {
			for n := range e.evalElems(el.group.elems, map[*treeNode]bool{t: true}, min, max, relaxed) {
				out[n] = true
			}
		}
		return out
	}
	comp := el.comp
	if comp.isRef {
		return e.refMatches(tips, comp, relaxed)
	}
	out := map[*treeNode]bool{}
	for t := range tips {
		// A subject with a :parents move keeps its positional part even
		// under relaxation — the relaxation moves into the move's inner.
		cand := e.collectMatches(t, min, max, comp, relaxed && !comp.hasMove())
		for n := range e.selectOrdered(cand, comp, relaxed) {
			out[n] = true
		}
	}
	return out
}

// selectOrdered applies the compound's pipeline pseudos and the
// :first/:last selection to one anchor's candidate set. Selection is
// per-anchor in document order — jQuery-level, scoped to the position.
func (e *engine) selectOrdered(cand map[*treeNode]bool, comp *selCompound, relaxed bool) map[*treeNode]bool {
	if comp.ordSel == 0 || relaxed {
		return e.applyPseudos(cand, comp, relaxed)
	}
	nodes := make([]*treeNode, 0, len(cand))
	for n := range cand {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodeLess(nodes[i], nodes[j]) })
	if comp.ordSel == 2 {
		for i, j := 0, len(nodes)-1; i < j; i, j = i+1, j-1 {
			nodes[i], nodes[j] = nodes[j], nodes[i]
		}
	}
	for _, n := range nodes {
		if got := e.applyPseudos(map[*treeNode]bool{n: true}, comp, relaxed); len(got) > 0 {
			return got
		}
	}
	return nil
}

// refMatches returns the matching ref children of each host tip. Under
// ∀-relaxation the DIRECTION classes still apply — which edge set you
// asked about is structure; the kind and ids are the tested property.
func (e *engine) refMatches(hosts map[*treeNode]bool, comp *selCompound, relaxed bool) map[*treeNode]bool {
	out := map[*treeNode]bool{}
	for h := range hosts {
		cand := map[*treeNode]bool{}
		for _, r := range e.refNodes(h) {
			if relaxed {
				if refDirMatch(r, comp) {
					cand[r] = true
				}
			} else if e.positionalMatch(r, comp) {
				cand[r] = true
			}
		}
		for n := range e.selectOrdered(cand, comp, relaxed) {
			out[n] = true
		}
	}
	return out
}

func refDirMatch(r *treeNode, comp *selCompound) bool {
	return r.refDir == comp.refDir
}

// refHop crosses each ref to its far end(s) and takes the far ends'
// next matching refs — one edge hop of a repeated ::in/::out element.
func (e *engine) refHop(refs map[*treeNode]bool, comp *selCompound, relaxed bool) map[*treeNode]bool {
	fars := map[*treeNode]bool{}
	for r := range refs {
		for _, f := range r.refFar {
			fars[f] = true
		}
	}
	return e.refMatches(fars, comp, relaxed)
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
	if comp.isRef {
		if n.class != "ref" || n.refDir != comp.refDir {
			return false
		}
		for _, c := range comp.refClasses {
			if n.refKind != c {
				return false
			}
		}
	} else {
		// Generated ref nodes are pseudo-elements: `*` (and every real
		// tag) never matches one — only ::in/::out does.
		if n.class == "ref" {
			return false
		}
		if !comp.anyType && n.class != comp.class {
			return false
		}
		if comp.langClass != "" &&
			e.s.languageForFile(n.abs) != languageClassAliases[comp.langClass] {
			return false
		}
	}
	for _, a := range comp.attrs {
		if !matchSelAttr(n, a) {
			return false
		}
	}
	return true
}

// applyPseudos runs each node through the compound's pseudo PIPELINE
// (see selPseudo). Per-subject, because a bare claim collapses back to
// the SUBJECT that opened the excursion — `func:parents:empty` keeps
// the func, not the (empty) referrer set.
func (e *engine) applyPseudos(set map[*treeNode]bool, comp *selCompound, relaxed bool) map[*treeNode]bool {
	if len(comp.pseudos) == 0 {
		return set
	}
	out := map[*treeNode]bool{}
	for n := range set {
		for r := range e.runPipeline(n, comp.pseudos, relaxed) {
			out[r] = true
		}
	}
	return out
}

// runPipeline evaluates one compound's pseudo chain IN WRITTEN ORDER
// over one positionally-matched subject:
//
//   - :parents opens an excursion: the tips become everything UPSTREAM
//     (containment ancestors ∪ sources of incoming references,
//     transitively), narrowed to the ROOTS of its inner selector;
//   - a parenthesized pseudo filters the CURRENT tips (before the move
//     that's the subject, after it the upstream set);
//   - a BARE claim (:any/:all/:empty) closes the excursion — it decides
//     by the tip set and collapses back to the subject. Bare :all
//     compares against the DOMAIN: the same excursion with the inner
//     relaxed (∀ = nothing upstream fails the inner's tests).
//
// relaxed is the ∀-domain pass of an ENCLOSING parenthesized :all: the
// subject's filters and claims are the property under test, so they are
// dropped, and the last move's inner is relaxed (see evalList).
func (e *engine) runPipeline(subject *treeNode, pseudos []selPseudo, relaxed bool) map[*treeNode]bool {
	lastMove, needDomain := -1, false
	for i := range pseudos {
		if pseudos[i].kind.isMove() {
			lastMove = i
		}
		if pseudos[i].kind == pseudoAll && pseudos[i].inner == nil {
			needDomain = true
		}
	}
	tips := map[*treeNode]bool{subject: true}
	var domain map[*treeNode]bool
	for i := range pseudos {
		ps := &pseudos[i]
		switch {
		case ps.kind.isMove():
			if needDomain {
				if domain == nil {
					domain = map[*treeNode]bool{subject: true}
				}
				domain = e.parentsMove(domain, ps, true)
			}
			tips = e.parentsMove(tips, ps, relaxed && i == lastMove)
		case ps.kind.isClaim() && ps.inner == nil:
			if relaxed {
				continue
			}
			ok := false
			switch ps.kind {
			case pseudoAny:
				ok = len(tips) > 0
			case pseudoEmpty:
				ok = len(tips) == 0
			case pseudoAll:
				ok = setsEqual(tips, domain)
			}
			if !ok {
				return nil
			}
			tips = map[*treeNode]bool{subject: true}
			domain = nil
		default: // :contains and the parenthesized set pseudos — per-tip filters
			if relaxed {
				continue
			}
			next := map[*treeNode]bool{}
			for t := range tips {
				if e.pseudoHolds(t, ps) {
					next[t] = true
				}
			}
			tips = next
		}
	}
	return tips
}

func setsEqual(a, b map[*treeNode]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for n := range a {
		if !b[n] {
			return false
		}
	}
	return true
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

// moveEdges is THE move: tip := the nodes one reference edge away —
// backward for :parents (who points at the tip), forward for
// :references (what the tip points at) — filtered by the inner selector
// when one was written (bare = unfiltered). Repeated min..max hops;
// every hop must match the inner (that is what "through" means — the
// intermediates are named, hence constrained).
// parentsMove is the ONE inverse move. The upstream of the current tips
// — containment ancestors ∪ sources of incoming references, computed as
// a transitive fixpoint (the visited set bounds cycles) — is narrowed to
// the ROOTS of the inner selector: nodes matching the inner's first
// element whose chain-subject lies upstream. `*:parents:empty` is
// therefore only ever the workspace root.
func (e *engine) parentsMove(tips map[*treeNode]bool, ps *selPseudo, relaxInner bool) map[*treeNode]bool {
	up := e.upstream(tips)
	if ps.inner == nil {
		return up
	}
	out := map[*treeNode]bool{}
	for _, cx := range ps.inner {
		if len(cx.elems) == 0 {
			continue
		}
		min0, max0 := cx.rel.rng()
		start := map[*treeNode]bool{e.project: true}
		roots := e.evalRepeat(start, &cx.elems[0], min0, max0, relaxInner && len(cx.elems) == 1)
		for r := range roots {
			if len(cx.elems) == 1 {
				if up[r] {
					out[r] = true
				}
				continue
			}
			cm, cM := combRange(cx.elems[1].comb)
			subs := e.evalElems(cx.elems[1:], map[*treeNode]bool{r: true}, cm, cM, relaxInner)
			for s := range subs {
				if up[s] {
					out[r] = true
					break
				}
			}
		}
	}
	return out
}

// upstream is the transitive closure of "who is above / who points
// here": the containment parent plus every source of an incoming
// reference edge. Recursive nodes ARE their own upstream (Walk calls
// Walk); the visited set keeps cycles finite.
func (e *engine) upstream(tips map[*treeNode]bool) map[*treeNode]bool {
	out := map[*treeNode]bool{}
	frontier := tips
	for len(frontier) > 0 {
		next := map[*treeNode]bool{}
		add := func(n *treeNode) {
			if n != nil && !out[n] {
				out[n] = true
				next[n] = true
			}
		}
		for n := range frontier {
			add(n.parent)
			for _, r := range e.refNodes(n) {
				if r.refDir == "in" {
					for _, src := range r.refFar {
						add(src)
					}
				}
			}
		}
		frontier = next
	}
	return out
}

// --------------------------------------------------------- ref nodes
//
// A reference is a NODE: tag ref, direction + kind as its class set, id
// = the far end, span = the site. Each edge appears twice — ::out
// under the innermost symbol enclosing the site (the source), ::in
// under the target — and the far end is the node's only child. Edges
// ride the lexical index, so they are NAME-keyed (same-named symbols
// share edges) and carry refConf "lexical"; a child-LSP precision pass
// can upgrade individual edges to "lsp" later without reshaping.

// refSite is one classified, non-declaration occurrence of a name.
type refSite struct {
	name      string
	line, col int
	kind      string // "call" | "type" | "import" | ""
	encl      string // dotted sym path of the innermost enclosing symbol
}

// refNodes materializes (once) and returns n's generated ref children.
func (e *engine) refNodes(n *treeNode) []*treeNode {
	switch n.class {
	case "project", "dir", "ref":
		return nil
	}
	if !n.refsLoaded {
		n.refsLoaded = true
		e.buildRefs(n)
	}
	return n.refs
}

func (e *engine) buildRefs(n *treeNode) {
	// Outgoing: sites in n's own file whose innermost enclosing symbol
	// is n itself — nesting attribution is the TREE SHAPE, so a closure's
	// calls belong to the closure, and `>` vs space picks inner vs outer.
	for _, site := range e.fileSites(n.file) {
		if site.encl != n.sym {
			continue
		}
		far := e.scopeDecls(e.declsOf(site.name), n.file)
		if len(far) == 0 {
			continue
		}
		n.refs = append(n.refs, &treeNode{
			class: "ref", refDir: "out", refKind: site.kind, refConf: "lexical",
			leaf: site.name, full: site.name,
			file: n.file, abs: n.abs, at: [2]int{site.line, site.line},
			parent: n, depth: n.depth + 1, refFar: far,
			fileOrd: n.fileOrd, symOrd: n.symOrd,
		})
	}
	// Incoming: every site of n's NAME elsewhere; the far end is the
	// site's innermost enclosing symbol (the source).
	if n.sym == "" {
		return // files/project have no indexed name to be targeted by
	}
	idx := e.s.getIndex()
	if idx == nil {
		return
	}
	for _, site := range idx.LookupExisting(n.leaf) {
		rel := relPath(site.File, e.s.getRoot())
		// An import is file-scoped by language semantics: it is only
		// ever the far end of sites in ITS OWN file. This is what makes
		// `import#huma::in.call` a per-file dependency-usage query
		// instead of name-keyed noise.
		if n.class == "import" && rel != n.file {
			continue
		}
		sites := e.fileSites(rel)
		var hit *refSite
		for i := range sites {
			if sites[i].name == n.leaf && sites[i].line == site.Line && sites[i].col == site.Col {
				hit = &sites[i]
				break
			}
		}
		if hit == nil {
			continue // the declaration itself, or an unindexed file
		}
		src := e.nodeByAddr(rel, hit.encl)
		if src == nil {
			continue
		}
		fnode := e.fileByRel[rel]
		n.refs = append(n.refs, &treeNode{
			class: "ref", refDir: "in", refKind: hit.kind, refConf: "lexical",
			leaf: n.leaf, full: n.leaf,
			file: rel, abs: fnode.abs, at: [2]int{hit.line, hit.line},
			parent: n, depth: n.depth + 1, refFar: []*treeNode{src},
			fileOrd: n.fileOrd, symOrd: n.symOrd,
		})
	}
}

// fileSites returns every classified non-declaration site in one file,
// memoized. Declarations are excluded here once, so neither direction
// ever counts a symbol's own name as an edge (LSP's includeDeclaration
// line: recursive Walk keeps its Walk(n-1) site, plain Save gets none).
func (e *engine) fileSites(rel string) []refSite {
	if e.siteCache == nil {
		e.siteCache = map[string][]refSite{}
	}
	if v, ok := e.siteCache[rel]; ok {
		return v
	}
	out := []refSite{}
	defer func() { e.siteCache[rel] = out }()
	idx := e.s.getIndex()
	fnode := e.fileByRel[rel]
	if idx == nil || fnode == nil {
		return out
	}
	if e.symCache == nil {
		e.symCache = map[string][]symbols.Symbol{}
	}
	for _, name := range idx.Names() {
		for _, site := range idx.LookupExisting(name) {
			if relPath(site.File, e.s.getRoot()) != rel {
				continue
			}
			if e.isDeclSite(site.File, site.Line, site.Col, name, e.symCache) {
				continue
			}
			out = append(out, refSite{
				name: name, line: site.Line, col: site.Col,
				encl: e.s.enclosingSymPath(site.File, site.Line, e.symCache),
			})
		}
	}
	// Classify all of the file's sites in ONE parse.
	if content, err := os.ReadFile(fnode.abs); err == nil {
		positions := make([][2]int, len(out))
		for i, s := range out {
			positions[i] = [2]int{s.line, s.col}
		}
		kinds := symbols.SiteKinds(e.s.languageForFile(fnode.abs), content, positions)
		for i := range out {
			out[i].kind = kinds[i]
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].line != out[j].line {
			return out[i].line < out[j].line
		}
		return out[i].col < out[j].col
	})
	return out
}

// scopeDecls drops import-class decls that live in OTHER files: an
// import is file-scoped, so `huma.Register(...)` resolves to THIS
// file's `import#huma`, never a sibling's. Non-import decls pass
// through untouched (still name-keyed across the workspace).
func (e *engine) scopeDecls(decls []*treeNode, file string) []*treeNode {
	out := decls[:0:0]
	for _, d := range decls {
		if d.class == "import" && d.file != file {
			continue
		}
		out = append(out, d)
	}
	return out
}

// declsOf returns every declared node answering to a name — the
// possible far ends of an outgoing edge. The full-tree walk parses
// every file once; graph queries are inherently whole-workspace.
func (e *engine) declsOf(name string) []*treeNode {
	if e.declsByName == nil {
		e.declsByName = map[string][]*treeNode{}
		var walk func(n *treeNode)
		walk = func(n *treeNode) {
			if n.sym != "" {
				e.declsByName[n.leaf] = append(e.declsByName[n.leaf], n)
			}
			for _, c := range e.kids(n) {
				walk(c)
			}
		}
		walk(e.project)
	}
	return e.declsByName[name]
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
