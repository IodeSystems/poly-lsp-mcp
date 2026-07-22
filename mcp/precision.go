package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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

// defaultLSPResolveCap bounds LSP round-trips per query. A round-trip is
// ~50-100ms, and a broad selector has thousands of ambiguous edges
// (func::out: 9,080) — uncapped that is minutes. Past the cap the
// remaining edges stay lexical and say so, which is the same contract
// the work budget already uses: partial, flagged, never silent.
const defaultLSPResolveCap = 200

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
	uri := pathToURI(abs)
	child := s.manager.RouteByURI(uri)
	if child == nil {
		return "", 0, false // tree-sitter-only language
	}
	// gopls answers about files it has been told about. didOpen is
	// idempotent per session (openDocs), so this is a no-op once warm.
	content, err := os.ReadFile(abs)
	if err != nil {
		return "", 0, false
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
		return "", 0, false
	}
	return firstDefinition(raw)
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
		// The LSP pointed outside the modelled tree (stdlib, vendor,
		// generated). Narrowing to nothing would DELETE a real edge, so
		// the candidates stand — but which one (if any) is right is
		// unknown, so this is unsettled, not certain.
		return far, refUnsettled
	}
	e.lspResolved++
	return []*treeNode{picked}, refLSP
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
			parts = append(parts, fmt.Sprintf("%d ambiguous edge(s) settled by a child "+
				"LSP, then the per-query cap ran out — remaining ambiguous edges are "+
				"UNSETTLED (name-keyed candidates, not resolved references). Narrow the "+
				"selector to bring the rest under the cap.", e.lspResolved))
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
