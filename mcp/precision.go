package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// The child-LSP precision pass: upgrading name-keyed guesses to answers.
//
// Edges are generated from the lexical index, which keys on the NAME, so
// a site can offer several candidate far ends — a field called `s` on
// three structs, a `Server` in two packages. Lexical scope already threw
// out the impossible ones (see scopeDecls: 99% of far ends were another
// function's locals). What remains is genuine cross-package collision,
// and only a real language server can settle it.
//
// Two rules keep this honest and affordable:
//
//   - ASK ONLY WHEN UNSURE. One candidate is not a guess, so it costs
//     nothing. Only an ambiguous edge (>1 candidate) is worth a
//     round-trip, and after the scope fix the average edge has 2.58.
//
//   - NARROW, NEVER INVENT. The LSP's answer is used to PICK from the
//     candidates lexical already found. If it points somewhere this tree
//     does not model — the stdlib, a vendored module, generated code —
//     the lexical candidates stand. A precision pass that could add
//     edges would be a second, unreviewed graph builder.
//
// Everything here degrades: no manager (the `query` CLI), no child for
// the language (tree-sitter-only), a timeout, a dead child. It degrades
// LOUDLY — refConf carries one of three states per edge, and the result
// says how many of each it is made of.

// An edge's refConf says what settled it — never decoration: a caller
// acting on "who calls Save" needs to know whether that is a language
// server's answer, a name that is unique anyway, or an unsettled guess.
//
// The split matters: "lexical" and "unsettled" were once one value, but
// they are opposites. A name-unique edge is CERTAIN without an LSP; an
// ambiguous edge no LSP could settle is a GUESS listing candidates. On a
// transitive walk the per-query cap is spent shallowest-first, so the
// unsettled edges cluster at the deep hops — and only a distinct value
// can say "these distant nodes are the least certain."
const (
	refLexical   = "lexical"   // name is UNIQUE in the workspace — certain without an LSP
	refLSP       = "lsp"       // a child LSP picked the far end
	refUnsettled = "unsettled" // >1 same-named decl, no LSP resolution — a guess (lists candidates)
)

// The LSP round-trip cap bounds per-query resolution. A round-trip is
// ~50-100ms, and a broad selector has thousands of ambiguous edges
// (func::out: 9,080) — uncapped that is minutes. Past the cap the
// remaining edges stay lexical and say so, which is the same contract
// the work budget already uses: partial, flagged, never silent.
//
// It is TUNED to the workspace, Timsort-style — cheap when the input has
// structure. The only edges that cost a round-trip are AMBIGUOUS ones (a
// name several decls answer to); names are Zipfian, so most are unique and
// resolve for free, and the cap only has to cover the collision-prone tail.
// So the cap scales with that tail — the count of declared names with >=2
// declarations — from a floor (a clean repo still resolves its rare
// collision) to a ceiling (cost stays predictable, the explainable-cost
// moat). An explicit SetLSPResolveCap overrides the tuning.
const (
	lspCapFloor   = 64   // a near-collision-free workspace still gets this
	lspCapPerName = 4    // round-trips to budget per collision-prone name
	lspCapCeil    = 1500 // ceiling — bounded cost regardless of workspace size
)

// tunedLSPCap maps the count of collision-prone (ambiguous) declared names
// to a per-query round-trip budget.
func tunedLSPCap(ambiguous int) int {
	c := lspCapFloor + lspCapPerName*ambiguous
	if c > lspCapCeil {
		c = lspCapCeil
	}
	return c
}

// ensureLSPCap sets the per-query round-trip budget on first use. An
// explicit server cap wins; otherwise it is tuned from the workspace
// collision rate. declsByName is already built by the edge that triggers
// this (buildOutRefs/buildInRefs call declsOf first), so the count is free.
func (e *engine) ensureLSPCap() {
	if e.lspCapReady {
		return
	}
	e.lspCapReady = true
	if e.s.lspResolveCap > 0 {
		e.lspLeft, e.lspCapChosen = e.s.lspResolveCap, e.s.lspResolveCap
		return
	}
	e.declsOf("") // ensure declsByName is built (no-op once it is)
	for _, decls := range e.declsByName {
		// Count only EDGE-TARGETABLE declarations: a name-collision costs a
		// round-trip only when a call/type site could resolve to more than
		// one of them. Synthetic leaves (params, return types, annotations)
		// aren't edge targets, so a param named `x` in many funcs is not a
		// collision that the cap has to cover.
		real := 0
		for _, d := range decls {
			switch d.class {
			case "argument", "return", "annotation":
			default:
				real++
			}
		}
		if real == 0 {
			continue
		}
		e.collTotal++
		if real >= 2 {
			e.collAmbiguous++
		}
	}
	e.capTuned = true
	e.lspLeft = tunedLSPCap(e.collAmbiguous)
	e.lspCapChosen = e.lspLeft
}

// lspResolveTimeout bounds ONE definition round-trip. gopls answers a
// warm file in single-digit ms; a slow one is not worth stalling a query
// that has a usable lexical answer in hand.
const lspResolveTimeout = 2 * time.Second

// lspLocation is the subset of the LSP definition reply we need. gopls
// may answer Location, []Location, or []LocationLink depending on client
// capabilities, so all three shapes are decoded.
type lspLocation struct {
	URI   string `json:"uri"`
	Range struct {
		Start struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"start"`
	} `json:"range"`
}

type lspLocationLink struct {
	TargetURI   string `json:"targetUri"`
	TargetRange struct {
		Start struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"start"`
	} `json:"targetRange"`
}

// lspAvailable reports whether a child LSP could answer for this file at
// all. Cheap enough to gate on before spending a cap slot.
func (s *Server) lspAvailable(abs string) bool {
	if s.manager == nil {
		return false
	}
	return s.manager.RouteByURI(pathToURI(abs)) != nil
}

// resolveDefinition asks the child LSP owning `abs` where the symbol at
// (line, col) — 1-BASED, as everything in this package is — is declared.
// Returns the declaration's absolute path and 1-based line.
//
// ok=false means "no answer", never "no definition": every failure path
// (no manager, no child, not open, timeout, unparseable reply, a reply
// pointing outside the workspace) lands here and leaves the caller on
// its lexical answer.
func (s *Server) resolveDefinition(abs string, line, col int) (defAbs string, defLine int, ok bool) {
	if s.manager == nil {
		return "", 0, false
	}
	// A warm session asks about the same site many times (an ::in target's
	// N callers, a :recursive self-call). Memoize per index generation.
	gen := uint64(0)
	if idx := s.getIndex(); idx != nil {
		gen = idx.Generation()
	}
	key := fmt.Sprintf("%s\x00%d\x00%d", abs, line, col)
	if d, hit := s.defCacheGet(gen, key); hit {
		return d.defAbs, d.defLine, d.found
	}
	d := s.resolveDefinitionUncached(abs, line, col)
	s.defCachePut(gen, key, d)
	return d.defAbs, d.defLine, d.found
}

// defEntry is one memoized textDocument/definition answer. found mirrors
// resolveDefinition's ok — a no-answer is cached too, so a site that does
// not resolve is not re-asked within the same generation.
type defEntry struct {
	defAbs  string
	defLine int
	found   bool
}

// defCacheGet returns the cached answer for key, dropping the whole cache
// first if the index has moved on (a mutation ⇒ definitions may have
// shifted). The LSP round-trip runs OUTSIDE this lock, so it never
// serializes concurrent queries.
func (s *Server) defCacheGet(gen uint64, key string) (defEntry, bool) {
	s.defCacheMu.Lock()
	defer s.defCacheMu.Unlock()
	if s.defCacheGen != gen || s.defCache == nil {
		s.defCache = map[string]defEntry{}
		s.defCacheGen = gen
		return defEntry{}, false
	}
	d, ok := s.defCache[key]
	return d, ok
}

// defCachePut stores an answer, but only if the generation still matches —
// a concurrent mutation may have dropped the cache while the round-trip
// was in flight, and a stale-gen answer must not poison the fresh cache.
func (s *Server) defCachePut(gen uint64, key string, d defEntry) {
	s.defCacheMu.Lock()
	defer s.defCacheMu.Unlock()
	s.defMisses++ // one real round-trip happened to produce d
	if s.defCacheGen == gen && s.defCache != nil {
		s.defCache[key] = d
	}
}

func (s *Server) resolveDefinitionUncached(abs string, line, col int) defEntry {
	uri := pathToURI(abs)
	child := s.manager.RouteByURI(uri)
	if child == nil {
		return defEntry{} // tree-sitter-only language
	}
	// gopls answers about files it has been told about. didOpen is
	// idempotent per session (openDocs), so this is a no-op once warm.
	content, err := os.ReadFile(abs)
	if err != nil {
		return defEntry{}
	}
	s.notifyChildOfOpen(child, uri, content)

	ctx, cancel := context.WithTimeout(context.Background(), lspResolveTimeout)
	defer cancel()
	// LSP positions are 0-based; ours are 1-based.
	raw, err := child.Call(ctx, "textDocument/definition", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line - 1, "character": col - 1},
	})
	if err != nil {
		return defEntry{}
	}
	da, dl, ok := firstDefinition(raw)
	return defEntry{defAbs: da, defLine: dl, found: ok}
}

// implLoc is one resolved implementation target — a declaration's file and
// 1-based line/col (col names an out-of-root target for its external stub).
type implLoc struct {
	abs  string
	line int
	col  int
}

// resolveImplementations asks the child LSP for textDocument/implementation
// at a type's name — the concrete types implementing an interface, or the
// interfaces a type satisfies (gopls answers whichever counterpart fits the
// symbol). Unlike definition it returns MANY targets. Memoized per index
// generation, same discipline as resolveDefinition. nil on any no-answer
// path (no manager/child, timeout, unparseable) — .implements then degrades
// to "unavailable, and says so".
func (s *Server) resolveImplementations(abs string, line, col int) []implLoc {
	if s.manager == nil {
		return nil
	}
	gen := uint64(0)
	if idx := s.getIndex(); idx != nil {
		gen = idx.Generation()
	}
	key := fmt.Sprintf("impl\x00%s\x00%d\x00%d", abs, line, col)
	if v, hit := s.implCacheGet(gen, key); hit {
		return v
	}
	v := s.resolveImplementationsUncached(abs, line, col)
	s.implCachePut(gen, key, v)
	return v
}

func (s *Server) implCacheGet(gen uint64, key string) ([]implLoc, bool) {
	s.implCacheMu.Lock()
	defer s.implCacheMu.Unlock()
	if s.implCacheGen != gen || s.implCache == nil {
		s.implCache = map[string][]implLoc{}
		s.implCacheGen = gen
		return nil, false
	}
	v, ok := s.implCache[key]
	return v, ok
}

func (s *Server) implCachePut(gen uint64, key string, v []implLoc) {
	s.implCacheMu.Lock()
	defer s.implCacheMu.Unlock()
	if s.implCacheGen == gen && s.implCache != nil {
		s.implCache[key] = v
	}
}

func (s *Server) resolveImplementationsUncached(abs string, line, col int) []implLoc {
	uri := pathToURI(abs)
	child := s.manager.RouteByURI(uri)
	if child == nil {
		return nil
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil
	}
	s.notifyChildOfOpen(child, uri, content)
	ctx, cancel := context.WithTimeout(context.Background(), lspResolveTimeout)
	defer cancel()
	raw, err := child.Call(ctx, "textDocument/implementation", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line - 1, "character": col - 1},
	})
	if err != nil {
		return nil
	}
	return allLocations(raw)
}

// allLocations decodes every target from a Location / []Location /
// []LocationLink reply (the multi-target sibling of firstDefinition).
func allLocations(raw json.RawMessage) []implLoc {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var out []implLoc
	var locs []lspLocation
	if err := json.Unmarshal(raw, &locs); err == nil && len(locs) > 0 {
		for _, l := range locs {
			out = append(out, implLoc{uriToPath(l.URI), l.Range.Start.Line + 1, l.Range.Start.Character + 1})
		}
		return out
	}
	var one lspLocation
	if err := json.Unmarshal(raw, &one); err == nil && one.URI != "" {
		return []implLoc{{uriToPath(one.URI), one.Range.Start.Line + 1, one.Range.Start.Character + 1}}
	}
	var links []lspLocationLink
	if err := json.Unmarshal(raw, &links); err == nil && len(links) > 0 {
		for _, l := range links {
			out = append(out, implLoc{uriToPath(l.TargetURI), l.TargetRange.Start.Line + 1, l.TargetRange.Start.Character + 1})
		}
	}
	return out
}

// firstDefinition decodes whichever of the three legal reply shapes the
// child sent and returns the first target.
func firstDefinition(raw json.RawMessage) (string, int, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", 0, false
	}
	var locs []lspLocation
	if err := json.Unmarshal(raw, &locs); err == nil && len(locs) > 0 {
		return uriToPath(locs[0].URI), locs[0].Range.Start.Line + 1, true
	}
	var one lspLocation
	if err := json.Unmarshal(raw, &one); err == nil && one.URI != "" {
		return uriToPath(one.URI), one.Range.Start.Line + 1, true
	}
	var links []lspLocationLink
	if err := json.Unmarshal(raw, &links); err == nil && len(links) > 0 {
		return uriToPath(links[0].TargetURI), links[0].TargetRange.Start.Line + 1, true
	}
	return "", 0, false
}

// refineFar narrows a site's candidate far ends to the one the child LSP
// actually resolves it to, and reports the confidence to record.
//
// It is a no-op — returning the candidates untouched and "lexical" —
// whenever there is nothing to settle (0 or 1 candidate), no LSP to ask,
// or the cap is spent. The cap is only charged when a round-trip really
// happens.
func (e *engine) refineFar(far []*treeNode, siteAbs string, line, col int) ([]*treeNode, string) {
	if len(far) < 2 {
		return far, refLexical // one candidate — name-unique, certain
	}
	e.ensureLSPCap()
	if e.lspLeft <= 0 || !e.s.lspAvailable(siteAbs) {
		return far, refUnsettled // ambiguous, nothing to settle it — a guess
	}
	e.lspLeft--
	e.lspAsked++
	defAbs, defLine, ok := e.s.resolveDefinition(siteAbs, line, col)
	if !ok {
		return far, refUnsettled
	}
	defRel := relPath(defAbs, e.s.getRoot())
	picked := pickByDefinition(far, defRel, defLine)
	if picked == nil {
		if !filepath.IsLocal(defRel) {
			// The LSP resolved OUTSIDE the git root — the stdlib, a
			// dependency, a vendored module. The real target is external,
			// so none of the local candidates is right. Mint an honest
			// EXTERNAL STUB (nameable, read-only, [not indexed]) as the far
			// end instead of leaving a false local. This is a CONFIDENT
			// resolution (conf: lsp) to a boundary node (North Star Stage 0).
			e.lspResolved++
			return []*treeNode{externalStub(defAbs, defLine, far[0].leaf)}, refLSP
		}
		// Inside the root but no candidate span matched (a decl the tree
		// did not model): genuinely unsettled, not certain.
		return far, refUnsettled
	}
	e.lspResolved++
	return []*treeNode{picked}, refLSP
}

// externalStub mints a read-only far-end node for a symbol a child LSP
// resolved OUTSIDE the git root. Its identity is `module@version#sym`
// (version omitted when unknown), derived best-effort from the resolved
// path — always nameable so an external edge is never a false local. It
// is domain:"external", never indexed, and carries the real path in `abs`
// for a potential read.
func externalStub(defAbs string, defLine int, name string) *treeNode {
	module, version := externalIdentity(defAbs)
	id := name
	if module != "" {
		id = module
		if version != "" {
			id += "@" + version
		}
		id += "#" + name
	}
	return &treeNode{
		class:  "external",
		domain: "external",
		leaf:   name,
		full:   id,
		abs:    defAbs,
		at:     [2]int{defLine, defLine},
	}
}

// externalIdentity derives a `module`, `version` pair from an absolute
// path a child LSP resolved outside the workspace. Best-effort per
// ecosystem (Go module cache carries @version; stdlib / node_modules /
// site-packages carry the package path), with a last-resort fallback to
// the containing directory — so the result is ALWAYS nameable and clearly
// not a workspace file.
func externalIdentity(defAbs string) (module, version string) {
	p := filepath.ToSlash(defAbs)
	if i := strings.LastIndex(p, "/pkg/mod/"); i >= 0 {
		return splitModuleVersion(path.Dir(p[i+len("/pkg/mod/"):]))
	}
	if i := strings.LastIndex(p, "/node_modules/"); i >= 0 {
		return firstNodePackage(p[i+len("/node_modules/"):]), ""
	}
	if i := strings.LastIndex(p, "/site-packages/"); i >= 0 {
		return firstSegment(p[i+len("/site-packages/"):]), ""
	}
	if i := strings.LastIndex(p, "/src/"); i >= 0 {
		// GOROOT/GOPATH stdlib-shaped: .../src/<pkg>/file.go. No version
		// on disk here, so leave it blank (still nameable: strings#Split).
		return path.Dir(p[i+len("/src/"):]), ""
	}
	return path.Base(path.Dir(p)), "" // last resort — the containing dir
}

// splitModuleVersion parses a Go module-cache subpath
// (`github.com/foo/bar@v1.2.3/sub`) into module + version, folding any
// subpackage back onto the module path.
func splitModuleVersion(s string) (module, version string) {
	at := strings.Index(s, "@")
	if at < 0 {
		return s, ""
	}
	module = s[:at]
	rest := s[at+1:]
	if slash := strings.Index(rest, "/"); slash >= 0 {
		version = rest[:slash]
		module += "/" + rest[slash+1:]
	} else {
		version = rest
	}
	return module, version
}

// firstNodePackage returns the package name from a node_modules subpath,
// keeping a leading @scope (@babel/core), else the first segment.
func firstNodePackage(s string) string {
	parts := strings.SplitN(s, "/", 3)
	if len(parts) >= 2 && strings.HasPrefix(parts[0], "@") {
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

// firstSegment returns the first path segment (or the file's base name,
// extension stripped, when there is no separator).
func firstSegment(s string) string {
	if i := strings.Index(s, "/"); i >= 0 {
		return s[:i]
	}
	return strings.TrimSuffix(s, path.Ext(s))
}

// refineIn settles an INCOMING edge, where the ambiguity has a different
// shape: the far end (the site's enclosing symbol) is never in doubt —
// what is in doubt is whether the site refers to `target` at all. The
// index offered it because the NAME matched, so when several
// declarations share that name, the site may belong to one of the
// others.
//
// The LSP's definition for the site is conclusive: if it is not
// target's declaration, this site is not a reference to target, and the
// edge is a coincidence. That holds even when the real definition is
// something this tree does not model (the stdlib) — "it is that one"
// still means "it is not this one".
//
// keep=true is the safe answer for every no-answer path: unasked,
// uncapped, timed out, unparseable.
func (e *engine) refineIn(target *treeNode, siteAbs string, line, col int, ambiguous bool) (keep bool, conf string) {
	if !ambiguous {
		return true, refLexical // only one decl answers to this name — certain
	}
	e.ensureLSPCap()
	if e.lspLeft <= 0 || !e.s.lspAvailable(siteAbs) {
		return true, refUnsettled // ambiguous, nothing to settle it — a guess
	}
	e.lspLeft--
	e.lspAsked++
	defAbs, defLine, ok := e.s.resolveDefinition(siteAbs, line, col)
	if !ok {
		return true, refUnsettled
	}
	e.lspResolved++
	defRel := relPath(defAbs, e.s.getRoot())
	if defRel == target.file && defLine >= target.at[0] && defLine <= target.at[1] {
		return true, refLSP
	}
	return false, refLSP // resolves to a different declaration
}

// confirmSelfEdge decides whether an out.call self-edge (far end == n) is a
// REAL recursive call, not a name collision. If the precision pass already
// resolved this edge to n (conf lsp), it is trusted. Otherwise — a
// name-unique or unsettled self-edge, never LSP-checked at build — the site
// is resolved now and confirmed only if the definition lands inside n's own
// span. With no LSP to ask, it returns false and marks the query
// under-resolved, so :recursive degrades to "cannot confirm", never to a
// silent false negative dressed as a real answer.
func (e *engine) confirmSelfEdge(n *treeNode, ref *treeNode) bool {
	if ref.refConf == refLSP {
		return true // the precision pass already resolved this edge to n
	}
	e.ensureLSPCap()
	if e.lspLeft <= 0 || !e.s.lspAvailable(ref.abs) {
		e.recursiveUnconfirmed = true
		return false
	}
	e.lspLeft--
	e.lspAsked++
	defAbs, defLine, ok := e.s.resolveDefinition(ref.abs, ref.at[0], ref.refCol)
	if !ok {
		e.recursiveUnconfirmed = true
		return false
	}
	defRel := relPath(defAbs, e.s.getRoot())
	if defRel == n.file && defLine >= n.at[0] && defLine <= n.at[1] {
		e.lspResolved++
		return true
	}
	return false // resolves elsewhere (an interface method, another decl) — not itself
}

// precisionNote reports what this query's edges are made of, or "" when
// there is nothing precision-relevant to say. Three facts can appear: how
// many ambiguous edges an LSP settled (and whether the cap ran out), and
// — for a transitive walk — at which HOP the unsettled edges begin, since
// the cap is spent shallowest-first and distant nodes are the least
// certain. It says what IS precise and what wasn't.
func (e *engine) precisionNote() string {
	var parts []string
	if e.lspAsked > 0 || e.lspResolved > 0 {
		if e.lspLeft <= 0 {
			cap := "the per-query cap"
			if e.capTuned {
				cap = fmt.Sprintf("the per-query cap (%d, tuned from %d collision-prone "+
					"names of %d)", e.lspCapChosen, e.collAmbiguous, e.collTotal)
			}
			parts = append(parts, fmt.Sprintf("%d ambiguous edge(s) settled by a child "+
				"LSP, then %s ran out — remaining ambiguous edges are UNSETTLED (name-keyed "+
				"candidates, not resolved references). Narrow the selector to bring the rest "+
				"under the cap.", e.lspResolved, cap))
		} else {
			parts = append(parts, fmt.Sprintf("%d ambiguous edge(s) settled by a child "+
				"LSP; the rest were name-unique (lexical) or unsettled.", e.lspResolved))
		}
	}
	if e.maxHopReached > 1 {
		if e.transUnsettled > 0 {
			parts = append(parts, fmt.Sprintf("This walk crossed up to %d hops; %d "+
				"unsettled edge(s) begin at hop %d — the LSP cap is spent shallowest-first, "+
				"so distant nodes are the least certain.", e.maxHopReached, e.transUnsettled,
				e.unsettledFromHop))
		} else {
			parts = append(parts, fmt.Sprintf("This walk crossed up to %d hops, all "+
				"LSP-resolved or name-unique.", e.maxHopReached))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	parts = append(parts, "Each edge's conf says which: lsp (resolved) | lexical "+
		"(name-unique, certain) | unsettled (a guess).")
	return strings.Join(parts, " ")
}

// pickByDefinition finds the candidate whose source span contains the
// declaration position the LSP named. Innermost wins, so a parameter
// beats the function it sits in.
func pickByDefinition(far []*treeNode, defRel string, defLine int) *treeNode {
	var best *treeNode
	for _, d := range far {
		if d.file != defRel || defLine < d.at[0] || defLine > d.at[1] {
			continue
		}
		if best == nil || (d.at[1]-d.at[0]) < (best.at[1]-best.at[0]) {
			best = d
		}
	}
	return best
}
