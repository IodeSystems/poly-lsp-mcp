package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

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
	class string // "project"|"dir"|"file"|<symbol class>|"argument"|"annotation"|"external"
	leaf  string
	full  string
	alias string // extra id (an annotation's own name, e.g. app.route)
	// domain gates ownership: "" = owned (workspace, rw). "external" = a
	// child-LSP resolved this far end OUTSIDE the git root (stdlib, a dep)
	// — a read-only STUB, nameable but [not indexed]. The North Star axis
	// that will later gate mutation/budget; today it just marks the boundary.
	domain    string
	commentAt [2]int // joined doc-comment span above this symbol (0 = none); ::comment reads it
	bodyAt    int    // line a callable's body begins (0 = none); splits ::signature | ::body
	nameAt    [2]int // [line, col] of the symbol's NAME — where an LSP is queried (.implements)
	genText   string // inline source of a generated ::signature/::body node (empty otherwise)

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
	refKind string      // "call" | "type" | "import" | ""  (the KIND axis)
	refPos  string      // "return" | "param" | "field" | "var" | ""  (the POSITION axis)
	refConf string      // refLexical (name-unique) | refLSP (child LSP settled it) | refUnsettled (ambiguous guess)
	refCol  int         // the SITE's column — kept so :recursive can re-resolve the site via the LSP
	refFar  []*treeNode // the far end(s) — >1 only under name collisions

	// The node's generated ref children, materialized lazily and kept OUT
	// of children so containment walks and `*` never see them.
	//
	// The two directions are built and cached SEPARATELY because they
	// cost wildly different amounts and no caller wants both. Outgoing
	// reads this node's OWN file. Incoming asks the index for every
	// occurrence of the node's NAME workspace-wide, so its cost scales
	// with how common the name is — measured at ~20-45k work units per
	// occurrence, where the whole default budget is 200k.
	//
	// Building both halves regardless of the question made ::out pay the
	// ::in bill: #New::out cost 1.78M work and returned the same 49
	// matches that 140k buys now. Direction is therefore REQUIRED to ask
	// for refs; there is no "both".
	refsOut       []*treeNode
	refsIn        []*treeNode
	refsOutLoaded bool
	refsInLoaded  bool

	// Fragment nodes (class == "fragment") — a matched line of the
	// host's own source, minted by ::grep. frag carries the match text
	// plus any -A/-B/-C context, clipped to the host's span.
	frag *grepHit
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
	case "ref", "fragment", "comment":
		return fmt.Sprintf("%s@%d", n.file, n.at[0])
	case "signature", "body":
		// A RANGE address so node_read/node_edit hit the whole span (these
		// are multi-line, unlike a ref/grep line).
		return fmt.Sprintf("%s@%d-%d", n.file, n.at[0], n.at[1])
	case "external":
		return n.full // module@version#sym — the external identity, not a workspace path
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
	case "fragment", "comment", "signature", "body":
		return nil // generated source region: no name to match by #id
	case "external":
		// The bare symbol (#Writer) and the full external identity
		// (#'io@go1.21#Writer'); no workspace address.
		return []string{n.leaf, n.full}
	}
	ids := []string{n.leaf, n.full, n.file + "#" + n.sym}
	if n.alias != "" {
		ids = append(ids, n.alias) // an annotation's own name (app.route)
	}
	return ids
}

// nameIDs is the `[name]` axis: what the node is CALLED. It is nodeIDs
// minus every id that is really a LOCATION — a symbol's "<file>#<sym>"
// address and a dir/file's workspace-relative path. Those moved to
// [path].
//
// Keeping locations here made [name] quietly answer path questions:
// func[name*=test] matched every func in a _test.go file (508 of them,
// via the address id) instead of the ~6 funcs actually named *test*.
// A filter that says `name` and means `name or path` has no spelling
// left for "named test" — hence two axes.
//
// #id is unaffected: it keeps matching addresses through nodeIDs.
func (n *treeNode) nameIDs() []string {
	switch n.class {
	case "project":
		return []string{n.full}
	case "dir", "file":
		return []string{n.leaf} // .full is the PATH — [path] owns it
	case "ref":
		var ids []string
		for _, f := range n.refFar {
			ids = append(ids, f.nameIDs()...)
		}
		return ids
	case "fragment":
		return nil
	}
	return []string{n.leaf, n.full} // leaf + dotted path; NOT the address
}

// nodePath is the `[path]` axis: where the node LIVES, workspace-
// relative. A symbol, edge or fragment answers with its FILE's path —
// that is where it lives. The project IS the root, so it has no path
// inside the workspace and matches no [path] filter.
func nodePath(n *treeNode) string {
	if n.class == "project" {
		return ""
	}
	return n.file
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

	// noPushdown disables the leading-ref cardinality pushdown. Only the
	// equivalence test sets it, to prove the fast path returns the same
	// nodes as the full scan.
	noPushdown bool

	// noPlan disables the descendant-chain reorder (planReorder). Only the
	// equivalence test sets it, to prove the reorder returns the same
	// nodes as the forward evaluation.
	noPlan bool

	// The child-LSP precision pass (see precision.go). lspLeft is the
	// per-query round-trip budget; asked/resolved report what the answer
	// is actually made of, so a partly-lexical graph says so instead of
	// passing itself off as resolved.
	lspLeft     int
	lspAsked    int
	lspResolved int

	// The LSP cap is set LAZILY on the first round-trip (ensureLSPCap): an
	// explicit server cap, else one TUNED from the workspace collision rate
	// (collAmbiguous of collTotal declared names). lspCapChosen records the
	// value for the legibility note; capTuned marks it as workspace-derived.
	lspCapReady   bool
	lspCapChosen  int
	collTotal     int
	collAmbiguous int
	capTuned      bool

	// Per-transitive-walk precision, for the hop-aware precisionNote: the
	// deepest hop any repeated ::in/::out reached, the shallowest hop that
	// carried an unsettled edge, and how many distinct unsettled edges the
	// walk crossed. hopCounted dedupes a ref node reached at more than one
	// hop (a bounded walk can revisit). Set only for genuinely repeated
	// edge elements ({m,}/{m>1}); a single hop leans on the aggregate line.
	maxHopReached    int
	unsettledFromHop int
	transUnsettled   int
	hopCounted       map[*treeNode]bool

	// recursiveUnconfirmed records that a :recursive test hit a self-edge
	// it could NOT confirm because no child LSP was reachable — so the
	// answer is under-resolved (a real recursion may be missed, since a
	// name-unique self-edge is trusted only once the LSP says it IS a
	// self-call). The result says so rather than reading as "none found".
	recursiveUnconfirmed bool

	// fragCache: minted ::grep fragments per host per pattern, so the
	// same host+pattern yields the SAME nodes across a query (set
	// semantics need identity).
	fragCache map[*treeNode]map[string][]*treeNode

	// commentCache: the generated ::comment node per host (see commentOf).
	commentCache map[*treeNode]*treeNode

	// genPartCache: the generated ::signature/::body node per host per part.
	genPartCache map[*treeNode]map[string]*treeNode

	// implRefCache: `.implements` edges per host per direction, built on
	// demand from the child LSP (textDocument/implementation). Separate
	// from the site-based refsIn/refsOut because implements has no lexical
	// site — it is a semantic relation only an LSP knows.
	implRefCache map[*treeNode]map[string][]*treeNode
	// implementsUnavailable records that a `.implements` query could not
	// reach a child LSP, so the answer is "unavailable", never "none".
	implementsUnavailable bool

	// selfSetCache: full-workspace match sets for :not/:is chain inners,
	// keyed by the inner AST's backing array.
	selfSetCache map[string]map[*treeNode]bool

	// The query-wide WORK budget: every node visited, edge crossed, and
	// site/line scanned spends one unit. A hop bound would guard the
	// wrong axis (breadth, not depth, is the cost; capping hops silently
	// under-reports — the grep-looked-complete failure). The budget
	// guards the real risk (hot-name fan-out on a big workspace) and
	// trips LOUDLY: partial results, flagged, with the repair recipe.
	workLeft     int
	workExceeded bool

	// deadline is a wall-clock budget (ms mode): spend() trips when the
	// clock passes it. Zero = no time limit (ops mode only). timedOut
	// records that the TIME limit tripped, not the work budget — the
	// distinction matters because a time-truncated result is NON-
	// deterministic (it stops wherever the clock lands), where the ops
	// budget truncates deterministically.
	deadline  time.Time
	timedOut  bool
	spendTick int // spend counter, so the clock is read every Nth spend not every one

	// Cost trace (always-on, ~free): costStack is the element chain under
	// evaluation. spend() increments the top FRAME's counter (a slice
	// index, no map), and evalElems flushes each frame to elemCost on pop
	// — so the hot path pays a single add, not a map write. blownElem is
	// the element on top when the budget tripped. A budget blow renders
	// the selector annotated with these, pointing at what ate the budget.
	costStack []costFrame
	elemCost  map[*selElem]int
	blownElem *selElem
}

// spend charges n work units; false means the budget is gone and the
// caller should stop expanding (results become partial + flagged).
// costFrame bills one element's evaluation. Its spent counter is flushed
// to elemCost when the frame pops (evalElems), so spend() only touches a
// slice index.
type costFrame struct {
	el    *selElem
	spent int
}

func (e *engine) spend(n int) bool {
	if len(e.costStack) > 0 {
		e.costStack[len(e.costStack)-1].spent += n
	}
	if e.workExceeded {
		return false
	}
	// Wall-clock budget (ms mode): read the clock every 256th spend — a
	// time check per work unit would dominate a fast query, and ms
	// granularity does not need finer sampling.
	if !e.deadline.IsZero() {
		e.spendTick++
		// Check on the first spend (an already-expired budget trips at
		// once) then every 256th — finer sampling would tax a fast query.
		if (e.spendTick == 1 || e.spendTick&0xff == 0) && time.Now().After(e.deadline) {
			e.workExceeded = true
			e.timedOut = true
			if len(e.costStack) > 0 {
				e.blownElem = e.costStack[len(e.costStack)-1].el
			}
			return false
		}
	}
	e.workLeft -= n
	if e.workLeft < 0 {
		e.workExceeded = true
		if len(e.costStack) > 0 {
			e.blownElem = e.costStack[len(e.costStack)-1].el
		}
		return false
	}
	return true
}

const defaultBudgetMs = 10_000 // omitted-budget default: 10s wall clock

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
			class:     sym.Class,
			leaf:      lastSeg(sym.Sym),
			full:      sym.Sym,
			alias:     sym.Alias,
			commentAt: [2]int{sym.CommentStartLine, sym.CommentEndLine},
			bodyAt:    sym.BodyStartLine,
			nameAt:    [2]int{sym.NameStartLine, sym.NameStartCol},
			file:      f.file,
			sym:       sym.Sym,
			at:        [2]int{sym.DeclStartLine, sym.DeclEndLine},
			parent:    parent,
			depth:     parent.depth + 1,
			abs:       f.abs,
			fileOrd:   f.fileOrd,
			symOrd:    i + 1,
		}
		bySym[sym.Sym] = n
		parent.children = append(parent.children, n)
	}
}

// nodeByAddr resolves a workspace-relative file path plus dotted sym
// path back to its tree node (sym "" = the file node itself). This is
// how a reference SITE re-enters the tree during a :parents move.
// nodeByAddr resolves a (file, sym-path) to its tree node, disambiguating
// colliding sym paths by which node's span contains `line` (0 = don't
// care). module main and func main share the path "main"; the site's
// line picks the one it lives in. A name match is kept as a fallback so a
// zero or slightly-off span still resolves to something.
func (e *engine) nodeByAddr(rel, sym string, line int) *treeNode {
	f := e.fileByRel[rel]
	if f == nil || sym == "" {
		return f
	}
	var find func(n *treeNode) *treeNode
	find = func(n *treeNode) *treeNode {
		var fallback *treeNode
		for _, c := range e.kids(n) {
			if c.sym == sym {
				if line == 0 || (line >= c.at[0] && line <= c.at[1]) {
					return c
				}
				if fallback == nil {
					fallback = c
				}
				continue
			}
			if strings.HasPrefix(sym, c.sym+".") {
				if r := find(c); r != nil {
					return r
				}
			}
		}
		return fallback
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
	e := &engine{s: s, project: project, fileByRel: map[string]*treeNode{}, elemCost: map[*selElem]int{}}
	if s.queryWorkBudget > 0 {
		// An explicit server-level ops budget (tests, --legacy paths):
		// deterministic, no wall clock.
		e.workLeft = s.queryWorkBudget
	} else {
		// Omitted default: a 10s wall-clock budget with the ops cap as a
		// backstop. Most queries finish well under it (so stay
		// deterministic); only a genuinely huge one hits the clock. A
		// caller wanting reproducibility passes an Nops budget.
		e.deadline = time.Now().Add(defaultBudgetMs * time.Millisecond)
		e.workLeft = maxBudgetOps
	}
	// lspLeft is set lazily on the first round-trip (ensureLSPCap), so a
	// non-edge query never pays to compute the workspace-tuned cap.
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
	// selRegex is `~=` — an unanchored RE2 match, which is how this
	// language spells OR: [path~=test|smoke]. CSS defines ~= as a
	// space-separated word-list match, which is worthless here (names
	// and paths are not word lists) — it used to be an error pointing
	// at ^= *= $=. Regex is what callers actually reach for, and it
	// subsumes all three with anchors.
	//
	// AND is NOT here on purpose: compound attrs already conjoin,
	// [path*=ma][path*=in], which is CSS-native and needs no operator.
	selRegex // ~=
)

// selAttrAxis picks WHICH strings an attr filter tests a node against.
// The axes are deliberately disjoint: a node is CALLED something and it
// LIVES somewhere, and conflating the two made [name] quietly answer
// path questions (see nameIDs).
type selAttrAxis uint8

const (
	// attrID is `#id` — every id the node answers to, INCLUDING a
	// symbol's "<file>#<sym>" address. This is the zero value because
	// `#id` is parsed straight into a selAttr, and the address must keep
	// matching: #'store.go#Save' is how a single symbol is pinned.
	attrID selAttrAxis = iota
	// attrName is `[name]` — what the node is CALLED, never where it lives.
	attrName
	// attrPath is `[path]` — where the node LIVES, never what it's called.
	attrPath
)

type selAttr struct {
	axis  selAttrAxis
	op    selOp
	value string
	// re is the compiled pattern for `~=`, compiled at PARSE time so a
	// bad regex is a selector error rather than a silent zero-match.
	re *regexp.Regexp
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

	// isFrag marks a `::grep('pattern')` pseudo-element — a generated
	// node per MATCHED LINE of the host's own source (URL text-fragment
	// prior, grep muscle memory). Same contract as edges: invisible to
	// `*`, address = the site (file@line), node_read/node_edit touch
	// the matched line. Replaces the grep tool-field outright;
	// :contains is its boolean form.
	isFrag   bool
	fragSpec *grepSpec
	fragRaw  string // the verbatim argument — the memo key

	// isComment marks a `::comment` pseudo-element — the joined doc block
	// above a symbol, generated from the symbol's stored span. Same
	// contract as ::grep: invisible to `*`, address = file@line, a
	// generated node — never a tree child.
	isComment bool

	// genPart marks a `::signature` / `::body` pseudo-element ("sig" |
	// "body"): a callable split into its declaration head and its body
	// block, generated from the stored body-start line. Same generated-
	// element contract as ::comment.
	genPart string

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

// isGenerated reports whether the compound names a GENERATED pseudo-element
// (::grep line, ::comment block, ::signature/::body split) — nodes minted
// on demand, invisible to `*`, that the cardinality planner and containment
// walk must not treat as ordinary tree nodes.
func (c *selCompound) isGenerated() bool {
	return c.isFrag || c.isComment || c.genPart != ""
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
	pseudoContains  selPseudoKind = iota // :contains('text') — filter on own source
	pseudoAnnotated                      // :annotated('text')— filter on the block ABOVE the decl
	pseudoParents                        // :parents[(sel)]   — MOVE upstream (the one inverse)
	pseudoWhere                          // :where(sel)       — filter: a path matches
	pseudoAny                            // :any[(sel)]       — ∃ claim
	pseudoAll                            // :all[(sel)]       — ∀ claim
	pseudoEmpty                          // :empty[(sel)]     — ∄ claim
	pseudoNot                            // :not(sel)         — the node ITSELF does not match (CSS-true)
	pseudoIs                             // :is(sel)          — the node ITSELF matches (CSS-true)
	pseudoArity                          // :arity(m,n)       — count of `argument` children in [m,n]
	pseudoRecursive                      // :recursive        — a callable with an LSP-confirmed self-call
)

func (k selPseudoKind) isMove() bool  { return k == pseudoParents }
func (k selPseudoKind) isClaim() bool { return k == pseudoAny || k == pseudoAll || k == pseudoEmpty }

type selPseudo struct {
	kind  selPseudoKind
	grep  *grepSpec    // pseudoContains only
	inner selectorList // nil on a bare :parents (= :parents(*)) or a bare claim

	// arityLo/arityHi bound pseudoArity's `argument`-child count. arityHi
	// < 0 means unbounded (:arity(m,) — m-or-more).
	arityLo, arityHi int
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

// normalizeSelector repairs the colon mistakes models actually make,
// BEFORE parsing — the `.func` lesson generalized: accept what is
// unambiguous, error only on what is actually wrong. Outside quotes and
// [attr] brackets, and never after an id/class sigil:
//
//	has(x)  any(x)  not(x) …  → :any(x) :not(x) …   (missing ':';
//	        :has IS our :any, so it maps instead of lecturing)
//	out.call  in  :out  grep( → ::out.call ::in ::out ::grep(
//	        (edge/fragment names take TWO colons — one or zero repaired)
//	::any(x)                  → :any(x)             (one too many)
func normalizeSelector(s string) string {
	rs := []rune(s)
	b := make([]rune, 0, len(rs)+8)
	var quote rune
	brackets := 0
	for i := 0; i < len(rs); {
		c := rs[i]
		if quote != 0 {
			b = append(b, c)
			if c == quote {
				quote = 0
			}
			i++
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '[':
			brackets++
		case ']':
			if brackets > 0 {
				brackets--
			}
		}
		if brackets == 0 && isSelIdentStart(c) {
			j := i
			for j < len(rs) && isModIdentPart(rs[j]) {
				j++
			}
			word := string(rs[i:j])
			var next rune
			if j < len(rs) {
				next = rs[j]
			}
			colons := 0
			for k := len(b) - 1; k >= 0 && b[k] == ':'; k-- {
				colons++
			}
			var prev rune
			if k := len(b) - colons - 1; k >= 0 {
				prev = b[k]
			}
			fix := func(want int, name string) {
				b = b[:len(b)-colons]
				for range want {
					b = append(b, ':')
				}
				b = append(b, []rune(name)...)
			}
			switch {
			case prev == '#' || prev == '.':
				b = append(b, rs[i:j]...) // an id or class keeps its word
			case word == "has" && next == '(':
				fix(1, "any")
			case normalizeAliases[word] != "" && next == '(':
				fix(1, normalizeAliases[word])
			case selPseudoParenName(word) && next == '(':
				fix(1, word)
			case selPseudoElemName(word):
				fix(2, word)
			default:
				b = append(b, rs[i:j]...)
			}
			i = j
			continue
		}
		b = append(b, c)
		i++
	}
	return string(b)
}

func selPseudoParenName(w string) bool {
	switch w {
	case "not", "is", "where", "any", "all", "empty", "contains", "annotated",
		"parents", "depth", "has_parent", "references":
		return true
	}
	return false
}

// normalizeAliases maps near-miss pseudo spellings to the real one.
var normalizeAliases = map[string]string{"parent": "parents"}

func selPseudoElemName(w string) bool {
	switch w {
	case "in", "out", "grep", "ref", "comment":
		return true
	}
	return false
}

func parseModernSelector(input string) (selectorList, error) {
	p := &modSelParser{s: []rune(normalizeSelector(input))}
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
			for p.peekIsPseudoElement() {
				if err := p.appendRefElem(&cx); err != nil {
					return cx, err
				}
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
			// Tight-bound pseudo-elements chain: X::out::grep('…').
			for p.peekIsPseudoElement() {
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
		if comp.isFrag {
			return fmt.Errorf("::grep doesn't repeat — a fragment is a matched line, not an edge to cross")
		}
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
	case "grep":
		return p.parseFragmentElement()
	case "comment":
		return p.parseCommentElement()
	case "signature", "body":
		return p.parseGenPartElement(name)
	case "ref":
		return comp, fmt.Errorf("the reference elements are named by DIRECTION: ::in (who points here) and ::out (what this points at) — e.g. ::out.call, ::in.type")
	default:
		return comp, fmt.Errorf("unknown pseudo-element ::%s — ::in / ::out (edges), ::grep('pattern') (matched lines), ::comment (doc block), ::signature / ::body (a callable's decl head / body) exist", name)
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
				// The caller chains it: X::out::grep('…') greps the SITE.
				return comp, nil
			}
			if err := p.parsePseudo(&comp); err != nil {
				return comp, err
			}
		default:
			return comp, nil
		}
	}
}

// parseCommentElement parses ::comment — no argument (it IS the doc
// block), but it accepts trailing filter pseudos so ::comment:contains('TODO')
// works. It has no kind/id/attr: a doc block has no name.
func (p *modSelParser) parseCommentElement() (selCompound, error) {
	comp := selCompound{isComment: true, class: "comment"}
	for {
		switch p.peek() {
		case ':':
			if p.peekIsPseudoElement() {
				return comp, nil // chained: ::comment::grep would be a new element
			}
			if err := p.parsePseudo(&comp); err != nil {
				return comp, err
			}
		case '.', '#', '[':
			return comp, fmt.Errorf("::comment has no kind, id or attr — it IS the doc block; filter it with :contains('…')")
		default:
			return comp, nil
		}
	}
}

// parseGenPartElement parses ::signature / ::body — no argument (it IS the
// decl head / body block), but it accepts trailing filter pseudos so
// ::body:contains('TODO') works. Like ::comment it has no kind/id/attr.
func (p *modSelParser) parseGenPartElement(name string) (selCompound, error) {
	part := "sig"
	if name == "body" {
		part = "body"
	}
	comp := selCompound{genPart: part, class: name}
	for {
		switch p.peek() {
		case ':':
			if p.peekIsPseudoElement() {
				return comp, nil // chained: ::body::grep is a new element
			}
			if err := p.parsePseudo(&comp); err != nil {
				return comp, err
			}
		case '.', '#', '[':
			return comp, fmt.Errorf("::%s has no kind, id or attr — it IS the callable's %s; filter it with :contains('…')", name, name)
		default:
			return comp, nil
		}
	}
}

// parseFragmentElement parses ::grep('pattern') — leading grep flags
// (INCLUDING -A/-B/-C context, attached: -A2) then the pattern verbatim,
// exactly :contains's argument shape plus context.
func (p *modSelParser) parseFragmentElement() (selCompound, error) {
	var comp selCompound
	comp.isFrag = true
	comp.class = "fragment"
	if p.peek() != '(' {
		return comp, p.errf("'(' after ::grep, e.g. ::grep('-w TODO')")
	}
	p.i++
	p.skipWS()
	q := p.peek()
	if q != '"' && q != '\'' {
		return comp, p.errf("a quoted pattern, e.g. ::grep('-i -A2 derp')")
	}
	p.i++
	start := p.i
	for !p.eof() && p.s[p.i] != q {
		p.i++
	}
	if p.eof() {
		return comp, p.errf(fmt.Sprintf("a closing %c for ::grep", q))
	}
	text := string(p.s[start:p.i])
	p.i++
	p.skipWS()
	if p.peek() != ')' {
		return comp, p.errf("')' to close ::grep")
	}
	p.i++
	g, err := parseFragmentSpec(text)
	if err != nil {
		return comp, fmt.Errorf("bad ::grep(%q): %w", text, err)
	}
	comp.fragSpec = g
	comp.fragRaw = text
	for {
		if p.peekIsPseudoElement() {
			return comp, fmt.Errorf("a ::grep fragment has no pseudo-elements of its own — it IS the matched line")
		}
		switch p.peek() {
		case ':':
			if err := p.parsePseudo(&comp); err != nil {
				return comp, err
			}
		case '#', '[', '.':
			return comp, fmt.Errorf("a ::grep fragment has no ids or classes — it IS the matched line; narrow with the pattern")
		default:
			return comp, nil
		}
	}
}

// parseFragmentSpec is parseContainsSpec plus context: leading boolean
// flags AND attached -A<n>/-B<n>/-C<n>, then the rest VERBATIM as the
// pattern (spaces and all — substring by default, -E for a regex).
func parseFragmentSpec(text string) (*grepSpec, error) {
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
		bs := tok[1:]
		for bi := 0; bi < len(bs); bi++ {
			c := bs[bi]
			if applyBoolFlag(g, c) {
				continue
			}
			switch c {
			case 'A', 'B', 'C':
				num := bs[bi+1:]
				bi = len(bs)
				v, err := strconv.Atoi(num)
				if err != nil || v < 0 {
					return nil, fmt.Errorf("::grep: -%c needs a number attached (as in -%c2)", c, c)
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

func validRefClass(c string) bool {
	switch c {
	case "call", "type", "import", "implements": // the KIND axis
		return true
	}
	return refClassIsPosition(c)
}

// refClassIsPosition reports whether a ref class names the POSITION axis
// (where the occurrence sits) rather than the kind axis (what it is).
func refClassIsPosition(c string) bool {
	switch c {
	case "return", "param", "field", "var":
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
	switch name {
	case "name":
		a.axis = attrName
	case "path":
		a.axis = attrPath
	default:
		return a, fmt.Errorf("unknown attribute %q: [name] (what it's CALLED — leaf or dotted path) "+
			"and [path] (where it LIVES — workspace-relative file path) are the attributes "+
			"(ops: = ^= $= *=)\n\n%s", name, selectorGrammarHelp)
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
	case '~':
		p.i++
		if p.peek() != '=' {
			return a, p.errf("'=' (to complete '~=')")
		}
		p.i++
		a.op = selRegex
	default:
		return a, p.errf("one of = ^= $= *=")
	}
	value, quoted := p.readAttrValue(a.op == selRegex)
	a.value = value
	if a.op == selRegex {
		re, err := regexp.Compile(a.value)
		if err != nil {
			return a, fmt.Errorf("[%s~=%s]: bad regex: %w", name, a.value, err)
		}
		a.re = re
	} else if !quoted && strings.Contains(a.value, "|") {
		// `|` under a LITERAL op is always an alternation attempt: it
		// matches the literal "a|b", finds nothing, and a wrapping :not()
		// then excludes nothing and hands back the unfiltered set.
		// Measured before ~= existed: func:not([path*=test|smoke])
		// returned all 820 funcs, looking exactly like a filter that
		// worked. A silent no-op is the one outcome a filter must never
		// have — so this stays an error even though ~= now answers it.
		return a, fmt.Errorf("[%s%s%s]: %s is a LITERAL match, so this looks for the literal %q "+
			"and silently no-ops (a wrapping :not() would then exclude nothing). "+
			"Alternation is regex: [%s~=%s]. To match a real '|', quote it: [%s%s'%s']",
			name, selOpSpelling(a.op), a.value, selOpSpelling(a.op), a.value,
			name, a.value, name, selOpSpelling(a.op), a.value)
	}
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
	case "not", "is":
		// SELF-anchored, exactly CSS: :not(#main) = not named main.
		// (The relative inners belong to :where/:any/:all/:empty — the
		// :has family; :not/:is are the element-test family.)
		inner, err := p.parsePseudoArg(name, false)
		if err != nil {
			return err
		}
		kind := pseudoNot
		if name == "is" {
			kind = pseudoIs
		}
		comp.pseudos = append(comp.pseudos, selPseudo{kind: kind, inner: inner})
		return nil
	case "grep":
		return fmt.Errorf(":grep is the pseudo-ELEMENT ::grep('-w TODO') — it mints the matched LINES as nodes; the boolean filter is :contains('…')")
	case "decorated", "tagged", "annotation":
		return fmt.Errorf(":%s is spelled :annotated('pat') — it greps the decorator/annotation/doc block above the declaration", name)
	case "has", "has_parent", "references":
		return removedPseudoErr(name)
	case "contains":
		// Same grep-flag vocabulary, same matcher — :contains is just
		// the boolean any-match projection of ::grep, over the body.
		g, err := p.parseGrepPseudoSpec("contains")
		if err != nil {
			return err
		}
		comp.pseudos = append(comp.pseudos, selPseudo{kind: pseudoContains, grep: g})
		return nil
	case "annotated":
		// The same matcher, over the annotation/decorator/doc block
		// ABOVE the declaration (see nodeAnnotated).
		g, err := p.parseGrepPseudoSpec("annotated")
		if err != nil {
			return err
		}
		comp.pseudos = append(comp.pseudos, selPseudo{kind: pseudoAnnotated, grep: g})
		return nil
	case "arity":
		// Structural (not edge-guessed): the count of `argument` children.
		// :arity(2) exactly two, :arity(2,) two-or-more, :arity(0,3) up to
		// three, :arity(0,0) no-arg.
		lo, hi, err := p.parseParenRange("arity")
		if err != nil {
			return err
		}
		comp.pseudos = append(comp.pseudos, selPseudo{kind: pseudoArity, arityLo: lo, arityHi: hi})
		return nil
	case "recursive":
		// A callable with a DIRECT self-call. Edge-semantic, so it is only
		// sound once a child LSP resolves the self-call's target (a lexical
		// self-edge is a name collision: `func Write` calling `w.Write` is
		// io.Writer's, not itself). No argument — mutual/cyclic recursion is
		// a separate, harder predicate.
		if p.peek() == '(' {
			return fmt.Errorf(":recursive takes no argument (direct self-recursion only); mutual/cyclic recursion is not yet a predicate — walk it with ::out.call{1,} for now")
		}
		comp.pseudos = append(comp.pseudos, selPseudo{kind: pseudoRecursive})
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

// parseParenRange parses a pseudo's "(m)", "(m,)" or "(m,n)" argument —
// the same m,n shape as {m,n} but parenthesized, for count-valued
// pseudos like :arity. hi < 0 means unbounded ((m,) = m-or-more).
func (p *modSelParser) parseParenRange(name string) (lo, hi int, err error) {
	if p.peek() != '(' {
		return 0, 0, p.errf("'(' after :" + name)
	}
	p.i++ // '('
	p.skipWS()
	lo, ok := p.readNonNegInt()
	if !ok {
		return 0, 0, p.errf("a count, e.g. :" + name + "(2) or :" + name + "(0,3)")
	}
	hi = lo
	p.skipWS()
	if p.peek() == ',' {
		p.i++
		p.skipWS()
		if p.peek() == ')' {
			hi = -1 // (m,) — unbounded
		} else {
			hi, ok = p.readNonNegInt()
			if !ok {
				return 0, 0, p.errf("a max count or ')' (as in :" + name + "(2,) = two-or-more)")
			}
			if hi < lo {
				return 0, 0, fmt.Errorf(":%s(%d,%d): max must be >= min", name, lo, hi)
			}
			p.skipWS()
		}
	}
	if p.peek() != ')' {
		return 0, 0, p.errf("')' to close :" + name)
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

// selOpSpelling renders an op back as written, so an error can quote
// the caller's own selector instead of a normalized one.
func selOpSpelling(op selOp) string {
	switch op {
	case selPrefix:
		return "^="
	case selSuffix:
		return "$="
	case selContains:
		return "*="
	}
	return "="
}

// readAttrValue returns the value and whether it was QUOTED. Quoting is
// the literal escape — [path*='a|b'] means the caller wants a real '|',
// so the alternation guard must not fire on it.
//
// regex makes the reader bracket-aware: `]` inside a char class
// ([name~=^[A-Z]]) is part of the pattern, not the attribute terminator,
// so a `]`-bearing regex needs no quoting. Depth-counting also passes
// POSIX classes ([[:alpha:]]); an escaped `\]` or `]` as a class's first
// member is the rare case that still wants quotes.
func (p *modSelParser) readAttrValue(regex bool) (string, bool) {
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
		return v, true
	}
	start := p.i
	depth := 0
	for !p.eof() {
		c := p.s[p.i]
		if c == ']' && depth == 0 {
			break // the attribute's closing bracket
		}
		if regex {
			if c == '[' {
				depth++
			} else if c == ']' {
				depth--
			}
		}
		p.i++
	}
	return strings.TrimSpace(string(p.s[start:p.i])), false
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
	"import": true, "argument": true, "annotation": true, "return": true,
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

const selectorGrammarHelp = `How to USE the selector (spec tables at the bottom).

WORK IN SMALL STEPS. Run a cheap query, take an ADDRESS from matches[].node, feed it to the
next query or to node_read/node_edit. Two simple queries beat one clever one — every nesting
level is a chance to lose a paren. If you are 3 parens deep, split the query.

:explain <selector>  a query MODE (prefix, not a pseudo) — returns a COST TREE, not matches:
each element's a-priori est (free) beside its measured work, with >x lower bounds on the element
that ate the budget. Use it when a query blows the budget to see WHICH element to narrow.

TASK → QUERY
  what is here?               :root > *            then descend:  #web > *
  find something by name      #Save                anywhere;  #'store.go#Save' pins one
  what is in a file           #'store.go' func     (or *, method, struct, …)
  who calls X                 #'store.go#Save'::in.call          rows carry from: + a file@line site
  what does X call            #'store.go#Save'::out.call > *
  everything that reaches X   #'X'::in.call{1,} > *
  is X used at all            #'X'::in             (0 matches = unused)
  dead code                   func:not(#main):not(#init):not([name^=Test]):where(::in:empty)
  find TODOs / any text       func::grep('-w TODO')              each row IS the line, editable
  uses of a dependency        import#huma::in.call               externals resolve to the import node
  endpoints on a dependency   import#huma::in.call::grep('-E (Register|Get|Post)\(')
  where a type is USED        #'T'::in.type        imports: ::in.import.  There is NO .implements kind.
  only one language           file.go   func.ts

COMPOSING — one hop at a time, left to right:
  1. anchor on something NAMED:   #'store.go#Save'      names are #ids, never bare words
  2. cross ONE edge:              ::in.call             ::in = toward me, ::out = away from me
  3. land on the far node:        > *                   the far end is the edge's child
  4. filter what you landed on:   > func[name^=Test]
  A claim decides the ANCHOR, not the landing: func:where(::in:empty) returns funcs, judged
  by their edges. To go deeper, STOP — run the query, then start the next one from its rows.

SPEC
  TAGS   project dir file func method type struct interface class const var field enum ctor
         module import argument annotation return, * — fixed; can't invent one. Lang class: file.go.
         annotation = a decorator (@route, py/ts) or struct-tag key (json, go), a CHILD of the
         symbol it marks. func:any(annotation#route). #route = leaf, #'app.route' = as written.
         return = a callable's result TYPE, a CHILD of it. func:any(return#error); Go (T,error)
         splits into one return child per type. #error = leaf, #'io.Writer' = via the full alias.
  ID     #bare ([A-Za-z_][A-Za-z0-9_.-]*) or #'anything else' — quote, never escape. A symbol
         answers to leaf, dotted path, "<file>#<sym>"; an edge answers to its far end's ids.
  ATTR   [name…] = what it's CALLED (leaf, dotted path).  [path…] = where it LIVES
         (workspace-relative file path; a symbol answers with its FILE's).
         OPS  = ^= $= *= are LITERAL (exact/prefix/suffix/contains).  ~= is a regex.
         OR   is the regex: [path~=test|smoke].  Quote a literal '|': [path*='a|b'].
         AND  is just two attrs — [path*=ma][path*=in] — CSS conjoins a compound.
         Non-test funcs: func:not([path~=test|smoke]).
         #id spans both axes and adds the "<file>#<sym>" address; it is never a regex.
  CONF   edge conf: "lsp" = LSP-resolved; "lexical" = name UNIQUE (certain); "unsettled" =
         several same-named decls, no LSP — a GUESS whose to:/from: is a CANDIDATE LIST, not a fact.
         A far end the LSP resolves OUTSIDE the git root (stdlib, a dep) is an external STUB —
         to:["strings#Split"], domain:"external" — nameable, read-only, [not indexed], never a
         false local.
  EDGES  ::in / ::out, on TWO orthogonal class axes (bare = any): KIND .call/.type/.import
         (what it is) and POSITION .return/.param/.field/.var (where it sits). Plus .implements
         (LSP-only): interface#Foo::in.implements > * = implementers, type#Bar::out.implements
         > * = interfaces Bar satisfies. They compose:
         #'S'::in.return.type = S used as a return type, .param.type = as a param type. The
         ref IS the occurrence (its address is the site); its far end (via >) is the SOURCE
         symbol — #'S'::in.return.type > * :parents(func). Attached to the INNERMOST enclosing
         symbol: X::out = X's own, X ::out = nested symbols' too. {m,n} = edges crossed, {1,} =
         transitive. * NEVER matches an edge. Address = site ("file@line").
  ::grep('flags pattern')  matched lines as nodes. -i -w -E -F -v -A<n> -B<n> -C<n>; literal
         unless -E. :contains('…') is the boolean form over the node's BODY.
  ::comment  the doc block above a decl, contiguous lines joined, a GENERATED node (invisible
         to *). func:any(::comment) = documented; func:not(:any(::comment)) = undocumented;
         ::comment:contains('TODO'). Address = file@line.
  ::signature / ::body  a callable split into its decl HEAD (no doc, no body) and its BODY
         (the statements between the braces) — GENERATED nodes carrying their source INLINE, so
         func::signature is a one-query signature overview. EDITABLE: node_edit #'F'::body
         newText:'…' rewrites just the body (address = file@start-end, a range).
  :annotated('…')  boolean grep over the decorator/annotation/doc block ON the decl —
         the symbol CARRYING the mark, not the line. func:annotated('@app.route'),
         class:annotated('@Component'), func:annotated('-w Deprecated'). Same flags as :contains.
  :parents(sel)  everything UPSTREAM of the tip (ancestors ∪ incoming refs, transitive) —
         broader than callers. *:parents:empty = only :root.
  CLAIMS bare :any/:all/:empty judge the set at their position (in :where, or after :parents)
         and decide the node under test. Parenthesized :where/:any/:all/:empty(sel) are
         RELATIVE (CSS-nesting): leading tag = descendant, leading ::/pseudo = the node, '>' =
         child, & = the node, :root re-anchors.
  SELF   :not(sel)/:is(sel) test the node ITSELF (CSS): func:not(#main), file > :is(func,method).
  ARITY  :arity(m,n) filters by argument-child count — :arity(2) exactly two, :arity(2,)
         two-or-more, :arity(0,0) no-arg. Structural (not edge-resolved). func:arity(4,).
  RECUR  :recursive — a callable that DIRECTLY calls itself, CONFIRMED by a child LSP (a lexical
         self-edge is a name collision: func Write calling w.Write is io.Writer's). func:recursive.
         Sound only with a language server; the MCP server has one, the query CLI does not.
  ORDER  :first/:last — this position's matches, per anchor, document order.
  REP    {m,n} repeats an element or (group), child-joined: func{2} = func>func; within 1..3
         levels = "> *{0,2} > x". {m,} unbounded (safe: cycle-guarded + work budget).
  COMB   space = descendant, '>' = child, ',' = union.`

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

// pushdownLeadingRef derives the small candidate host set for a GLOBAL
// leading ref that is filtered to one exact far-end name, so evalElems
// can skip expanding the implied universal host to the whole workspace.
//
// The reduction is exact by a symmetry: an out-edge H→X is the SAME
// underlying site as X's in-edge with far=H, so the hosts of `::out#X`
// are precisely the far ends of X's IN edges, and the hosts of `::in#X`
// the far ends of X's OUT edges. X has a handful of declarations with a
// handful of sites, so this is cheap where the universal expansion is
// O(workspace) (measured 6 hosts vs 4,206 for #Save).
//
// It returns a SUPERSET of the true hosts and refMatches re-filters
// exactly, so the only correctness obligation is completeness — never
// dropping a host that could match. That holds only when:
//   - the expansion would be global (tips is exactly the project node),
//     never a relative/nested `&`-anchored ref;
//   - elem 0 is the synthesized universal host (pseudoOnly);
//   - the far filter is ONE exact bare-leaf name, which declsOf resolves
//     completely (it keys on the leaf, so a dotted #a.b or an address
//     #'f#S' would resolve short — those keep the full scan).
func (e *engine) pushdownLeadingRef(elems []selElem, tips map[*treeNode]bool) (map[*treeNode]bool, bool) {
	if e.noPushdown || len(elems) < 2 || len(tips) != 1 || !tips[e.project] {
		return nil, false
	}
	host := elems[0].comp
	if host == nil || !host.pseudoOnly() {
		return nil, false // elem 0 must be the implied universal host
	}
	ref := elems[1].comp
	name, ok := refFarLeaf(ref)
	if !ok {
		return nil, false
	}
	opp := "out"
	if ref.refDir == "out" {
		opp = "in"
	}
	hosts := map[*treeNode]bool{}
	for _, decl := range e.declsOf(name) {
		for _, r := range e.refNodes(decl, opp) {
			for _, f := range r.refFar {
				hosts[f] = true
			}
		}
	}
	return hosts, true
}

// refFarLeaf returns the single bare-leaf name a ref's far end is pinned
// to, and true only when that is the ref's ONLY id/name constraint. Kind
// classes (.call), pseudos and combinators are left to refMatches — they
// only ever narrow the superset further.
func refFarLeaf(comp *selCompound) (string, bool) {
	if comp == nil || !comp.isRef || len(comp.attrs) != 1 {
		return "", false
	}
	a := comp.attrs[0]
	if a.op != selExact || a.axis == attrPath {
		return "", false // ^= *= ~= can't be index-resolved; path isn't a far name
	}
	if strings.ContainsAny(a.value, ".#") {
		return "", false // dotted/address form: declsOf keys on the leaf
	}
	return a.value, true
}

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

// ordered returns set's nodes in document order.
//
// Traversal order is load-bearing ONLY because the work budget can trip
// mid-walk. Ranging a Go map is randomized per run, so when the budget
// trips the cutoff lands on a different subset every time and the same
// selector answers differently run to run. Walking a fixed order makes
// a budget-truncated result reproducible: still partial, still flagged
// by workExceeded, but the SAME partial every time.
//
// Use this in any loop that can reach e.spend. Loops that only union
// sets together are order-independent — leave those ranging the map.
func ordered(set map[*treeNode]bool) []*treeNode {
	out := make([]*treeNode, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return nodeLess(out[i], out[j]) })
	return out
}

// evaluate runs a parsed selector over the tree and returns the
// matching nodes in deterministic pre-order.
func (e *engine) evaluate(list selectorList) []*treeNode {
	return ordered(e.evalList(list, e.project, false))
}

// classCounts returns symbols-per-class over the whole workspace, the
// a-priori bare-class figure for the query-cost estimator (:explain's est
// column, the descendant-chain planner). The class lives in the symbol
// TREE, not the index, so the first call after any index change walks
// every file's symbols once; it is then memoized against the index
// generation and O(1) until the index next changes. An estimate — it
// never spends the query budget (walks kids directly, not through spend).
func (s *Server) classCounts() map[string]int {
	var gen uint64
	if idx := s.getIndex(); idx != nil {
		gen = idx.Generation()
	}
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if s.statsClass != nil && s.statsGen == gen {
		return s.statsClass
	}
	counts := map[string]int{}
	if e, err := s.buildTree(); err == nil {
		var walk func(n *treeNode)
		walk = func(n *treeNode) {
			if n.sym != "" { // symbols only — dirs/files/project have no class to tally
				counts[n.class]++
			}
			for _, c := range e.kids(n) {
				walk(c)
			}
		}
		walk(e.project)
	}
	s.statsClass = counts
	s.statsGen = gen
	return counts
}

// reorderFactor: only reorder a chain when the tip's a-priori
// cardinality is at least this many times smaller than the leading
// element's — a clear win, not a coin-flip, given both paths still walk
// the tree to seed.
const reorderFactor = 4

// planReorder evaluates a top-level pure-descendant chain from its RAREST
// end. The result of `C0 C1 … Cn` is the Cn matches whose ancestors match
// C0 ⊃ … ⊃ C(n-1) in order; the forward evaluator collects C0 and
// descends, which is expensive when C0 is broad. When the tip Cn is far
// rarer (by the commit-2 est), this collects Cn and checks the ancestor
// SUBSEQUENCE instead — the same set, seeded from the selective end. The
// pushdown is this idea for a leading ref; this is the containment form.
//
// Returns (result, true) only for an eligible, clearly-winning chain;
// otherwise the caller runs the forward evaluation unchanged.
func (e *engine) planReorder(cx selComplex) (map[*treeNode]bool, bool) {
	if e.noPlan || cx.rel != relTop || len(cx.elems) < 2 {
		return nil, false
	}
	for i := range cx.elems {
		if !plainDescendantElem(&cx.elems[i], i) {
			return nil, false
		}
	}
	last := cx.elems[len(cx.elems)-1].comp
	// The tip must be exact-name-anchored so it can be seeded from the
	// INDEX — that is the whole win: load only the files that contain the
	// name, never the whole workspace the forward collect would walk.
	tip, ok := exactBareName(last)
	if !ok {
		return nil, false
	}
	idx := e.s.getIndex()
	if idx == nil {
		return nil, false
	}
	// O(1) decision — deliberately NOT estCard/classCounts, which would
	// trigger the very full-symbol walk the seed is trying to avoid. A
	// rare tip against a broad leading element is the clear win.
	firstEst, fok := e.estCardCheap(cx.elems[0].comp)
	if !fok || idx.NameFreq(tip)*reorderFactor >= firstEst {
		return nil, false
	}

	leading := make([]*selCompound, len(cx.elems)-1)
	for i := range leading {
		leading[i] = cx.elems[i].comp
	}
	out := map[*treeNode]bool{}
	for _, n := range e.declsNamed(tip) {
		if e.positionalMatch(n, last) && e.ancestorSubseq(n, leading) {
			out[n] = true
		}
	}
	return out, true
}

// declsNamed returns every declaration whose leaf is `name`, seeded from
// the INDEX: only the files the index records an occurrence of `name` in
// are loaded and walked, never the whole workspace. Complete, because a
// declaration's own name is always an indexed occurrence, so its file is
// always in the set.
func (e *engine) declsNamed(name string) []*treeNode {
	idx := e.s.getIndex()
	if idx == nil {
		return nil
	}
	files := map[string]bool{}
	for _, s := range idx.LookupExisting(name) {
		files[relPath(s.File, e.s.getRoot())] = true
	}
	var out []*treeNode
	for rel := range files {
		f := e.fileByRel[rel]
		if f == nil {
			continue
		}
		var walk func(n *treeNode)
		walk = func(n *treeNode) {
			if n.sym != "" && n.leaf == name {
				out = append(out, n)
			}
			for _, c := range e.kids(n) {
				walk(c)
			}
		}
		walk(f)
	}
	return out
}

// estCardCheap is the O(1) planner estimate — NameFreq for an exact name,
// or the index's distinct-name count as a broad proxy for a bare class.
// Never classCounts: the planner runs on every query and must not force
// the full-symbol walk that the exact tally needs.
func (e *engine) estCardCheap(c *selCompound) (int, bool) {
	if c == nil || c.isRef || c.isGenerated() {
		return 0, false
	}
	idx := e.s.getIndex()
	if idx == nil {
		return 0, false
	}
	for _, a := range c.attrs {
		if a.op == selExact && a.axis != attrPath {
			return idx.NameFreq(a.value), true // name — checked before anyType
		}
	}
	if c.anyType {
		return 0, false // bare * — no cheap estimate
	}
	if c.class != "" {
		return idx.Cardinality(), true // broad: not selective, no walk
	}
	return 0, false
}

// exactBareName returns the single exact bare-leaf name a compound pins,
// if any — a plain identifier (no '.'/'#') declsNamed can resolve fully.
func exactBareName(c *selCompound) (string, bool) {
	if c == nil {
		return "", false
	}
	for _, a := range c.attrs {
		if a.op == selExact && a.axis != attrPath && !strings.ContainsAny(a.value, ".#") {
			return a.value, true
		}
	}
	return "", false
}

// plainDescendantElem reports whether an element is a plain positional
// compound joined by descendant containment — the only shape the reorder
// is sound for. It excludes anything the ancestor-subsequence check
// cannot faithfully replay: edges/fragments/comments, groups, `{m,n}`,
// child `>` (a direct-parent constraint, not a subsequence), `*`, :root,
// `&`, and any pseudo/claim/:first — those carry evaluation order or a
// re-root the forward path expresses and this rewrite would not.
func plainDescendantElem(el *selElem, i int) bool {
	if el.group != nil || el.comp == nil || el.min != 1 || el.max != 1 {
		return false
	}
	if i > 0 && el.comb != selDescendant {
		return false
	}
	c := el.comp
	if c.isRef || c.isGenerated() || c.root || c.selfRef {
		return false
	}
	if len(c.pseudos) > 0 || len(c.positionClaims) > 0 || c.ordSel != 0 {
		return false
	}
	// Needs something to match on. `#ctx` is anyType (no explicit tag) but
	// its name attr qualifies; a bare `*` (anyType, no attr) does not.
	return c.class != "" || len(c.attrs) > 0
}

// ancestorSubseq reports whether comps appear, in order, as a subsequence
// of n's ancestor chain — comps[last] matching the closest qualifying
// ancestor, comps[0] the highest. Greedy bottom-up, the standard
// subsequence match: correct whenever a valid chain exists.
func (e *engine) ancestorSubseq(n *treeNode, comps []*selCompound) bool {
	i := len(comps) - 1
	for p := n.parent; p != nil && i >= 0; p = p.parent {
		if e.positionalMatch(p, comps[i]) {
			i--
		}
	}
	return i < 0
}

const (
	maxBudgetOps = 5_000_000 // deterministic work-unit cap
	maxBudgetMs  = 30_000    // wall-clock cap (30s) — a runaway can't stall the server
)

// parseBudget reads a budget value with an optional `ms`/`ops` suffix. A
// bare number is MILLISECONDS (wall clock — the intuitive default); `ops`
// is deterministic work units. ok=false on empty or non-numeric input.
func parseBudget(s string) (value int, unit string, ok bool) {
	s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), `"`))
	if s == "" || s == "null" {
		return 0, "", false
	}
	unit = "ms"
	switch {
	case strings.HasSuffix(s, "ops"):
		unit, s = "ops", strings.TrimSuffix(s, "ops")
	case strings.HasSuffix(s, "ms"):
		unit, s = "ms", strings.TrimSuffix(s, "ms")
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0, "", false
	}
	return n, unit, true
}

// setBudget applies a parsed budget: `ops` sets the deterministic work
// budget; `ms` sets a wall-clock deadline and leaves work effectively
// unbounded (capped) so the clock is the limit. Call after buildTree.
func (e *engine) setBudget(value int, unit string) {
	if unit == "ops" {
		if value > maxBudgetOps {
			value = maxBudgetOps
		}
		e.workLeft = value
		e.deadline = time.Time{} // explicit ops → deterministic, drop the default wall clock
		return
	}
	if value > maxBudgetMs {
		value = maxBudgetMs
	}
	e.deadline = time.Now().Add(time.Duration(value) * time.Millisecond)
	e.workLeft = maxBudgetOps // ms mode: the clock trips first; this only backstops a runaway
}

// splitExplain strips a leading ":explain" query MODE. It is a mode, not
// a pseudo — kept out of the grammar so it can never nest inside a
// selector. Returns the remaining selector and whether explain was asked.
func splitExplain(sel string) (rest string, explain bool) {
	s := strings.TrimSpace(sel)
	const p = ":explain"
	if s == p {
		return "", true
	}
	if strings.HasPrefix(s, p) {
		switch s[len(p)] {
		case ' ', '\t', '\n':
			return strings.TrimSpace(s[len(p):]), true
		}
	}
	return sel, false
}

// explainRow is one element on the :explain cost tree: its a-priori EST
// (free, from the commit-2 tallies) beside its MEASURED cost (the actual
// budget spent). Measured degrades to a ">x" LOWER BOUND on the element
// the budget tripped in, and "—" for elements the walk never reached —
// full while there's budget, a floor once there isn't.
type explainRow struct {
	Element  string `json:"element"`
	Est      string `json:"est"`
	Measured string `json:"measured"`
	Blown    bool   `json:"blown,omitempty"`
}

func (e *engine) explainRows(list selectorList) []explainRow {
	var rows []explainRow
	reached := true
	var walk func(elems []selElem, indent string)
	walk = func(elems []selElem, indent string) {
		for i := range elems {
			el := &elems[i]
			r := explainRow{Element: indent + renderElem(el), Est: e.estElem(el)}
			switch {
			case !reached:
				r.Measured = "—" // never ran — the budget was gone before it
			case e.workExceeded && el == e.blownElem:
				r.Measured = ">" + commaInt(e.elemCost[el])
				r.Blown = true
				reached = false
			default:
				r.Measured = commaInt(e.elemCost[el])
			}
			rows = append(rows, r)
			if el.group != nil {
				walk(el.group.elems, indent+"  ")
			}
		}
	}
	for ci := range list {
		if len(list) > 1 {
			rows = append(rows, explainRow{Element: fmt.Sprintf("branch %d:", ci+1)})
		}
		walk(list[ci].elems, "")
	}
	return rows
}

// estElem is one element's a-priori cardinality from the commit-2 tallies
// — free, no budget spent. An exact name filter reads NameFreq (O(1)); a
// bare class reads classCounts. Edges and `*` are "?" — the index has no
// fan-out, so their breadth only shows in the MEASURED column (the
// deferred fan-out the commit-2 note names).
func (e *engine) estElem(el *selElem) string {
	if el.comp == nil {
		return "?"
	}
	if n, ok := e.estCard(el.comp); ok {
		return commaInt(n)
	}
	return "?"
}

// estCard is a compound's a-priori cardinality from the commit-2 tallies
// — an exact name filter reads NameFreq (O(1)), a bare class reads
// classCounts. ok=false when there is no cheap estimate (an edge, `*`, a
// pseudo-only compound) — the caller must not reorder on an unknown.
func (e *engine) estCard(c *selCompound) (int, bool) {
	if c == nil || c.isRef || c.isGenerated() {
		return 0, false
	}
	if idx := e.s.getIndex(); idx != nil {
		for _, a := range c.attrs {
			if a.op == selExact && a.axis != attrPath {
				return idx.NameFreq(a.value), true // name — before anyType
			}
		}
	}
	if c.anyType {
		return 0, false // bare * — no cheap estimate
	}
	if c.class != "" {
		return e.s.classCounts()[c.class], true
	}
	return 0, false
}

// costTrace renders the selector as a per-element cost breakdown, marking
// the element the budget tripped on. Empty when nothing was billed (a
// query that never touched the budget), so a caller can skip it.
func (e *engine) costTrace(list selectorList) []string {
	var lines []string
	var walk func(elems []selElem, indent string)
	walk = func(elems []selElem, indent string) {
		for i := range elems {
			el := &elems[i]
			label := indent + renderElem(el)
			mark := ""
			if el == e.blownElem {
				mark = "   ← budget ran out here"
			}
			lines = append(lines, fmt.Sprintf("  %-30s %10s%s", label, commaInt(e.elemCost[el]), mark))
			if el.group != nil {
				walk(el.group.elems, indent+"  ")
			}
		}
	}
	for ci := range list {
		if len(list) > 1 {
			lines = append(lines, fmt.Sprintf("  branch %d:", ci+1))
		}
		walk(list[ci].elems, "")
	}
	return lines
}

// renderElem spells one selector element compactly enough to point at
// it in a cost trace — not a faithful round-trip, a recognizable label.
func renderElem(el *selElem) string {
	var b strings.Builder
	switch {
	case el.group != nil:
		b.WriteString("( … )")
	case el.comp == nil:
		b.WriteString("?")
	default:
		c := el.comp
		switch {
		case c.isRef:
			b.WriteString("::" + c.refDir)
			for _, rc := range c.refClasses {
				b.WriteString("." + rc)
			}
		case c.isFrag:
			b.WriteString("::grep")
		case c.isComment:
			b.WriteString("::comment")
		case c.genPart != "":
			b.WriteString("::" + c.class) // signature | body
		case c.root:
			b.WriteString(":root")
		case c.anyType:
			b.WriteString("*")
		default:
			b.WriteString(c.class)
			if c.langClass != "" {
				b.WriteString("." + c.langClass)
			}
		}
		for _, a := range c.attrs {
			if a.axis == attrID && a.op == selExact {
				b.WriteString("#" + a.value)
				continue
			}
			axis := "name"
			if a.axis == attrPath {
				axis = "path"
			}
			b.WriteString("[" + axis + selOpSpelling(a.op) + a.value + "]")
		}
		if len(c.pseudos) > 0 {
			b.WriteString(":…")
		}
	}
	if el.min != 1 || el.max != 1 {
		if el.max < 0 {
			b.WriteString(fmt.Sprintf("{%d,}", el.min))
		} else {
			b.WriteString(fmt.Sprintf("{%d,%d}", el.min, el.max))
		}
	}
	return b.String()
}

// commaInt formats with thousands separators — a cost of 158649 reads as
// 158,649, which is the number the reader is comparing to the budget.
func commaInt(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return s
	}
	var out []byte
	for i, d := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, d)
	}
	return string(out)
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
	// Cardinality-order a pure-descendant chain: seed from the rarer end
	// when the est says it clearly wins. Never in the relaxed (∀-domain)
	// pass — that one is compared against the strict pass and must walk
	// the same shape.
	if !relaxSubject {
		if out, ok := e.planReorder(cx); ok {
			return out
		}
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
	// Cardinality pushdown for a global leading ref filtered to an exact
	// far-end name — `::in.call#'Save'`. Without it, the implied
	// universal host expands to EVERY symbol and refMatches builds every
	// edge before discarding all but the few whose far end is Save. The
	// index already knows the handful of hosts that can match; start from
	// them and the sweep never happens. See pushdownLeadingRef.
	start := 0
	if hosts, ok := e.pushdownLeadingRef(elems, tips); ok {
		tips = hosts // candidate hosts replace the universal expansion
		start = 1    // elem 0 (the implied universal host) is subsumed
	}
	last := len(elems) - 1
	for i := start; i < len(elems); i++ {
		el := &elems[i]
		relaxed := relaxSubject && i == last
		imin, imax := min0, max0
		if i > 0 {
			imin, imax = combRange(el.comb)
		}
		e.costStack = append(e.costStack, costFrame{el: el})
		next := e.evalRepeat(tips, el, imin, imax, relaxed)
		top := e.costStack[len(e.costStack)-1]
		e.costStack = e.costStack[:len(e.costStack)-1]
		e.elemCost[el] += top.spent
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
	// A repeated edge element is a transitive walk; record per-hop
	// precision so precisionNote can say where the unsettled edges begin
	// (the cap is spent shallowest-first). A single hop leans on the
	// aggregate line instead.
	trackHops := el.comp != nil && el.comp.isRef && (el.max < 0 || el.max > 1)
	frontier := e.evalInstance(tips, el, min1, max1, relaxed)
	if trackHops {
		e.noteHop(1, frontier)
	}
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
		if trackHops {
			e.noteHop(count, next)
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

// noteHop records one hop of a transitive edge walk: how deep it reached
// and, if any of this hop's edges are unsettled guesses, the shallowest
// hop at which that first happens (unsettled edges cluster at the deep
// hops, where the LSP cap is already spent). refs are the ref NODES the
// hop produced; a node reached at more than one hop is counted once.
func (e *engine) noteHop(hop int, refs map[*treeNode]bool) {
	if len(refs) == 0 {
		return // an empty hop crossed nothing; the walk terminated
	}
	e.maxHopReached = max(e.maxHopReached, hop)
	if e.hopCounted == nil {
		e.hopCounted = map[*treeNode]bool{}
	}
	for r := range refs {
		if r.refConf != refUnsettled || e.hopCounted[r] {
			continue
		}
		e.hopCounted[r] = true
		e.transUnsettled++
		if e.unsettledFromHop == 0 || hop < e.unsettledFromHop {
			e.unsettledFromHop = hop
		}
	}
}

// evalInstance evaluates ONE instance of an element from each tip.
func (e *engine) evalInstance(tips map[*treeNode]bool, el *selElem, min, max int, relaxed bool) map[*treeNode]bool {
	if el.group != nil {
		out := map[*treeNode]bool{}
		for _, t := range ordered(tips) {
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
	if comp.isFrag {
		return e.fragMatches(tips, comp, relaxed)
	}
	if comp.isComment {
		return e.commentMatches(tips, comp, relaxed)
	}
	if comp.genPart != "" {
		return e.genPartMatches(tips, comp, relaxed)
	}
	out := map[*treeNode]bool{}
	for _, t := range ordered(tips) {
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
	nodes := ordered(cand)
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
	for _, h := range ordered(hosts) {
		cand := map[*treeNode]bool{}
		// The compound names its direction (::in / ::out), so build only
		// that half — the other is pure waste for this query.
		for _, r := range e.refNodes(h, comp.refDir) {
			if !e.spend(1) {
				break
			}
			if relaxed {
				if refDirMatch(r, comp) {
					cand[r] = true
				}
			} else if e.positionalMatch(r, comp) {
				cand[r] = true
			}
		}
		// `.implements` is LSP-native and expensive, so it is built ONLY
		// when explicitly named (never for a bare ::in/::out), alongside
		// the site edges.
		if compHasClass(comp, "implements") {
			for _, r := range e.implementsRefs(h, comp.refDir) {
				if !e.spend(1) {
					break
				}
				if relaxed {
					if refDirMatch(r, comp) {
						cand[r] = true
					}
				} else if e.positionalMatch(r, comp) {
					cand[r] = true
				}
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

// selfMatches is :not/:is's test — does the node ITSELF match the
// selector, exactly CSS's reading (a leading tag/#id here is the node,
// never a descendant). Single-compound inners test in place; a full
// chain falls back to global-set membership, memoized per inner AST.
func (e *engine) selfMatches(n *treeNode, list selectorList) bool {
	for ci := range list {
		cx := &list[ci]
		if len(cx.elems) == 1 && cx.elems[0].comp != nil && cx.elems[0].group == nil {
			comp := cx.elems[0].comp
			if comp.root && n != e.project {
				continue
			}
			if e.positionalMatch(n, comp) &&
				len(e.applyPseudos(map[*treeNode]bool{n: true}, comp, false)) > 0 {
				return true
			}
			continue
		}
		if e.globalComplexSet(cx)[n] {
			return true
		}
	}
	return false
}

// globalComplexSet memoizes a complex's full workspace match set —
// :not/:is may test it once per candidate node.
func (e *engine) globalComplexSet(cx *selComplex) map[*treeNode]bool {
	key := fmt.Sprintf("%p", cx.elems)
	if e.selfSetCache == nil {
		e.selfSetCache = map[string]map[*treeNode]bool{}
	}
	if v, ok := e.selfSetCache[key]; ok {
		return v
	}
	v := e.evalComplex(*cx, e.project, false)
	e.selfSetCache[key] = v
	return v
}

// fragMatches mints (memoized) the matched-line fragments of each host.
// Under ∀-relaxation the pattern is the tested property, so the domain
// is EVERY line — :all(::grep(p)) = every line of the host matches p.
func (e *engine) fragMatches(hosts map[*treeNode]bool, comp *selCompound, relaxed bool) map[*treeNode]bool {
	out := map[*treeNode]bool{}
	for _, h := range ordered(hosts) {
		cand := map[*treeNode]bool{}
		for _, f := range e.fragmentsOf(h, comp, relaxed) {
			cand[f] = true
		}
		for n := range e.selectOrdered(cand, comp, relaxed) {
			out[n] = true
		}
	}
	return out
}

func (e *engine) fragmentsOf(h *treeNode, comp *selCompound, relaxed bool) []*treeNode {
	key := comp.fragRaw
	if relaxed {
		key = "\x00every-line"
	}
	if e.fragCache == nil {
		e.fragCache = map[*treeNode]map[string][]*treeNode{}
	}
	byPat := e.fragCache[h]
	if byPat == nil {
		byPat = map[string][]*treeNode{}
		e.fragCache[h] = byPat
	}
	if v, ok := byPat[key]; ok {
		return v
	}
	frags := []*treeNode{}
	defer func() { byPat[key] = frags }()
	lines, startLine, ok := e.nodeSource(h)
	if !ok {
		return frags // project/dir (and other sourceless) hosts have no lines
	}
	g := comp.fragSpec
	for i, l := range lines {
		if !e.spend(1) {
			break
		}
		if !relaxed && !g.matchLine(l) {
			continue
		}
		// Elide a long matched line around the match so one generated
		// line can't blow the token budget (the same guard the `search`
		// tool applies). -v has no span to centre on; keep the head.
		ms, me := 0, 0
		if !g.invert {
			if loc := g.re.FindStringIndex(l); loc != nil {
				ms, me = loc[0], loc[1]
			}
		}
		capped := symbols.CapHitLine(l, ms, me)
		hit := &grepHit{Line: startLine + i, Text: capped}
		// Context is clipped to the host's own span — a fragment never
		// leaks its neighbours' lines.
		if g.before > 0 {
			lo := i - g.before
			if lo < 0 {
				lo = 0
			}
			if lo < i {
				hit.Before = capGrepContext(lines[lo:i])
			}
		}
		if g.after > 0 {
			hi := i + 1 + g.after
			if hi > len(lines) {
				hi = len(lines)
			}
			if i+1 < hi {
				hit.After = capGrepContext(lines[i+1 : hi])
			}
		}
		frags = append(frags, &treeNode{
			class: "fragment", leaf: strings.TrimSpace(capped), full: strings.TrimSpace(capped),
			file: h.file, abs: h.abs, at: [2]int{hit.Line, hit.Line},
			parent: h, depth: h.depth + 1, frag: hit,
			fileOrd: h.fileOrd, symOrd: h.symOrd,
		})
	}
	return frags
}

// capGrepContext copies context lines and trims each to the per-line
// budget (no match to centre on — keep the head), so a fragment's
// neighbours can't reintroduce the long-line hazard the matched line
// dodges.
func capGrepContext(src []string) []string {
	out := make([]string, len(src))
	for i, l := range src {
		out[i] = symbols.CapHitLine(l, 0, 0)
	}
	return out
}

// commentMatches returns each host's ::comment node — the joined doc
// block above it — filtered by the compound's pseudos. Mirrors
// fragMatches; the generated node is never a tree child, so `*` and the
// containment walk never see it.
func (e *engine) commentMatches(hosts map[*treeNode]bool, comp *selCompound, relaxed bool) map[*treeNode]bool {
	out := map[*treeNode]bool{}
	for _, h := range ordered(hosts) {
		c := e.commentOf(h)
		if c == nil {
			continue
		}
		for n := range e.selectOrdered(map[*treeNode]bool{c: true}, comp, relaxed) {
			out[n] = true
		}
	}
	return out
}

// commentOf materializes (once) the doc-comment node for a host from its
// stored span, or nil when the host has no attached doc. The node reads
// its own source via nodeSource (its span), so :contains greps the block
// and node_read returns it.
func (e *engine) commentOf(h *treeNode) *treeNode {
	if h.commentAt[0] == 0 {
		return nil // no doc block (or a sourceless host)
	}
	if e.commentCache == nil {
		e.commentCache = map[*treeNode]*treeNode{}
	}
	if c, ok := e.commentCache[h]; ok {
		return c
	}
	c := &treeNode{
		class: "comment",
		file:  h.file, abs: h.abs, at: h.commentAt,
		parent: h, depth: h.depth + 1,
		fileOrd: h.fileOrd, symOrd: h.symOrd,
	}
	e.commentCache[h] = c
	return c
}

// genPartMatches returns each host's ::signature or ::body node, filtered
// by the compound's pseudos. Mirrors commentMatches — the generated node
// is invisible to `*` and the containment walk.
func (e *engine) genPartMatches(hosts map[*treeNode]bool, comp *selCompound, relaxed bool) map[*treeNode]bool {
	out := map[*treeNode]bool{}
	for _, h := range ordered(hosts) {
		g := e.genPartOf(h, comp.genPart)
		if g == nil {
			continue
		}
		for n := range e.selectOrdered(map[*treeNode]bool{g: true}, comp, relaxed) {
			out[n] = true
		}
	}
	return out
}

// genPartOf materializes (once per host+part) a callable's ::signature
// ("sig") or ::body node from its stored body-start line, or nil when the
// host has no body (not a callable, or a bodyless decl). The node carries
// its source INLINE (genText) so a broad `func::signature` returns every
// signature in one query — the token-lean overview — and reads its own span
// so :contains greps it and node_read returns it.
func (e *engine) genPartOf(h *treeNode, part string) *treeNode {
	if h.bodyAt == 0 {
		return nil
	}
	lines, startLine, ok := e.nodeSource(h)
	if !ok {
		return nil
	}
	bodyIdx := h.bodyAt - startLine // 0-based index of the body-start line
	if bodyIdx < 0 || bodyIdx >= len(lines) {
		return nil
	}
	if e.genPartCache == nil {
		e.genPartCache = map[*treeNode]map[string]*treeNode{}
	}
	if e.genPartCache[h] == nil {
		e.genPartCache[h] = map[string]*treeNode{}
	}
	if g, ok := e.genPartCache[h][part]; ok {
		return g
	}
	// The signature starts at the DECLARATION head, not the doc block: the
	// symbol's span is doc-inclusive, but the doc has its own ::comment, so
	// skip past it (commentAt) to keep ::signature a lean overview.
	sigStart := h.at[0]
	if h.commentAt[0] > 0 && h.commentAt[1] >= h.at[0] {
		sigStart = h.commentAt[1] + 1
	}
	sigIdx := sigStart - startLine
	if sigIdx < 0 || sigIdx > bodyIdx {
		sigIdx = 0
	}
	class, span, text := "signature", [2]int{startLine + sigIdx, h.bodyAt}, lines[sigIdx:bodyIdx+1]
	if part == "body" {
		// The body is the STATEMENTS between the braces — the `{` line and
		// the `}` line are the signature's / the decl's, not the body's — so
		// a ::body REWRITE replaces just the implementation. Degenerate
		// bodies (single-line, empty) have no such span and yield no node.
		lastIdx := h.at[1] - startLine // index of the closing `}` line
		if bodyIdx+1 >= lastIdx || lastIdx > len(lines) {
			return nil
		}
		class, span, text = "body", [2]int{h.bodyAt + 1, h.at[1] - 1}, lines[bodyIdx+1:lastIdx]
	}
	g := &treeNode{
		class: class,
		file:  h.file, abs: h.abs, at: span,
		parent: h, depth: h.depth + 1,
		fileOrd: h.fileOrd, symOrd: h.symOrd,
		genText: strings.Join(text, "\n"),
	}
	e.genPartCache[h][part] = g
	return g
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
		if !e.spend(1) {
			return
		}
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
		// Each class filters ITS axis: call/type/import test the kind,
		// return/param/field/var test the position. So .return.type ANDs
		// across axes, while a bare .type matches any position.
		for _, c := range comp.refClasses {
			if refClassIsPosition(c) {
				if n.refPos != c {
					return false
				}
			} else if n.refKind != c {
				return false
			}
		}
	} else {
		// Generated nodes are pseudo-elements: `*` (and every real
		// tag) never matches one — only naming ::in/::out/::grep does.
		if n.class == "ref" || n.class == "fragment" {
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
	for _, n := range ordered(set) {
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
			for _, t := range ordered(tips) {
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

// isRecursive reports whether a callable directly calls itself, confirmed
// by a child LSP. It is edge-semantic and therefore ONLY sound with the
// LSP: a lexical self-edge is a name collision (`func Write` calling
// `w.Write` is io.Writer's method, not itself). A self-edge already
// resolved to n by the precision pass (conf lsp) is trusted; a name-unique
// self-edge (conf lexical — never LSP-checked at build) is re-resolved
// here. When no LSP can confirm, the func is NOT flagged and the query
// records that :recursive was under-resolved (see confirmSelfEdge).
func (e *engine) isRecursive(n *treeNode) bool {
	switch n.class {
	case "func", "method", "ctor":
	default:
		return false // only callables call
	}
	for _, ref := range e.refNodes(n, "out") {
		if ref.refKind != "call" {
			continue
		}
		for _, far := range ref.refFar {
			if far == n && e.confirmSelfEdge(n, ref) {
				return true
			}
		}
	}
	return false
}

// argCount returns how many `argument` children a node declares — the
// structural signature size :arity filters on. A non-callable (no
// argument children) is zero, so :arity(0,0) is a valid "no-arg" test on
// anything.
func (e *engine) argCount(n *treeNode) int {
	c := 0
	for _, k := range e.kids(n) {
		if k.class == "argument" {
			c++
		}
	}
	return c
}

// pseudoHolds evaluates one filter pseudo against one node. The inner
// selector is RELATIVE to n (see selRel), so :any(func) asks about n's
// descendants and :any(:parents(S)) about n's own referrers.
func (e *engine) pseudoHolds(n *treeNode, ps *selPseudo) bool {
	switch ps.kind {
	case pseudoContains:
		return e.nodeContains(n, ps.grep)
	case pseudoArity:
		c := e.argCount(n)
		return c >= ps.arityLo && (ps.arityHi < 0 || c <= ps.arityHi)
	case pseudoRecursive:
		return e.isRecursive(n)
	case pseudoAnnotated:
		return e.nodeAnnotated(n, ps.grep)
	case pseudoWhere, pseudoAny:
		// :where and :any coincide while the set is tested tip-by-tip;
		// :where is documented as the filter (subset flows on), :any as
		// the ∃ claim. Kept distinct so path-level filtering can later
		// diverge without a grammar change.
		return len(e.evalList(ps.inner, n, false)) > 0
	case pseudoEmpty:
		return len(e.evalList(ps.inner, n, false)) == 0
	case pseudoNot:
		return !e.selfMatches(n, ps.inner)
	case pseudoIs:
		return e.selfMatches(n, ps.inner)
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
			if n != nil && !out[n] && e.spend(1) {
				out[n] = true
				next[n] = true
			}
		}
		for _, n := range ordered(frontier) {
			add(n.parent)
			// :parents is upstream — incoming edges only.
			for _, r := range e.refNodes(n, "in") {
				for _, src := range r.refFar {
					add(src)
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
// share edges) and carry refConf "lexical" (name-unique) or "unsettled"
// (ambiguous); a child-LSP precision pass can upgrade individual edges to
// "lsp" later without reshaping.

// refSite is one classified, non-declaration occurrence of a name.
type refSite struct {
	name      string
	line, col int
	kind      string // "call" | "type" | "import" | ""   (WHAT it is)
	pos       string // "return" | "param" | "field" | "var" | ""  (WHERE it sits)
	encl      string // dotted sym path of the innermost enclosing symbol
}

// refNodes materializes (once per direction) and returns n's generated
// ref children for ONE direction — "in" or "out".
//
// Direction is required, not a filter applied afterwards: the incoming
// half is 12-16x the cost of the outgoing one, so building both and
// discarding half is what put a single ::out over the whole budget.
func (e *engine) refNodes(n *treeNode, dir string) []*treeNode {
	switch n.class {
	case "project", "dir", "ref":
		return nil
	case "return":
		// A `return` node is a type PROJECTION, not an edge participant.
		// Its leaf is the return TYPE's name, so it answers to #Error the
		// same as the type's declaration — but it has no edges of its own;
		// building them would double every `#Type::in.type` (the type decl
		// AND each return projection both scanning the same sites). The
		// type USAGE it marks is already an incoming edge on the type.
		return nil
	}
	if dir == "out" {
		if !n.refsOutLoaded {
			n.refsOutLoaded = true
			e.buildOutRefs(n)
		}
		return n.refsOut
	}
	if !n.refsInLoaded {
		n.refsInLoaded = true
		e.buildInRefs(n)
	}
	return n.refsIn
}

// implementsRefs builds `.implements` edges for a type-like host, on demand,
// from the child LSP's textDocument/implementation. There is no lexical site
// for this relation, so it lives apart from the site-based refsIn/refsOut and
// is only built when a query NAMES `.implements` (one round-trip per host).
// `::in.implements` on an interface = its implementers; `::out.implements` on
// a concrete type = the interfaces it satisfies (gopls answers the fitting
// counterpart either way). Every edge is conf `lsp`; a far end outside the
// root becomes an external stub, like any resolved-out-of-root reference.
func (e *engine) implementsRefs(h *treeNode, dir string) []*treeNode {
	switch h.class {
	case "type", "interface", "struct", "class", "enum":
	default:
		return nil // only type-like symbols implement / are implemented
	}
	if e.implRefCache == nil {
		e.implRefCache = map[*treeNode]map[string][]*treeNode{}
	}
	if e.implRefCache[h] == nil {
		e.implRefCache[h] = map[string][]*treeNode{}
	}
	if refs, ok := e.implRefCache[h][dir]; ok {
		return refs
	}
	var refs []*treeNode
	defer func() { e.implRefCache[h][dir] = refs }()

	if h.nameAt[0] == 0 || !e.s.lspAvailable(h.abs) {
		e.implementsUnavailable = true
		return refs
	}
	e.ensureLSPCap()
	if e.lspLeft <= 0 {
		e.implementsUnavailable = true
		return refs
	}
	e.lspLeft--
	e.lspAsked++
	locs := e.s.resolveImplementations(h.abs, h.nameAt[0], h.nameAt[1])
	if len(locs) > 0 {
		e.lspResolved++
	}
	for _, loc := range locs {
		far := e.nodeForLoc(loc)
		if far == nil {
			continue
		}
		refs = append(refs, &treeNode{
			class: "ref", refDir: dir, refKind: "implements", refConf: refLSP,
			leaf: far.leaf, full: far.leaf,
			file: far.file, abs: far.abs, at: [2]int{loc.line, loc.line},
			parent: h, depth: h.depth + 1, refFar: []*treeNode{far},
			fileOrd: h.fileOrd, symOrd: h.symOrd,
		})
	}
	return refs
}

// nodeForLoc maps an implementation target location to a tree node — the
// declared type at that line, or an external stub when it resolves outside
// the git root (a workspace type satisfying io.Reader; a stdlib type).
func (e *engine) nodeForLoc(loc implLoc) *treeNode {
	rel := relPath(loc.abs, e.s.getRoot())
	if !filepath.IsLocal(rel) {
		return externalStub(loc.abs, loc.line, identAt(loc.abs, loc.line, loc.col))
	}
	return e.declAt(rel, loc.line)
}

// declAt returns the innermost declared node in rel whose span contains
// line — how an implementation location resolves back to a workspace type.
func (e *engine) declAt(rel string, line int) *treeNode {
	f := e.fileByRel[rel]
	if f == nil {
		return nil
	}
	var best *treeNode
	var walk func(n *treeNode)
	walk = func(n *treeNode) {
		for _, c := range e.kids(n) {
			if c.sym == "" || c.at[0] > line || line > c.at[1] {
				continue
			}
			if best == nil || (c.at[1]-c.at[0]) < (best.at[1]-best.at[0]) {
				best = c
			}
			walk(c)
		}
	}
	walk(f)
	return best
}

// identAt reads the identifier at (line, col) — used to NAME an out-of-root
// implementation target for its external stub. Falls back to the file's base
// name (extension stripped) when the position doesn't sit on an identifier.
func identAt(abs string, line, col int) string {
	fallback := strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	content, err := os.ReadFile(abs)
	if err != nil {
		return fallback
	}
	lines := splitNodeReadLines(content)
	if line < 1 || line > len(lines) {
		return fallback
	}
	l := lines[line-1]
	i := col - 1
	if i < 0 || i >= len(l) {
		return fallback
	}
	j := i
	for j < len(l) && isIdentByte(l[j]) {
		j++
	}
	if j > i {
		return l[i:j]
	}
	return fallback
}

func isIdentByte(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// compHasClass reports whether a ref compound explicitly names a KIND class.
func compHasClass(comp *selCompound, kind string) bool {
	for _, c := range comp.refClasses {
		if c == kind {
			return true
		}
	}
	return false
}

// buildOutRefs: sites in n's own file whose innermost enclosing symbol
// is n itself — nesting attribution is the TREE SHAPE, so a closure's
// calls belong to the closure, and `>` vs space picks inner vs outer.
// Reads ONE file, so its cost is flat regardless of how common the
// names it mentions are.
func (e *engine) buildOutRefs(n *treeNode) {
	for _, site := range e.fileSites(n.file) {
		// Attribution is by enclosing sym path AND span containment. The
		// path alone is not unique — a `module main` (the package clause)
		// and a `func main` share it — so a name-only check hands the
		// same call site to both, double-counting the edge. The site
		// physically sits in exactly one of them.
		if site.encl != n.sym || site.line < n.at[0] || site.line > n.at[1] {
			continue
		}
		far := e.scopeDecls(e.declsOf(site.name), n.file, site.encl)
		if len(far) == 0 {
			continue
		}
		// Lexical scope narrowed the name-keyed candidates; a child LSP
		// settles whatever is still ambiguous (see precision.go).
		far, conf := e.refineFar(far, n.abs, site.line, site.col)
		n.refsOut = append(n.refsOut, &treeNode{
			class: "ref", refDir: "out", refKind: site.kind, refPos: site.pos, refConf: conf,
			refCol: site.col,
			leaf:   site.name, full: site.name,
			file: n.file, abs: n.abs, at: [2]int{site.line, site.line},
			parent: n, depth: n.depth + 1, refFar: far,
			fileOrd: n.fileOrd, symOrd: n.symOrd,
		})
	}
}

// buildInRefs: every site of n's NAME elsewhere; the far end is the
// site's innermost enclosing symbol (the source).
//
// This is the expensive half, and unavoidably so while edges are
// name-keyed: it must visit every occurrence of the name in the
// workspace, so a common name costs more than a rare one for the same
// symbol (#New = 93 occurrences = 1.78M work; #nodePath = 2 = 74k).
// The child-LSP edge-precision pass is what makes this a lookup rather
// than a sweep.
func (e *engine) buildInRefs(n *treeNode) {
	if n.sym == "" {
		return // files/project have no indexed name to be targeted by
	}
	idx := e.s.getIndex()
	if idx == nil {
		return
	}
	// The mirror of scopeDecls: if n is itself a LOCAL, the only sites
	// that can reference it are inside its own function. Every other
	// occurrence of the name is a coincidence, not a caller.
	owner, isLocal := localOwner(n)

	// Sites are offered because the NAME matches. If n is the only thing
	// answering to that name there is nothing to settle; otherwise a
	// site may belong to one of the others, and only an LSP can say.
	ambiguous := len(e.declsOf(n.leaf)) > 1

	for _, site := range idx.LookupExisting(n.leaf) {
		rel := relPath(site.File, e.s.getRoot())
		// An import is file-scoped by language semantics: it is only
		// ever the far end of sites in ITS OWN file. This is what makes
		// `import#huma::in.call` a per-file dependency-usage query
		// instead of name-keyed noise.
		if n.class == "import" && rel != n.file {
			continue
		}
		if isLocal && rel != n.file {
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
		if isLocal && !inScopeOf(hit.encl, owner) {
			continue // same file, but a different function's body
		}
		src := e.nodeByAddr(rel, hit.encl, hit.line)
		if src == nil {
			continue
		}
		fnode := e.fileByRel[rel]
		keep, conf := e.refineIn(n, fnode.abs, hit.line, hit.col, ambiguous)
		if !keep {
			continue // the LSP says this site refers to a different decl
		}
		n.refsIn = append(n.refsIn, &treeNode{
			class: "ref", refDir: "in", refKind: hit.kind, refPos: hit.pos, refConf: conf,
			refCol: hit.col,
			leaf:   n.leaf, full: n.leaf,
			file: rel, abs: fnode.abs, at: [2]int{hit.line, hit.line},
			parent: n, depth: n.depth + 1, refFar: []*treeNode{src},
			fileOrd: n.fileOrd, symOrd: n.symOrd,
		})
	}
}

// sitesByFile returns the index's file → sites inversion, keyed by
// ABSOLUTE file path. The index owns and memoizes this on its generation
// (symbols.Index.SitesByFile), so the FIRST edge query in a session builds
// it and every later query against an unchanged index reuses it free.
//
// It used to be rebuilt per query here — a whole-workspace sweep that
// spent one budget unit per occurrence and measured as ~85% of an
// anchored edge query's work. Moving it to index-owned derived state
// (like the estimator tallies) drops that per-query floor to zero and
// stops charging query budget for work that is not the query's.
func (e *engine) sitesByFile() map[string][]symbols.InvSite {
	idx := e.s.getIndex()
	if idx == nil {
		return nil
	}
	return idx.SitesByFile()
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
	// Only THIS file's occurrences, straight from the inversion (abs-keyed)
	// — the symbol-dependent work below stays scoped to one file.
	for _, raw := range e.sitesByFile()[fnode.abs] {
		if e.isDeclSite(fnode.abs, raw.Line, raw.Col, raw.Name, e.symCache) {
			continue
		}
		out = append(out, refSite{
			name: raw.Name, line: raw.Line, col: raw.Col,
			encl: e.s.enclosingSymPath(fnode.abs, raw.Line, e.symCache),
		})
	}
	// Classify all of the file's sites in ONE parse.
	if content, err := os.ReadFile(fnode.abs); err == nil {
		positions := make([][2]int, len(out))
		for i, s := range out {
			positions[i] = [2]int{s.line, s.col}
		}
		classes := symbols.SiteKinds(e.s.languageForFile(fnode.abs), content, positions)
		for i := range out {
			out[i].kind = classes[i].Kind
			out[i].pos = classes[i].Pos
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

// localOwner reports the function-like symbol a decl is LOCAL to, if
// any. Parameters (and anything else a language nests inside a function
// body) hang off their function in the tree, so the tree already knows
// this — it is lexical scope, not a heuristic.
func localOwner(d *treeNode) (string, bool) {
	for p := d.parent; p != nil; p = p.parent {
		switch p.class {
		case "func", "method", "ctor":
			return p.sym, true
		case "file", "dir", "project":
			return "", false
		}
	}
	return "", false
}

// inScopeOf reports whether a site enclosed by `encl` can see a local
// declared in `owner`. Equal means the same body; the prefix covers a
// closure nested inside it, which CAN capture the local.
func inScopeOf(encl, owner string) bool {
	return encl == owner || strings.HasPrefix(encl, owner+".")
}

// scopeDecls drops decls that the site at (file, encl) cannot actually
// see, keeping the name-keyed candidates the index offers honest.
//
//   - An import is file-scoped, so `huma.Register(...)` resolves to THIS
//     file's `import#huma`, never a sibling's.
//   - A LOCAL (a parameter, a closure's binding) is visible only from
//     inside its own function. Name-keying without this made every
//     function's `t` an edge to every OTHER function's `t`: measured
//     across func::out, 1,250,227 of 1,263,196 far ends (99.0%) pointed
//     at a local of some other function, 96% of them parameters. One
//     edge claimed 492 far ends because 492 tests take a `t`.
//
// Everything else stays name-keyed across the workspace — that residue
// is what a child-LSP pass is for.
func (e *engine) scopeDecls(decls []*treeNode, file, encl string) []*treeNode {
	out := decls[:0:0]
	for _, d := range decls {
		if d.class == "import" && d.file != file {
			continue
		}
		if owner, ok := localOwner(d); ok {
			if d.file != file || !inScopeOf(encl, owner) {
				continue
			}
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
			if !e.spend(1) {
				return
			}
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
	for _, id := range attrAxisValues(n, a.axis) {
		if a.op == selRegex {
			if a.re != nil && a.re.MatchString(id) {
				return true
			}
			continue
		}
		if matchAttrOp(id, a.op, a.value) {
			return true
		}
	}
	return false
}

// attrAxisValues returns the strings one axis tests against.
func attrAxisValues(n *treeNode, axis selAttrAxis) []string {
	switch axis {
	case attrName:
		return n.nameIDs()
	case attrPath:
		if p := nodePath(n); p != "" {
			return []string{p}
		}
		return nil
	}
	return n.nodeIDs() // attrID — `#id`, addresses included
}

func matchAttrOp(id string, op selOp, want string) bool {
	switch op {
	case selExact:
		return id == want
	case selPrefix:
		return strings.HasPrefix(id, want)
	case selSuffix:
		return strings.HasSuffix(id, want)
	case selContains:
		return strings.Contains(id, want)
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

// annotatedScanLimit bounds how far above a declaration :annotated
// looks. A blank line ends the block first in almost every case; this
// only guards a pathological run of unbroken comment lines.
const annotatedScanLimit = 64

// nodeAnnotated is the :annotated predicate — it greps the contiguous
// non-blank block IMMEDIATELY ABOVE the declaration, where decorators
// (@app.route), annotations (@Deprecated), and attached doc comments
// live. That block is OUTSIDE the node's own span, so :contains (which
// greps the body) never sees it — hence a separate predicate. This is
// what answers "who is annotated with X": the SYMBOL carrying the
// annotation, not the comment line ::grep would return.
//
// The block is the leading trivia: walking up from the decl's first
// line until a blank line. Stacked decorators have no blank between
// them, and a doc comment attached to a decl has none either, so one
// contiguous run captures both.
func (e *engine) nodeAnnotated(n *treeNode, g *grepSpec) bool {
	switch n.class {
	case "project", "dir", "file":
		return false // no declaration line to sit beneath annotations
	}
	content, err := os.ReadFile(n.abs)
	if err != nil {
		return false
	}
	all := splitNodeReadLines(content)
	declIdx := n.at[0] - 1 // 0-based index of the decl's first line
	if declIdx < 0 || declIdx > len(all) {
		return false
	}
	// Above the span: decorators/annotations/doc a language leaves
	// OUTSIDE the declaration (Python, TS). A blank line ends the block.
	for i := declIdx - 1; i >= 0 && declIdx-i <= annotatedScanLimit; i-- {
		if strings.TrimSpace(all[i]) == "" {
			break
		}
		if g.matchLine(all[i]) {
			return true
		}
	}
	// Inside the span: some languages fold the doc comment INTO the
	// declaration (Go attributes `// Deprecated:` to the func, so at[0]
	// is the comment line). Scan the leading trivia until the real code.
	for i := declIdx; i < len(all) && i-declIdx <= annotatedScanLimit; i++ {
		if !isTriviaLine(all[i]) {
			break // reached the declaration keyword — trivia is over
		}
		if g.matchLine(all[i]) {
			return true
		}
	}
	return false
}

// isTriviaLine reports whether a line is leading trivia — a decorator,
// annotation, comment, or blank — rather than code. Prefix-based and
// language-agnostic: it only decides where a declaration's annotation
// block ends, not what the trivia means.
func isTriviaLine(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return true
	}
	switch t[0] {
	case '@', '#', '*': // decorator/annotation, hash comment, block-comment body
		return true
	}
	return strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*")
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
		// A `return` node is a PROJECTION of a type usage — its name span
		// sits on the return-type occurrence, which is a reference, not a
		// declaration. Counting it here would delete that ref (and misfile
		// the position axis). It declares nothing the name-keyed edge index
		// should treat as a decl.
		if sym.Class == "return" {
			continue
		}
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
	return fmt.Errorf("unsupported flag %q — supported: -i -w -E -F -v -n, and -A<n>/-B<n>/-C<n> in ::grep. The selector scopes the search, so -r and file arguments are never accepted", "-"+string(c))
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
// parseGrepPseudoSpec reads `('<pattern>')` for a boolean grep pseudo
// (:contains, :annotated) and compiles it. Both quotes work, ' preferred
// — the selector rides inside a JSON string, so "x" costs an escaping
// layer 'x' does not.
func (p *modSelParser) parseGrepPseudoSpec(name string) (*grepSpec, error) {
	if p.peek() != '(' {
		return nil, p.errf("'(' after :" + name)
	}
	p.i++
	p.skipWS()
	q := p.peek()
	if q != '"' && q != '\'' {
		return nil, p.errf("a quoted string, e.g. :" + name + "('TODO')")
	}
	p.i++
	start := p.i
	for !p.eof() && p.s[p.i] != q {
		p.i++
	}
	if p.eof() {
		return nil, p.errf(fmt.Sprintf("a closing %c for :%s", q, name))
	}
	text := string(p.s[start:p.i])
	p.i++
	p.skipWS()
	if p.peek() != ')' {
		return nil, p.errf("')' to close :" + name)
	}
	p.i++
	g, err := parseContainsSpec(text)
	if err != nil {
		return nil, fmt.Errorf("bad :%s(%q): %w", name, text, err)
	}
	return g, nil
}

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

// grepHit is one matched line plus its context, in the before/after
// convention symbols.Search already uses.
type grepHit struct {
	Line   int      `json:"line"`
	Text   string   `json:"text"`
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}
