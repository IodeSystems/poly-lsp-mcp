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

const modernNodeQueryDescription = `Projectional editor over ONE node tree, queried by CSS selector: project > dir > file > symbols (dotted-nested) > argument. Files are nodes; no separate filesystem API.
TYPES are bare tags (fixed set): project dir file func method type struct interface class const var field enum ctor module import argument, or *. Workspace NAMES are #ids, never tags: dir cache/ = #cache. Ids: #bare or #'quoted'; a '<file>#<sym>' address is an id: #'store.go#Save'. A class scopes a tag to a LANGUAGE: file.go, func.ts.
space=descendant, >=child, comma=union — containment, as CSS. {m,n} REPEATS an element or (group), child-joined: func{2} = func > func.
REFERENCES are pseudo-element nodes on every symbol: ::in (who points here) / ::out (what its body points at); kind as class (.call/.type/.import, bare = any kind). X::out = X's own edges, X ::out includes nested symbols'. The far end is the edge's child — cross with '>'. {m,n} on the element = edges crossed; {1,} = transitive. '*' NEVER matches an edge, so containment queries never leak.
:parents(sel) — everything UPSTREAM (ancestors + incoming references, transitive) matching sel. A BARE :any/:all/:empty judges the set at its position and decides the node under test (inside :where, or closing :parents). :where/:any/:all/:empty(sel) are relative, CSS-nesting style: a leading pseudo/::element binds to the node itself, a leading tag/#id means a descendant.
::grep('-i -A2 derp') mints each MATCHED LINE of the host's own source as an addressable node (flags -i -w -E -F -v -A/-B/-C<n>; literal unless -E). Rows carry text/in; :contains is its boolean form.
RECIPES (start from an address in matches[].node):
#'store.go#Save'::in.call who calls Save (rows carry from:) | ::in.call{1,} > * every transitive caller | #'main'::out.call > * what main calls | func:where(::in:empty) dead code | #'store.go' func funcs in that file | import#huma::in.call::grep('-E (Get|Post)\(') routes registered on a dependency | func::grep('-w TODO') every TODO line per func | :root > * top-level tour. An edge's or fragment's address is its SITE (file@line): node_read/node_edit touch that line.
limit default 20; offset pages. selector "?" returns the full grammar (:contains, groups, [name^=] …).`

var modernNodeQuerySchema = json.RawMessage(`{"type":"object","properties":{` +
	`"selector":{"type":"string","description":"e.g. #'app.go' func, or #'app.go#Save'::in.call"},` +
	`"limit":{"type":"integer","minimum":1,"description":"Max rows. Default 20."},` +
	`"offset":{"type":"integer","minimum":0,"description":"Skip this many rows. Default 0."}},` +
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
Exactly ONE op:
oldText+newText — replace a snippet inside the node. oldText must occur exactly once in the node; the address scopes it, so it need only be unique WITHIN that node — keep it short. Pass the node's whole text to rewrite it entirely.
newText alone — CREATE the node; only where the address resolves to nothing yet (a new path makes a file node).
delete:true — excise the node.
rename — workspace-wide semantic rename; lexical guesses reported under candidates, never applied.
params — [{name,type}] rebuilds the parameter list (go/typescript/python).
return — rebuilds the return type.
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
	`"includeComments":{"type":"boolean"},` +
	`"resolution":{"type":"object","properties":{"mode":{"type":"string",` +
	`"enum":["underlying","projection","mapping","hide"]},"target":{"type":"string"}}}},` +
	`"required":["node"]}`)

// --------------------------------------------------------- node_query

// defaultQueryLimit is deliberately small: a tight window pushes the
// model to narrow its selector rather than page through noise.
const defaultQueryLimit = 20

func handleModernNodeQuery(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var p struct {
		Selector string `json:"selector"`
		Grep     string `json:"grep"`
		Limit    *int   `json:"limit"`
		Offset   *int   `json:"offset"`
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
	list, err := parseModernSelector(p.Selector)
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

	rows := e.evaluate(list)

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
			cls := "::" + n.refDir
			if n.refKind != "" {
				cls += "." + n.refKind
			}
			m["type"] = cls
			far := make([]string, 0, len(n.refFar))
			for _, f := range n.refFar {
				far = append(far, f.addr())
			}
			if n.refDir == "out" {
				m["to"] = far
			} else {
				m["from"] = far
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
		}
		matches = append(matches, m)
	}

	payload := map[string]any{
		"totalMatches": total,
		"returned":     len(paged),
		"matches":      matches,
	}
	// Never cut off silently: say there's more, and how to reach it.
	if end < total {
		payload["truncated"] = true
	}
	if end < total || offset > 0 {
		payload["note"] = fmt.Sprintf("%d of %d shown; raise limit or use offset", len(paged), total)
	}
	return jsonContent(payload), false, nil
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

// ordinalSuffix matches the "[n]" disambiguator renderSegment emits.
var ordinalSuffix = regexp.MustCompile(`\[\d+\]`)

// refSiteAddr matches a ref node's "<file>@<line>" site address.
var refSiteAddr = regexp.MustCompile(`^(.+)@(\d+)$`)

// resolveRefSiteAddr resolves "<file>@<line>" to that line of the file.
func (s *Server) resolveRefSiteAddr(file, lineStr string) (*modernNode, error) {
	abs, err := s.resolveFileArg(file)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	line, _ := strconv.Atoi(lineStr)
	lines := splitNodeReadLines(content)
	if line < 1 || line > len(lines) {
		return nil, fmt.Errorf("%s has no line %d", file, line)
	}
	r := rangeArgs{File: file, StartLine: line, StartCol: 1, EndLine: line, EndCol: len(lines[line-1]) + 1}
	return &modernNode{
		class: "ref", file: file, addr: fmt.Sprintf("%s@%d", file, line),
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
		if rn, err := s.resolveRefSiteAddr(m[1], m[2]); err == nil {
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
		return nil, fmt.Errorf("no symbol %q in %s; did you mean: %s", symPath, file, nearestSyms(symPath, syms))
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
	if rn.sym == "" {
		return string(content), nil
	}
	return readRangeText(content, rn.decl)
}

// ---------------------------------------------------------- node_read

func handleModernNodeRead(s *Server, args json.RawMessage) ([]Content, bool, error) {
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
	if rn.sym == "" {
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
		return jsonContent(buildReadPayload(content, rn.file, startLine, lineLimit, 0)), false, nil
	}

	// An addressed-node read is ALWAYS whole.
	if p.StartLine != nil || p.LineLimit != nil {
		return nil, true, fmt.Errorf(
			"node reads are always whole: %q addresses a symbol, so startLine/lineLimit don't apply (they browse a whole FILE). Drop them, or pass the file address %q",
			p.Node, rn.file)
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
	return jsonContent(map[string]any{
		"node": rn.addr,
		"file": rn.file,
		"type": rn.class, // "type", matching the tag grammar — see node_query
		"@":    []int{rn.decl.StartLine, rn.decl.EndLine},
		"text": text,
	}), false, nil
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

func handleModernNodeEdit(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var p modernEditArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
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

	rn, err := s.resolveModernNode(p.Node)
	if err != nil {
		return nil, true, err
	}
	if rn.isDir {
		return nil, true, errors.New("node_edit doesn't recurse into directories")
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

	if p.Delete != nil {
		if rn.sym == "" {
			return s.applyWholeFileDelete(rn.file, p.diagnosticOptions)
		}
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
		if rn.sym == "" {
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
		mode, target := "", ""
		if p.Resolution != nil {
			mode, target = p.Resolution.Mode, p.Resolution.Target
		}
		// applyCandidates is always false here: the old two-phase
		// preview/apply workflow is gone. Cross-namespace lexical
		// guesses still surface under `candidates`, never auto-applied.
		return s.refactorRename(rn.name, *p.Rename, p.IncludeComments, false, mode, target, p.diagnosticOptions)
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
