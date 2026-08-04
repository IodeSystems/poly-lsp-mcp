package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// registerModernTools returns the default 3-tool MCP surface:
// node_query (find), node_read (read), node_edit (write/delete). It
// replaces the legacy 9-tool surface (structure, node_query,
// node_references, node_read, node_edit, node_delete, node_refactor,
// search, node_rename_file) — see server.go's SetLegacyTools.
//
// MCP re-sends every tool's schema on every turn, which LOOKS like a per-turn
// cost and isn't: llama.cpp reuses the KV slot, so a byte-identical prefix is
// sent every turn and evaluated once (measured: 83% of prompt tokens served
// from cache). The 9→3 cut was originally justified by summing per-turn
// prompt_tokens — an accounting artifact that bills cache hits at full price.
// Keep the surface small for ATTENTION and self-consistency, not for tokens.
//
// The deep grammar still lives in guided errors (selectorGrammarHelp) and
// selector "?" rather than on the wire — because a 441-token grammar dump per
// mistake taught nothing about the mistake, not because of its size.
//
// The low-level helpers (applyRangeRewrite, applyWholeFileWrite,
// applyWholeFileDelete, refactorRename, refactorSignature,
// buildReadPayload, readRangeText, …) are shared with the legacy
// surface unchanged; this file only adds the modern dispatch layer,
// and query.go the selector engine it drives.
func registerModernTools() map[string]Tool {
	return map[string]Tool{
		"node_query": {
			Name:        "node_query",
			Description: modernNodeQueryDescription,
			InputSchema: modernNodeQuerySchema,
			Handler:     handleModernNodeQuery,
		},
		"node_read": {
			Name:        "node_read",
			Description: modernNodeReadDescription,
			InputSchema: modernNodeReadSchema,
			Handler:     handleModernNodeRead,
		},
		"node_edit": {
			Name:        "node_edit",
			Description: modernNodeEditDescription,
			InputSchema: modernNodeEditSchema,
			Handler:     handleModernNodeEdit,
		},
	}
}

// The schemas below are deliberately MINIFIED. InputSchema is written
// to the wire verbatim, and MCP re-sends every tool definition on every
// turn, so pretty-printing indentation is a per-turn tax for zero
// information. Keep them dense.

const modernNodeQueryDescription = `CSS-inspired selector language over ONE tree — DAG: project > dir > file > symbols (dotted) > argument — plus the reference GRAPH as pseudo-elements. Files are nodes; no separate filesystem API.
TAGS are a FIXED set: project dir file func method type struct interface class const var field enum ctor module import argument, *. Workspace NAMES are NEVER tags, always #ids: #cache, #'store.go#Save' (quote with '). Language as class: file.go. space=descendant >=child ,=union — space is ALWAYS a node boundary. FILTER by bracketing ONTO an element: func[path=a.go] ≠ func path=a.go (things inside a func). Bare attr = its own *[…]. In [] | is OR, & AND, () groups, over whole TESTS: [name=a|name=b]; quote a pipe in a value: [path~='a|b'].
GRAPH: X::in who points at X / X::out what X's body points at; kind class .call/.type/.import. The far end is the edge's CHILD — cross with >. X::out = X's own edges, X ::out = nested symbols' too. {m,n} on an edge = edges crossed, {1,} = transitive.
::grep('flags pattern') = matched LINES as nodes (-i -w -E -F -v -A/-B/-C<n>; literal unless -E); no ctx by default (hit=the line), -A/-B/-C adds it, byte-bounded; result carries a per-file rollup.
FOOTGUNS: * NEVER matches ::edges or ::grep lines — name them or they're invisible. {m,n} elsewhere is regex REPETITION child-joined, NOT depth: func{2} = func>func; within-3-levels = "> *{0,2} > x". :not/:is(sel) test the node ITSELF; :where/:any/:all/:empty(sel) test AROUND it (leading tag = a descendant, leading ::/pseudo = the node; bare :any/:all/:empty judge their position). :parents(sel) = ALL upstream (ancestors + incoming refs) — broader than callers. Edge/::grep addresses are file@line: node_read/node_edit hit that exact line.
RECIPES: #'a.go#Save'::in.call callers | #'main'::out.call > * callees | func:not([name^=Test]):empty(::in) dead code | import#huma::in.call::grep('-E (Get|Post)\(') endpoints | :root > * tour.
limit 20; offset pages; selector "?" = the full grammar.`

var modernNodeQuerySchema = json.RawMessage(`{"type":"object","properties":{` +
	`"selector":{"type":"string","description":"e.g. #'app.go' func, or #'app.go#Save'::in.call"},` +
	`"limit":{"type":"integer","minimum":1,"description":"Max rows. Default 20."},` +
	`"offset":{"type":"integer","minimum":0,"description":"Skip this many rows. Default 0."},` +
	`"budget":{"type":["integer","string"],"description":"limit Nms (bare=ms) or Nops; def 10000ms"}},` +
	`"required":["selector"]}`)

const modernNodeReadDescription = `Read a node whole. node = an address from node_query's matches[].node ("<file>#<sym>" or "<file>"), or a selector matching exactly one node (2+ errors and lists candidates).
An addressed symbol is NEVER truncated: you get the complete declaration, byte-for-byte the span node_edit's newText replaces.
startLine/lineLimit are only for browsing a whole FILE (that view may be truncated, and says so).`

var modernNodeReadSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"node":{"type":"string","description":"Address or selector."},` +
	`"startLine":{"type":"integer","minimum":1},` +
	`"lineLimit":{"type":"integer","minimum":1}},` +
	`"required":["node"]}`)

const modernNodeEditDescription = `Edit one node of the projection. node = an address from node_query's matches[].node, or a selector matching exactly one node (2+ errors and lists candidates).
RENAMING a symbol? Use the rename op — ONE call renames the declaration AND every usage across the WHOLE workspace, atomically. Do NOT rename by hand-editing files one at a time with oldText/newText: that misses usages and leaves the build broken between edits.
Exactly ONE op:
rename:"NewName" — workspace-wide semantic rename; lexical guesses reported under candidates, never applied.
oldText+newText — replace a snippet inside the node. oldText must occur exactly once in the node; the address scopes it, so it need only be unique WITHIN that node — keep it short. Pass the node's whole text to rewrite it entirely, or that text + a new declaration to ADD one next to it.
newText alone — REPLACES a span address (@start-end: ::body/::signature/::grep/a conflict); CREATES a file at a new path; refused on a symbol (use oldText) and never inserts one.
delete:true — excise the node.
accept:"mine"|"theirs" — resolve merge conflicts (whole file, or one block by its @start-end span); markers go too. A conflicted file indexes both sides as PEERS; ::conflict/::mine/::theirs/::base read them apart (sides carry their ref).
params [{name,type}] / return — rebuild the parameter list / return type (go/typescript/python).
includeComments / resolution:{mode,target} — rename only.`

var modernNodeEditSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"node":{"type":"string","description":"Address or selector."},` +
	`"oldText":{"type":"string","description":"Snippet to replace; must be unique within the node."},` +
	`"newText":{"type":"string"},` +
	`"rename":{"type":"string"},` +
	`"params":{"type":"array","items":{"type":"object",` +
	`"properties":{"name":{"type":"string"},"type":{"type":"string"}},"required":["name","type"]}},` +
	`"return":{"type":"string"},` +
	`"delete":{"type":"boolean"},` +
	`"accept":{"type":"string","enum":["mine","theirs"]},` +
	`"includeComments":{"type":"boolean"},` +
	`"resolution":{"type":"object","properties":{"mode":{"type":"string",` +
	`"enum":["underlying","projection","mapping","hide"]},"target":{"type":"string"}}}},` +
	`"required":["node"]}`)

// --------------------------------------------------------- node_query

// defaultQueryLimit is deliberately small: a tight window pushes the
// model to narrow its selector rather than page through noise.
const defaultQueryLimit = 20

func handleModernNodeQuery(s *Server, sess sessionID, args json.RawMessage) ([]Content, bool, error) {
	var p struct {
		Selector string          `json:"selector"`
		Grep     string          `json:"grep"`
		Limit    *int            `json:"limit"`
		Offset   *int            `json:"offset"`
		Budget   json.RawMessage `json:"budget"` // number (=ms) or "Nms"/"Nops"
	}
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, true, fmt.Errorf("bad arguments: %w", err)
		}
	}
	if strings.TrimSpace(p.Selector) == "" {
		return nil, true, errors.New("selector is required (e.g. \":root > *\" for the top-level tour)")
	}
	// "?" is the ONLY way to get the full grammar now: errors answer their own
	// mistake instead of dumping it (see unknownClassErr). Without an explicit
	// way to ask, a caller that genuinely is lost has nowhere to go.
	if strings.TrimSpace(p.Selector) == "?" {
		return []Content{{Type: "text", Text: selectorGrammarHelp}}, false, nil
	}
	if strings.TrimSpace(p.Grep) != "" {
		return nil, true, errors.New("the grep field is gone — put the pattern IN the selector as a fragment: <sel>::grep('-i -A2 derp'). Same flags; every match is an addressable line")
	}
	sel, explain := splitExplain(p.Selector)
	list, err := parseModernSelector(sel)
	if err != nil {
		return nil, true, err
	}
	limit := defaultQueryLimit
	if p.Limit != nil {
		if *p.Limit < 1 {
			return nil, true, errors.New("limit must be >= 1")
		}
		limit = *p.Limit
	}
	offset := 0
	if p.Offset != nil {
		if *p.Offset < 0 {
			return nil, true, errors.New("offset must be >= 0")
		}
		offset = *p.Offset
	}

	e, err := s.buildTree()
	if err != nil {
		return nil, true, err
	}
	if v, unit, ok := parseBudget(string(p.Budget)); ok {
		e.setBudget(v, unit) // Nms wall-clock (bare=ms) or Nops deterministic
	}

	// `--limit 5` should not pay for 24,590 matches. The cap is honoured
	// only for a selector whose evaluation is one document-ordered walk;
	// everything else evaluates in full (see evaluateCapped). `explain`
	// deliberately opts OUT — a cost trace of a short-circuited run would
	// under-report the very work it exists to show.
	need := 0
	if !explain {
		need = offset + limit
	}
	rows, capped := e.evaluateCapped(list, need)

	// :explain returns a cost TRACE, not matches — a deliberate
	// result-shape fork. The query still RAN (that is the measured
	// column); we just render the estimate/actual per element instead.
	if explain {
		return jsonContent(map[string]any{
			"explain":   e.explainRows(list),
			"truncated": e.workExceeded,
		}), false, nil
	}

	total := len(rows)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	paged := rows[offset:end]

	matches := make([]any, 0, len(paged))
	for _, n := range paged {
		// "type", not "class": the OUTPUT is the strongest teacher we have —
		// it lands every turn, in front of the model, right where it decides
		// what to write next. Labeling a tag "class" here while the grammar
		// calls it a tag is precisely the mixed signal that had the model
		// writing `.cache`. The result must model the language it wants back.
		m := map[string]any{"node": n.addr(), "type": n.class}
		// project/dir nodes have no source span, so no "@".
		if n.class != "project" && n.class != "dir" {
			m["@"] = []int{n.at[0], n.at[1]}
		}
		switch n.class {
		case "ref":
			// A ref row teaches the edge: its type IS the selector
			// spelling, and the far end is keyed by direction so the row
			// reads as the fact it states.
			m["type"] = refTypeLabel(n)
			// What settled this edge. A caller acting on "who calls Save"
			// needs to know whether a language server said so (lsp), the
			// name is unique so the match is certain anyway (lexical), or
			// it is an "unsettled" guess whose several far ends are a
			// CANDIDATE LIST, not a fact.
			m["conf"] = n.refConf
			far := make([]string, 0, len(n.refFar))
			external := false
			for _, f := range n.refFar {
				far = append(far, f.addr())
				if f.domain == "external" {
					external = true
				}
			}
			if n.refDir == "out" {
				m["to"] = far
			} else {
				m["from"] = far
			}
			// An external far end (module@version#sym) is a read-only STUB
			// outside the git root — resolved (conf: lsp) but NOT indexed.
			// Flag it so the identity never reads as a workspace symbol.
			if external {
				m["domain"] = "external"
			}
		case "fragment":
			// A fragment row IS its matched line: text (plus -A/-B/-C
			// context) and the node it was found in.
			m["type"] = "::grep"
			m["in"] = n.parent.addr()
			m["text"] = n.frag.Text
			if len(n.frag.Before) > 0 {
				m["before"] = n.frag.Before
			}
			if len(n.frag.After) > 0 {
				m["after"] = n.frag.After
			}
		case "conflict", "mine", "theirs", "base":
			// A conflict node carries its source INLINE and, for a side, the
			// REF it came from. The ref is the side's real identity: under a
			// rebase "ours" is the upstream and "theirs" is your own commit,
			// so a caller told only "mine/theirs" is being told the thing
			// that misleads them.
			m["type"] = "::" + n.class
			m["in"] = n.parent.addr()
			m["text"] = n.genText
			if n.conflictLabel != "" {
				m["ref"] = n.conflictLabel
			}
			if n.conflict != nil {
				sides := map[string]any{"mine": n.conflict.ours.label, "theirs": n.conflict.theirs.label}
				if n.conflict.base != nil {
					sides["base"] = n.conflict.base.label
				}
				m["sides"] = sides
				// Whether each side RECONSTRUCTS into parseable source, and
				// the text diff when neither does. Without this a caller
				// cannot tell a structural answer from a recovered guess.
				if abs, err := s.resolveFileArg(n.file); err == nil {
					if content, err := os.ReadFile(abs); err == nil {
						v := s.viewOf(n.file, content, *n.conflict)
						m["mineParses"], m["theirsParses"] = v.MineParses, v.TheirsParses
						if v.Diff != "" {
							m["diff"] = v.Diff
						}
						if v.Note != "" {
							m["note"] = v.Note
						}
					}
				}
			}
		case "signature", "body":
			// A generated decl-head / body node carries its source INLINE,
			// so a broad `func::signature` is a one-query overview.
			m["type"] = "::" + n.class
			m["in"] = n.parent.addr()
			m["text"] = n.genText
		}
		// A row the walk CROSSED an edge to reach keeps the edge's facts. An
		// uncrossed `::out.call` row already carries site and conf; `> *`
		// used to drop both, so a caller could not tell a resolved edge from
		// an unsettled guess in the very results the walk was for.
		if via, ok := e.reachedVia[n]; ok && n.class != "ref" {
			m["via"] = via.addr()
			m["conf"] = via.refConf
		}
		// How far away it is. Only meaningful for a repeated walk — a single
		// hop is trivially 1 and saying so is noise.
		//
		// firstHop is keyed on the REF nodes the walk stepped through, so a
		// crossed row (`> *`, the common shape) is not in it directly: its
		// distance is the distance of the edge that reached it.
		if e.maxHopReached > 1 {
			hop, ok := e.firstHop[n]
			if !ok {
				if via, viaOK := e.reachedVia[n]; viaOK {
					hop, ok = e.firstHop[via]
				}
			}
			if ok {
				m["hop"] = hop
			}
		}
		matches = append(matches, m)
	}

	payload := map[string]any{
		"returned": len(paged),
		"matches":  matches,
	}
	// The same reading the CLI prints above the matches. Both paths must
	// agree about what a selector MEANS for the same reason they already
	// agree about what it matches: a tool that explains a selector one way
	// to a person and another way to a model is a liar in a subtler
	// register. It is also the output that teaches — a model reading back
	// "returns the `argument` nodes, NOT the `func` nodes" writes a better
	// selector next turn than one that only sees rows.
	payload["read"] = describeSelector(list)
	if s := subjectLine(list); s != "" {
		payload["subject"] = s
	}
	if capped {
		// The walk stopped at the limit, so the count is a FLOOR, not a
		// total. Rendered as the same ">N" the cost trace uses for a
		// budget blow, and under a DIFFERENT key so a reader cannot
		// mistake it for an exact figure.
		payload["totalMatchesAtLeast"] = ">" + commaInt(total)
	} else {
		payload["totalMatches"] = total
	}
	// ::grep recon: a per-file count over ALL matches (not just the page),
	// so a truncated result still shows WHERE the term concentrates —
	// which files to narrow into next. Cheap: one int per file, no text.
	if rollup := fragmentRollup(rows); len(rollup) > 0 {
		payload["rollup"] = rollup
	}
	// Never cut off silently: say there's more, and how to reach it.
	if end < total || capped {
		payload["truncated"] = true
	}
	switch {
	case capped:
		payload["note"] = fmt.Sprintf(
			"%d shown; the walk STOPPED at the limit, so the total is only known to be >%s — "+
				"raise limit or use offset to see more.", len(paged), commaInt(total))
	case end < total || offset > 0:
		payload["note"] = fmt.Sprintf("%d of %d shown; raise limit or use offset", len(paged), total)
	case total == 0:
		if n := literalRegexNote(p.Selector); n != "" {
			payload["note"] = n
		}
		// Which CLAUSE emptied it. Separate key from `note` on purpose:
		// note says what happened to the query that ran, hint names the
		// one thing to write instead. Silent unless a probe finds a
		// concrete alternative (see zeroResultHint).
		if h := e.zeroResultHint(list); h != "" {
			payload["hint"] = h
		}
	}
	// Symbols from a conflicted file may combine both sides, so say so on
	// the rows that carry the risk. Placed ahead of the clause notes: a
	// selector critique is advice, this is "what you are holding may not be
	// real".
	if w := s.conflictWarning(paged); w != "" {
		if prev, ok := payload["note"].(string); ok && prev != "" {
			payload["note"] = w + ". " + prev
		} else {
			payload["note"] = w
		}
	}
	// A clause that could never contribute, at ANY match count. This one
	// must not be gated on total == 0 like zeroResultHint: its worst case is
	// precisely the NON-empty one, where a vanishing {0,…} clause leaves the
	// prefix answering alone and the result looks like a considered answer to
	// the whole selector.
	if h := inertContainmentNote(list); h != "" {
		if prev, ok := payload["hint"].(string); ok && prev != "" {
			payload["hint"] = h + ". " + prev
		} else {
			payload["hint"] = h
		}
	}
	// What was SEARCHED differs from what was written — say so at any
	// match count, and ahead of any note about the result set.
	if n := shellQuotedNote(p.Selector); n != "" {
		if prev, ok := payload["note"].(string); ok && prev != "" {
			payload["note"] = n + ". " + prev
		} else {
			payload["note"] = n
		}
	}
	// Say what the edges are made of. An ambiguous lexical edge lists
	// CANDIDATES; silence here would let a name match read as a fact.
	if note := e.precisionNote(); note != "" {
		payload["edges"] = note
	}
	// :recursive is edge-semantic — sound only with a child LSP. If one
	// wasn't reachable, say the answer is UNDER-RESOLVED, so an empty or
	// partial result never reads as "nothing is recursive".
	if e.recursiveUnconfirmed {
		payload["recursive"] = "no child LSP could confirm some self-call(s), so :recursive is UNDER-RESOLVED — a name-unique self-edge is only counted once the LSP says it really calls ITSELF (not, e.g., an interface method of the same name). Run via the MCP server with a language server; results here may miss real recursion."
	}
	// .implements is LSP-native — there is no lexical fallback, so without a
	// child LSP it is UNAVAILABLE (empty), not "nothing implements this".
	if e.implementsUnavailable {
		payload["implements"] = ".implements is resolved ENTIRELY by a child LSP (structural typing has no lexical clause to key on); none was reachable, so the result is UNAVAILABLE, not empty. Run via the MCP server with a language server."
	}
	// The work budget tripping is the OTHER partial-result path — same
	// contract: flag it and name the fix, never trim quietly.
	if e.workExceeded {
		payload["truncated"] = true
		lim := "the work budget"
		if e.timedOut {
			lim = "the TIME limit (results are NON-deterministic — vary run to run; pass an `ops` budget for a reproducible cut)"
		}
		payload["note"] = "evaluation stopped at " + lim + " — results may be INCOMPLETE. Two levers: (1) NARROW — cost[] names the element that ate the budget; add a kind class (::in.call), a filter (:parents(func)), or bounded hops ({1,3}) THERE. (2) RAISE — retry with a higher `budget` (Nms or Nops) if the query is genuinely large. Narrowing is usually right."
		// The per-element cost trace points at what ate the budget, so
		// the model narrows the RIGHT element instead of guessing.
		payload["cost"] = e.costTrace(list)
	}
	return jsonContent(payload), false, nil
}

// refTypeLabel spells an edge node back as the selector that names it —
// "::in.param.call". This IS the row's `type` in a result, so a caller
// can copy it straight back into a query; the zero-result hint uses the
// same rendering, which is what keeps the two from drifting into
// different vocabularies for the same edge.
func refTypeLabel(n *treeNode) string {
	cls := "::" + n.refDir
	if n.refPos != "" {
		cls += "." + n.refPos
	}
	if n.refKind != "" {
		cls += "." + n.refKind
	}
	return cls
}

// fragmentRollup counts ::grep matches per file across the WHOLE result
// set. Nil when there are no fragment rows (a structural query has
// nothing to roll up), so the field only appears for grep.
func fragmentRollup(rows []*treeNode) map[string]int {
	var rollup map[string]int
	for _, n := range rows {
		if n.class != "fragment" {
			continue
		}
		if rollup == nil {
			rollup = map[string]int{}
		}
		rollup[n.file]++
	}
	return rollup
}

// --------------------------------------------------------- addressing

// maxNodeReadBytes caps an addressed-node read. A declaration bigger
// than this returns an ERROR, never a partial: node_edit's newText
// replaces the whole span, so handing back a truncated declaration
// would let a caller write it straight back and silently destroy the
// tail. Partial addressed reads are structurally impossible by design.
const maxNodeReadBytes = 256 << 10

// modernNode is a resolved node_read / node_edit target.
type modernNode struct {
	class  string
	file   string // workspace-relative
	sym    string // "" = whole file
	addr   string
	isDir  bool
	exists bool

	decl rangeArgs // whole declaration — node_read / newText / delete
	name rangeArgs // identifier — rename / signature refactors
}

// wholeFile reports whether this address targets the ENTIRE file.
//
// NOT the same as sym == "". A ref-site address ("<file>@<line>", which is
// exactly what ::grep / edge / ::signature / ::body rows hand back) has no
// dotted sym either, but it DOES carry a decl span. Using sym == "" as the
// whole-file test silently widened every ref-site op to the whole file:
// node_read returned page 1 instead of the addressed line, an oldText edit
// matched the first occurrence anywhere in the file rather than on that
// line, and delete:true removed the FILE instead of the line. Test the
// class, which is the thing that actually distinguishes them.
func (n *modernNode) wholeFile() bool { return n.sym == "" && n.class != "ref" }

// ordinalSuffix matches the "[n]" disambiguator renderSegment emits.
var ordinalSuffix = regexp.MustCompile(`\[\d+\]`)

// refSiteAddr matches a generated node's site address: "<file>@<line>" (a
// ref/::grep/::comment line) or "<file>@<start>-<end>" (a ::signature/::body
// SPAN, node_read/node_edit hit the whole range).
var refSiteAddr = regexp.MustCompile(`^(.+)@(\d+)(?:-(\d+))?$`)

// resolveRefSiteAddr resolves "<file>@<line>" (one line) or
// "<file>@<start>-<end>" (a span) to an editable range.
func (s *Server) resolveRefSiteAddr(file, startStr, endStr string) (*modernNode, error) {
	abs, err := s.resolveFileArg(file)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	lines := splitNodeReadLines(content)
	start, _ := strconv.Atoi(startStr)
	end := start
	if endStr != "" {
		end, _ = strconv.Atoi(endStr)
	}
	if start < 1 || end < start || end > len(lines) {
		return nil, fmt.Errorf("%s has no line range %d-%d", file, start, end)
	}
	addr := fmt.Sprintf("%s@%d", file, start)
	if endStr != "" {
		addr = fmt.Sprintf("%s@%d-%d", file, start, end)
	}
	r := rangeArgs{File: file, StartLine: start, StartCol: 1, EndLine: end, EndCol: len(lines[end-1]) + 1}
	return &modernNode{
		class: "ref", file: file, addr: addr,
		exists: true, decl: r, name: r,
	}, nil
}

// isClassicSymPath reports whether a "<file>#<here>" fragment is a
// plain dotted symbol path rather than selector syntax. Dots-as-nesting
// and "[n]" ordinal suffixes ARE classic address syntax (Server.Start,
// init[2]) and must not be mistaken for a class marker / attribute.
func isClassicSymPath(sym string) bool {
	if sym == "" {
		return true
	}
	s := ordinalSuffix.ReplaceAllString(sym, "")
	if strings.ContainsAny(s, ":>,()[]*#\"") {
		return false
	}
	if strings.HasPrefix(s, ".") || strings.Contains(s, "..") {
		return false
	}
	return true
}

// looksLikeSelector reports whether a string can ONLY be a selector —
// used to tell a not-yet-existing FILE PATH (a legitimate node_edit
// create target) apart from a selector that happened to match nothing.
func looksLikeSelector(s string) bool {
	if strings.ContainsAny(s, ":>,()*\"") {
		return true
	}
	if strings.ContainsAny(ordinalSuffix.ReplaceAllString(s, ""), "[]") {
		return true
	}
	// A bare known TYPE ("func", "file") is a tag selector. Since types are
	// tags rather than ".class", a type name is now shaped exactly like a
	// relative path, so this is the only thing telling them apart.
	//
	// Not ambiguous in practice: callers reach here only AFTER the path failed
	// to stat, so a real file named `func` already won on the fast path. This
	// only decides what a NON-existent bare word meant, and "the tag" beats
	// "a file that isn't there".
	if knownSelectorClass(s) {
		return true
	}
	// A leading ".<known-type>" is the OLD class spelling. Route it to the
	// selector parser so it returns the guided "write it bare" error rather
	// than a baffling "no such file: .func".
	if strings.HasPrefix(s, ".") && knownSelectorClass(s[1:]) {
		return true
	}
	return false
}

// resolveModernNode resolves node_read / node_edit's `node` field.
//
// Two accepted forms, and NEVER a silent guess between them:
//
//  1. An opaque address "<file>#<dotted.sym.path>" (exactly what
//     node_query returns), or a bare "<file>". Fast path: the file part
//     must stat, and the sym part must be plain dotted-path syntax.
//  2. Any full selector, which must match EXACTLY ONE node.
//
// Ambiguity is always an ERROR listing the candidates. This is the fix
// for the silent-wrong-node bug: renderSegment's ids are cardinality-
// dependent (a lone `init` renders bare; a second `init` appearing
// anywhere retroactively makes the first one `init[1]`) and ordinal
// (an insertion above renumbers), so the old "bare name == the first
// one" normalization could silently re-point a previously-valid
// address at a DIFFERENT symbol after an unrelated edit. Ordinals stay
// a last-resort disambiguator on the OUTPUT side; the INPUT side never
// guesses.
func (s *Server) resolveModernNode(node string) (*modernNode, error) {
	node = strings.TrimSpace(node)
	if node == "" {
		return nil, errors.New("node is required")
	}

	// "<file>@<line>" — a ref node's SITE address (node_query's ref
	// rows). Resolves to that whole line, so reading it shows the site
	// and oldText/newText edits the call site.
	if m := refSiteAddr.FindStringSubmatch(node); m != nil {
		if rn, err := s.resolveRefSiteAddr(m[1], m[2], m[3]); err == nil {
			return rn, nil
		}
	}

	file, symPath := node, ""
	if h := strings.IndexByte(node, '#'); h >= 0 {
		file, symPath = node[:h], node[h+1:]
	}

	// ---- classic fast path
	if file != "" && isClassicSymPath(symPath) {
		abs, err := s.resolveFileArg(file)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(abs)
		switch {
		case err == nil && info.IsDir():
			if symPath != "" {
				return nil, fmt.Errorf("%s is a directory, so it has no symbols", file)
			}
			return &modernNode{class: "dir", file: file, addr: file, isDir: true, exists: true}, nil
		case err == nil:
			return s.resolveClassicAddr(file, symPath)
		case os.IsNotExist(err) && symPath == "" && !looksLikeSelector(node):
			// A not-yet-existing path: only node_edit's whole-file
			// create can use this; node_read reports it as missing.
			return &modernNode{class: "file", file: file, addr: file, exists: false}, nil
		}
	}

	// ---- modern selector path
	list, err := parseModernSelector(node)
	if err != nil {
		return nil, err
	}
	e, err := s.buildTree()
	if err != nil {
		return nil, err
	}
	matches := e.evaluate(list)
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no node matches %q", node)
	case 1:
		m := matches[0]
		switch m.class {
		case "dir":
			return &modernNode{class: "dir", file: m.file, addr: m.addr(), isDir: true, exists: true}, nil
		case "project":
			return nil, fmt.Errorf("%q matches the project root, which is not a file or a symbol", node)
		case "ref", "fragment", "comment", "signature", "body":
			// Generated nodes have no dotted sym; resolve them by their
			// file@line / file@start-end address (a range for ::signature/
			// ::body) so a `#'X'::body` selector edits the SPAN, not the file.
			if am := refSiteAddr.FindStringSubmatch(m.addr()); am != nil {
				return s.resolveRefSiteAddr(am[1], am[2], am[3])
			}
			return nil, fmt.Errorf("%q is a generated node with no editable span", node)
		case "external":
			return nil, fmt.Errorf("%q resolves to an EXTERNAL stub (%s) — read-only, outside the workspace", node, m.addr())
		}
		return s.resolveClassicAddr(m.file, m.sym)
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%q is ambiguous: %d nodes match. Pass one of these exact addresses:", node, len(matches))
		for i, m := range matches {
			if i == 20 {
				fmt.Fprintf(&b, "\n  … and %d more", len(matches)-20)
				break
			}
			fmt.Fprintf(&b, "\n  %s", m.addr())
		}
		return nil, errors.New(b.String())
	}
}

// resolveClassicAddr resolves "<file>#<dotted.sym.path>" against the
// file's symbols. A BARE segment (no explicit "[n]") resolves only when
// exactly ONE candidate carries that name — 2+ is an ambiguity error
// listing every candidate's disambiguated form. An explicit "[n]"
// disambiguates exactly as it always has.
func (s *Server) resolveClassicAddr(file, symPath string) (*modernNode, error) {
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
		return &modernNode{class: "file", file: file, addr: file, exists: true, decl: r, name: r}, nil
	}
	lang := s.languageForFile(abs)
	syms, err := symbols.FileSymbols(lang, content)
	if err != nil {
		return nil, fmt.Errorf("no symbol tree for %s (language %q); read the file whole instead", file, lang)
	}
	var hits []symbols.Symbol
	for _, sym := range syms {
		if classicSymMatch(symPath, sym.Sym) {
			hits = append(hits, sym)
		}
	}
	switch len(hits) {
	case 0:
		// Answer the OTHER reading of this address too. "file#NotThereYet"
		// is just as often "add this" as "typo", and the did-you-mean list
		// only serves the typo — a model that meant to ADD a symbol saw no
		// way forward and fell back to passing the entire file as newText
		// (observed: 93 KB of newText to insert one function). newText-alone
		// creates a new FILE, not a new symbol inside one, so name the idiom
		// that does work.
		return nil, fmt.Errorf("no symbol %q in %s; did you mean: %s. "+
			"To ADD it instead: there is no insert op — grow a NEIGHBOUR. "+
			"node_read a symbol next to where it belongs, then node_edit that address with "+
			"oldText=<its whole text> and newText=<its whole text>+\"\\n\\n\"+<the new declaration>. "+
			"(newText alone creates a new FILE; a whole-file address works but rewrites the file.)",
			symPath, file, nearestSyms(symPath, syms))
	case 1:
		sym := hits[0]
		return &modernNode{
			class: sym.Class, file: file, sym: sym.Sym, addr: file + "#" + sym.Sym, exists: true,
			decl: rangeArgs{File: file, StartLine: sym.DeclStartLine, StartCol: sym.DeclStartCol, EndLine: sym.DeclEndLine, EndCol: sym.DeclEndCol},
			name: rangeArgs{File: file, StartLine: sym.NameStartLine, StartCol: sym.NameStartCol, EndLine: sym.NameEndLine, EndCol: sym.NameEndCol},
		}, nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%q is ambiguous in %s: %d symbols share that name. Pass one of these exact addresses:", symPath, file, len(hits))
		for _, h := range hits {
			fmt.Fprintf(&b, "\n  %s#%s", file, h.Sym)
		}
		return nil, errors.New(b.String())
	}
}

// classicSymMatch compares a query sym path to a candidate, segment by
// segment. A bare query segment matches on NAME ALONE (the caller then
// resolves any resulting ambiguity as an error); an explicit "[n]" pins
// the ordinal, with a bare candidate normalizing to [1].
func classicSymMatch(query, cand string) bool {
	qs := strings.Split(query, ".")
	cs := strings.Split(cand, ".")
	if len(qs) != len(cs) {
		return false
	}
	for i := range qs {
		qn, qi := parseSeg(qs[i])
		cn, ci := parseSeg(cs[i])
		if qn != cn {
			return false
		}
		if qi != 0 && norm1(qi) != norm1(ci) {
			return false
		}
	}
	return true
}

// nodeCurrentText returns the node's current text — byte-for-byte the
// same span node_read returns for the same address, which is what makes
// "read it, then pass its whole text as oldText" a reliable whole-node
// rewrite.
//
// Always FROM DISK, never from cache: this is the read half of a
// compare-and-swap, and the staleness it guards against is precisely an
// out-of-band write (bash, the user's editor, another agent) that a
// cache would not have seen.
func (s *Server) nodeCurrentText(rn *modernNode) (string, error) {
	abs, err := s.resolveFileArg(rn.file)
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", rn.file, err)
	}
	if rn.wholeFile() {
		return string(content), nil
	}
	return readRangeText(content, rn.decl)
}

// ---------------------------------------------------------- node_read

func handleModernNodeRead(s *Server, sess sessionID, args json.RawMessage) ([]Content, bool, error) {
	var p struct {
		Node      string `json:"node"`
		StartLine *int   `json:"startLine"`
		LineLimit *int   `json:"lineLimit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}
	if strings.TrimSpace(p.Node) == "" {
		return nil, true, errors.New("node is required")
	}
	rn, err := s.resolveModernNode(p.Node)
	if err != nil {
		return nil, true, err
	}
	if rn.isDir {
		return nil, true, fmt.Errorf("%s is a directory; node_read reads a file or a symbol. Browse with node_query (e.g. \":root > *\")", rn.file)
	}
	if !rn.exists {
		return nil, true, fmt.Errorf("no such file: %s", rn.file)
	}
	abs, err := s.resolveFileArg(rn.file)
	if err != nil {
		return nil, true, err
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", rn.file, err)
	}

	// A whole-FILE address is a BROWSE: the existing line-window /
	// auto-cap behavior applies, and a truncated view says so. That is
	// WYSIWYG — what you read is what you'd write back — unlike a
	// silently-clipped declaration.
	if rn.wholeFile() {
		startLine := 1
		if p.StartLine != nil {
			if *p.StartLine < 1 {
				return nil, true, errors.New("startLine must be >= 1")
			}
			startLine = *p.StartLine
		}
		lineLimit := 0 // 0 = auto
		if p.LineLimit != nil {
			if *p.LineLimit < 1 {
				return nil, true, errors.New("lineLimit must be >= 1")
			}
			lineLimit = *p.LineLimit
		}
		return jsonContent(buildReadPayload(content, rn.file, startLine, lineLimit, 0,
			targetedSearchAdvice(rn.file, true),
			fileOutline(s.languageForFile(abs), content))), false, nil
	}

	// An addressed-node read is ALWAYS whole.
	if p.StartLine != nil || p.LineLimit != nil {
		what := "a symbol"
		if rn.class == "ref" {
			what = "a source line/span"
		}
		return nil, true, fmt.Errorf(
			"node reads are always whole: %q addresses %s, so startLine/lineLimit don't apply (they browse a whole FILE). Drop them, or pass the file address %q",
			p.Node, what, rn.file)
	}
	text, err := readRangeText(content, rn.decl)
	if err != nil {
		return nil, true, err
	}
	if len(text) > maxNodeReadBytes {
		return nil, true, fmt.Errorf(
			"declaration too large to return whole (%d bytes, limit %d); browse it by file+line window instead: node_read(node:%q, startLine:%d)",
			len(text), maxNodeReadBytes, rn.file, rn.decl.StartLine)
	}
	out := map[string]any{
		"node": rn.addr,
		"file": rn.file,
		"type": rn.class, // "type", matching the tag grammar — see node_query
		"@":    []int{rn.decl.StartLine, rn.decl.EndLine},
		"text": text,
	}
	// Reading a declaration that straddles a marker hands back source from
	// no commit and no branch. node_edit refuses to write it; this is where
	// a caller SEES it, so this is where it has to be said.
	if cs := conflictOverlap(content, rn.decl.StartLine, rn.decl.EndLine); len(cs) > 0 {
		out["note"] = fmt.Sprintf(
			"this span straddles an UNRESOLVED merge conflict at %s, so the text above is part MINE and part THEIRS — "+
				"a declaration that exists on neither side. Read the versions with ::mine / ::theirs, or resolve with "+
				"node_edit(node:%q, accept:\"mine\"|\"theirs\")",
			conflictSpans(cs), conflictAddr(rn.file, cs[0]))
	}
	return jsonContent(out), false, nil
}

// ---------------------------------------------------------- node_edit

type modernEditArgs struct {
	Node    string           `json:"node"`
	OldText *string          `json:"oldText,omitempty"`
	NewText *string          `json:"newText,omitempty"`
	Rename  *string          `json:"rename,omitempty"`
	Params  *[]refactorParam `json:"params,omitempty"`
	Return  *string          `json:"return,omitempty"`
	Delete  *bool            `json:"delete,omitempty"`
	// Accept resolves merge conflicts inside the addressed node: "mine"
	// (a.k.a. "ours") or "theirs". See applyConflictAccept.
	Accept *string `json:"accept,omitempty"`

	// commit / rollback drive the staged-edit transaction. HIDDEN — kept
	// out of the schema on purpose; the rejection help reveals them when a
	// multi-stage edit is actually needed. commit:false stages (unvalidated);
	// the first edit without it commits the batch atomically; rollback:true
	// discards the batch. commit defaults to true.
	Commit   *bool `json:"commit,omitempty"`
	Rollback bool  `json:"rollback,omitempty"`

	IncludeComments bool `json:"includeComments,omitempty"`
	Resolution      *struct {
		Mode   string `json:"mode"`
		Target string `json:"target"`
	} `json:"resolution,omitempty"`

	diagnosticOptions
}

// ops names every op BUCKET actually supplied — node_edit requires
// exactly one and never silently picks. The text bucket
// (oldText/newText/delete) is one bucket: the modify / create / delete
// shapes are distinguished INSIDE it (see textOpKind), not against the
// refactor ops.
func (p *modernEditArgs) ops() []string {
	var out []string
	var text []string
	if p.OldText != nil {
		text = append(text, "oldText")
	}
	if p.NewText != nil {
		text = append(text, "newText")
	}
	if p.Delete != nil {
		text = append(text, "delete")
	}
	if len(text) > 0 {
		out = append(out, strings.Join(text, "+"))
	}
	if p.Rename != nil {
		out = append(out, "rename")
	}
	if p.Params != nil {
		out = append(out, "params")
	}
	if p.Return != nil {
		out = append(out, "return")
	}
	if p.Accept != nil {
		out = append(out, "accept")
	}
	return out
}

// occurrencesOf returns the byte offset of every occurrence of sub in
// s. Overlapping occurrences each count: if "aa" appears in "aaa" there
// genuinely are two places it could mean, and this tool never guesses
// which one the caller meant.
func occurrencesOf(s, sub string) []int {
	if sub == "" {
		return nil
	}
	var out []int
	for i := 0; i+len(sub) <= len(s); {
		j := strings.Index(s[i:], sub)
		if j < 0 {
			break
		}
		out = append(out, i+j)
		i += j + 1
	}
	return out
}

// oldTextNotFoundErr reports a miss and hands back the node's CURRENT
// full text, so a retry costs one turn instead of a read-then-edit
// round trip.
func oldTextNotFoundErr(addr, cur string) error {
	if len(cur) > maxNodeReadBytes {
		return fmt.Errorf("oldText not found in %s (its text is %d bytes — too large to show here; node_read it)", addr, len(cur))
	}
	return fmt.Errorf(
		"oldText not found in %s. oldText must be an exact substring of the node's CURRENT text, which is:\n---\n%s\n---\nIt only has to be unique within this node, not the file.",
		addr, cur)
}

// oldTextAmbiguousErr lists every occurrence with its line of context —
// the same never-guess principle the address resolver uses.
func oldTextAmbiguousErr(addr, cur, oldText string, offs []int) error {
	var b strings.Builder
	fmt.Fprintf(&b, "oldText occurs %d times in %s; lengthen it until it's unique WITHIN this node (it needn't be unique in the file). Occurrences:", len(offs), addr)
	lines := strings.Split(cur, "\n")
	for _, off := range offs {
		// Line index of this offset within the node's own text.
		n := strings.Count(cur[:off], "\n")
		ctx := ""
		if n < len(lines) {
			ctx = strings.TrimSpace(lines[n])
		}
		fmt.Fprintf(&b, "\n  node line %d: %s", n+1, ctx)
	}
	return errors.New(b.String())
}

func handleModernNodeEdit(s *Server, sess sessionID, args json.RawMessage) ([]Content, bool, error) {
	var p modernEditArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}

	// The staged-edit transaction (commit:false) serializes on editMu and
	// is handled here, before the per-edit machinery. activeSession names the
	// batch this edit belongs to; the shared write funnels read it (see
	// session.go). Reset on exit so a later un-threaded caller defaults to
	// localSession.
	s.editMu.Lock()
	defer s.editMu.Unlock()
	s.activeSession = sess
	defer func() { s.activeSession = localSession }()
	commit := p.Commit == nil || *p.Commit

	if p.Rollback {
		b := s.currentBatch()
		if b == nil {
			return jsonContent(map[string]any{"rolledBack": false, "note": "no open batch to roll back"}), false, nil
		}
		oc := editOutcome{}
		n := b.count
		b.revertAll(&oc)
		s.closeBatch()
		res := map[string]any{"rolledBack": true, "discarded": n,
			"note": fmt.Sprintf("discarded %d staged edit(s); reverted to last committed", n)}
		if oc.RevertFailed {
			res["revertFailed"], res["revertError"] = true, oc.RevertErr
		}
		return jsonContent(res), false, nil
	}

	// commit-only (the noop): no edit op, a batch is open → validate and
	// commit what's staged.
	if b := s.currentBatch(); len(p.ops()) == 0 && b != nil && commit {
		oc := b.commit()
		if !oc.Conflict {
			s.closeBatch() // a conflict leaves the batch open to resolve
		}
		return jsonContent(batchCommitPayload(oc)), oc.Rejected || oc.Conflict, nil
	}

	if strings.TrimSpace(p.Node) == "" {
		return nil, true, errors.New("node is required")
	}
	switch ops := p.ops(); len(ops) {
	case 1:
	case 0:
		return nil, true, errors.New("node_edit needs exactly one of {oldText+newText, newText, delete, rename, params, return}; none was given")
	default:
		return nil, true, fmt.Errorf("node_edit takes exactly one of {oldText+newText, newText, delete, rename, params, return}; got %d: %s", len(ops), strings.Join(ops, ", "))
	}
	if p.Delete != nil {
		if !*p.Delete {
			return nil, true, errors.New("delete must be true if present")
		}
		if p.OldText != nil || p.NewText != nil {
			return nil, true, errors.New("delete removes the whole node, so it takes neither oldText nor newText")
		}
	}
	if p.OldText != nil && p.NewText == nil {
		return nil, true, errors.New("oldText needs newText (the text to replace it with); pass delete:true to remove the node instead")
	}
	// rename-only modifiers, rejected before anything touches disk.
	if p.Rename == nil {
		if p.IncludeComments {
			return nil, true, errors.New("includeComments only applies to rename")
		}
		if p.Resolution != nil {
			return nil, true, errors.New("resolution only applies to rename")
		}
	}

	// Open the batch when staging; flag the edit that closes it. Every edit
	// that joins the batch — a raw text edit OR a params/return/rename
	// refactor — counts as one pending operation.
	if !commit {
		b := s.currentBatch()
		if b == nil {
			b = s.openBatch(p.diagnosticOptions)
			s.setBatch(b)
		}
		b.count++
	} else if b := s.currentBatch(); b != nil {
		p.commitBatch = true // this edit stages then validates+commits the union
		b.count++
	}

	rn, err := s.resolveModernNode(p.Node)
	if err != nil {
		return nil, true, err
	}
	if rn.isDir {
		return nil, true, errors.New("node_edit doesn't recurse into directories")
	}

	// A generated-span address (::signature / ::body / a ref/::grep line)
	// always EXISTS as a range — there is no "create" for a span — so
	// newText ALONE replaces it. This is what makes `#'F'::body newText:…`
	// rewrite just the body without repeating the old text.
	if rn.class == "ref" && p.NewText != nil && p.OldText == nil && p.Delete == nil {
		return s.applyRangeRewrite(rn.addr, rn.decl, *p.NewText, p.diagnosticOptions)
	}

	// An EMPTY oldText is "replace nothing with newText" — a create, and
	// the spelling every Edit-shaped tool accepts for one. Refusing it
	// with "no such file" is a lie about which argument was wrong, and
	// the caller's next move is to guess. Treat it as the create it is.
	// Only when the target doesn't exist: on an existing node an empty
	// oldText matches at every offset, and the ambiguity error below is
	// the right answer to that.
	if p.NewText != nil && p.OldText != nil && *p.OldText == "" && !rn.exists {
		p.OldText = nil
	}

	// ---- create: newText alone, and only where nothing resolves yet.
	// Guarded so a create can never silently degrade into clobbering
	// something that already exists.
	if p.NewText != nil && p.OldText == nil {
		if rn.exists {
			return nil, true, fmt.Errorf(
				"node already exists; supply oldText to modify it (%s). To replace it entirely, pass its whole current text as oldText", rn.addr)
		}
		if *p.NewText == "" {
			return nil, true, fmt.Errorf("creating %s needs non-empty newText", rn.file)
		}
		return s.applyWholeFileWrite(rn.file, *p.NewText, p.diagnosticOptions)
	}

	// Every remaining op acts on an EXISTING node.
	if !rn.exists {
		return nil, true, fmt.Errorf("no such file: %s", rn.file)
	}

	if p.Accept != nil {
		return s.applyConflictAccept(rn, *p.Accept, p.diagnosticOptions)
	}
	// Every OTHER mutating op is refused on a span that is half of each side
	// of an unresolved conflict. accept: is exempt above — it is the op that
	// exists to end this state.
	if !rn.wholeFile() && rn.class != "conflict" {
		if abs, err := s.resolveFileArg(rn.file); err == nil {
			if content, err := os.ReadFile(abs); err == nil {
				if cs := conflictOverlap(content, rn.decl.StartLine, rn.decl.EndLine); len(cs) > 0 {
					return nil, true, conflictChimeraErr(rn.file, rn.addr, rn.decl.StartLine, rn.decl.EndLine, cs)
				}
			}
		}
	}
	if p.Delete != nil {
		if rn.wholeFile() {
			return s.applyWholeFileDelete(rn.file, p.diagnosticOptions)
		}
		// A ref-site span empties in place (the newline is outside the
		// span), leaving a blank line rather than closing it up. That is
		// the same splice every other span delete does — and it is the
		// point: `file@12` must never reach applyWholeFileDelete.
		return s.applyRangeRewrite(rn.addr, rn.decl, "", p.diagnosticOptions)
	}

	// ---- modify: Edit-shaped oldText → newText, scoped to the node.
	//
	// oldText is also a second, independent guard against the
	// truncation footgun: a partial read used as oldText simply won't
	// be found (a loud error), where a pure full-span replace would
	// have silently eaten the untruncated tail.
	if p.OldText != nil {
		cur, err := s.nodeCurrentText(rn)
		if err != nil {
			return nil, true, err
		}
		offs := occurrencesOf(cur, *p.OldText)
		switch len(offs) {
		case 1:
		case 0:
			return nil, true, oldTextNotFoundErr(rn.addr, cur)
		default:
			return nil, true, oldTextAmbiguousErr(rn.addr, cur, *p.OldText, offs)
		}
		off := offs[0]
		updated := cur[:off] + *p.NewText + cur[off+len(*p.OldText):]
		if rn.wholeFile() {
			if updated == "" {
				return nil, true, fmt.Errorf(
					"that edit would empty %s; pass delete:true if you meant to remove the file", rn.file)
			}
			return s.applyWholeFileWrite(rn.file, updated, p.diagnosticOptions)
		}
		return s.applyRangeRewrite(rn.addr, rn.decl, updated, p.diagnosticOptions)
	}

	switch {
	case p.Rename != nil:
		// Rename needs a SYMBOL declaration — it renames an identifier and
		// every usage. A whole-file address (sym == "") or a generated span
		// (ref/::grep/::signature/::body, also sym == "") has no identifier;
		// renaming one used to silently rewrite whatever token sat at the
		// span's first line (dogfood: `node_edit(worker.py, rename:…)` renamed
		// the docstring and reported filesChanged:0). Refuse, and point at the
		// symbol form.
		if rn.sym == "" {
			what := "a whole file"
			if rn.class == "ref" {
				what = "a source line/span"
			}
			return nil, true, fmt.Errorf(
				"rename needs a SYMBOL address (file#Name): %s is %s, which has no identifier to rename. Address the declaration you mean (e.g. %s#TheName) and rename THAT",
				rn.addr, what, rn.file)
		}
		mode, target := "", ""
		if p.Resolution != nil {
			mode, target = p.Resolution.Mode, p.Resolution.Target
		}
		// applyCandidates is always false here: the old two-phase
		// preview/apply workflow is gone. Cross-namespace lexical
		// guesses still surface under `candidates`, never auto-applied.
		return s.refactorRename(rn.name, *p.Rename, p.IncludeComments, false, mode, target, p.diagnosticOptions, nil)
	default: // params / return
		ro := refactorOps{}
		if p.Params != nil {
			ro.Params = *p.Params
		}
		if p.Return != nil {
			ro.Return = *p.Return
		}
		return s.refactorSignature(rn.name, ro, p.IncludeComments, p.diagnosticOptions)
	}
}
