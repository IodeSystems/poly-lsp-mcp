package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// node_query is a CSS-selector QUERY layer over the same declaration-
// oriented symbol model that structure / node_read / node_edit address.
// It does NOT replace "<file>#<sym>" addressing (that stays the exact
// address); it FINDS the symbols matching a selector and returns them in
// the shared grouped-by-file shape, so a result is turned into an
// address by concatenating a match's file `file` + "#" + a symbol's
// `sym`.
//
// The selector grammar maps CSS onto the AST symbol model:
//
//   - type selector      → a `class` name (func / method / type / struct
//     / interface / const / var / field / import / …); `*` = any class.
//   - [name=…] attribute → tested against the symbol's LEAF name (last
//     dotted segment); a dotted value is also tested against the full
//     `sym`. Operators: `=` exact, `^=` prefix, `$=` suffix, `*=` contains.
//   - descendant (space)  → left matches a dotted-path ANCESTOR of right.
//   - child (`>`)         → left matches the DIRECT parent (exactly one
//     dot deeper; parent path == the left match's sym).
//   - :has(<selector>)    → the node CONTAINS a descendant SYMBOL matching
//     the inner selector (see :has semantics note below).
//   - comma               → union of selectors.

// ----------------------------------------------------------- selector AST

type qOp int

const (
	opExact    qOp = iota // =
	opPrefix              // ^=
	opSuffix              // $=
	opContains            // *=
)

type qAttr struct {
	op    qOp
	value string
}

type qCompound struct {
	anyType bool    // `*` or an implied universal (attrs/pseudo only)
	class   string  // the type selector (a symbol `class`), if !anyType
	attrs   []qAttr // [name …] filters
	has     []qList // :has(...) inner selector lists

	// depth is an optional :depth(min,max) override. When set on a
	// compound, it REPLACES the default depth range of the combinator
	// immediately to its left (see combDefaultRange / depthLevels):
	// measured from the PREVIOUS compound in the chain, 0 = that
	// compound itself. nil means "use the combinator's default".
	depth *[2]int
}

type qCombinator int

const (
	combDescendant qCombinator = iota // whitespace
	combChild                         // >
)

// qComplex is a chain of compounds joined by combinators. combs has
// len(compounds)-1 entries; combs[i] joins compounds[i] and
// compounds[i+1]. The LAST compound is the subject (the matched node).
type qComplex struct {
	compounds []qCompound
	combs     []qCombinator
}

// qList is a comma-separated union of complex selectors.
type qList []qComplex

// ----------------------------------------------------------- parser

// parseSelector parses a full selector list. It is total: any malformed
// input returns a guided error and never panics.
func parseSelector(input string) (qList, error) {
	p := &selParser{s: []rune(input)}
	list, err := p.parseList()
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if !p.eof() {
		return nil, p.errf("unexpected trailing input")
	}
	if len(list) == 0 {
		return nil, errors.New("empty selector")
	}
	return list, nil
}

type selParser struct {
	s []rune
	i int
}

func (p *selParser) eof() bool { return p.i >= len(p.s) }

func (p *selParser) peek() rune {
	if p.eof() {
		return 0
	}
	return p.s[p.i]
}

// skipWS advances over spaces/tabs and reports whether any were consumed.
func (p *selParser) skipWS() bool {
	consumed := false
	for !p.eof() {
		c := p.s[p.i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			p.i++
			consumed = true
			continue
		}
		break
	}
	return consumed
}

// errf renders a guided parse error naming what was expected and a
// snippet of the unconsumed input.
func (p *selParser) errf(expected string) error {
	rest := string(p.s[p.i:])
	if len(rest) > 24 {
		rest = rest[:24] + "…"
	}
	if rest == "" {
		rest = "end of input"
	} else {
		rest = "'" + rest + "'"
	}
	return fmt.Errorf("bad selector near %s: expected %s", rest, expected)
}

func (p *selParser) parseList() (qList, error) {
	var list qList
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
		break
	}
	return list, nil
}

func (p *selParser) parseComplex() (qComplex, error) {
	var cx qComplex
	first, err := p.parseCompound()
	if err != nil {
		return cx, err
	}
	cx.compounds = append(cx.compounds, first)
	for {
		sawWS := p.skipWS()
		c := p.peek()
		// Terminators end this complex selector.
		if p.eof() || c == ',' || c == ')' {
			break
		}
		comb := combDescendant
		if c == '>' {
			p.i++
			p.skipWS()
			comb = combChild
		} else if !sawWS {
			// Two compounds with no whitespace and no '>' between them is
			// malformed (attrs/pseudos attach without space inside
			// parseCompound, so reaching here means a stray character).
			return cx, p.errf("combinator, ',' or end of selector")
		}
		next, err := p.parseCompound()
		if err != nil {
			return cx, err
		}
		cx.combs = append(cx.combs, comb)
		cx.compounds = append(cx.compounds, next)
	}
	return cx, nil
}

func (p *selParser) parseCompound() (qCompound, error) {
	var comp qCompound
	sawType := false
	c := p.peek()
	switch {
	case c == '*':
		p.i++
		comp.anyType = true
		sawType = true
	case isIdentStart(c):
		comp.class = p.readIdent()
		sawType = true
	default:
		comp.anyType = true // implied universal; must be followed by attr/pseudo
	}
	for {
		switch p.peek() {
		case '[':
			a, err := p.parseAttr()
			if err != nil {
				return comp, err
			}
			comp.attrs = append(comp.attrs, a)
		case ':':
			hasList, depthRange, err := p.parsePseudo()
			if err != nil {
				return comp, err
			}
			if hasList != nil {
				comp.has = append(comp.has, *hasList)
			}
			if depthRange != nil {
				if comp.depth != nil {
					return comp, p.errf("only one :depth(...) per compound")
				}
				comp.depth = depthRange
			}
		default:
			if !sawType && len(comp.attrs) == 0 && len(comp.has) == 0 && comp.depth == nil {
				return comp, p.errf("a type selector ('func', '*', …) or '['")
			}
			return comp, nil
		}
	}
}

func (p *selParser) parseAttr() (qAttr, error) {
	var a qAttr
	p.i++ // consume '['
	p.skipWS()
	name := p.readIdent()
	if name == "" {
		return a, p.errf("an attribute name")
	}
	if name != "name" {
		return a, fmt.Errorf("unknown attribute %q: only [name] is supported (with = ^= $= *=)", name)
	}
	switch p.peek() {
	case '=':
		p.i++
		a.op = opExact
	case '^':
		p.i++
		if p.peek() != '=' {
			return a, p.errf("'=' (to complete '^=')")
		}
		p.i++
		a.op = opPrefix
	case '$':
		p.i++
		if p.peek() != '=' {
			return a, p.errf("'=' (to complete '$=')")
		}
		p.i++
		a.op = opSuffix
	case '*':
		p.i++
		if p.peek() != '=' {
			return a, p.errf("'=' (to complete '*=')")
		}
		p.i++
		a.op = opContains
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

// parsePseudo parses one ":name(...)" pseudo-class at the cursor
// (leading ':' still unconsumed), dispatching on the name. Returns
// EITHER a :has(...) inner selector list OR a :depth(min,max) range —
// never both; the caller folds whichever came back into the compound
// being built.
func (p *selParser) parsePseudo() (has *qList, depth *[2]int, err error) {
	p.i++ // consume ':'
	name := p.readIdent()
	switch name {
	case "has":
		if p.peek() != '(' {
			return nil, nil, p.errf("'(' after :has")
		}
		p.i++
		inner, err := p.parseList()
		if err != nil {
			return nil, nil, err
		}
		p.skipWS()
		if p.peek() != ')' {
			return nil, nil, p.errf("')' to close :has")
		}
		p.i++
		return &inner, nil, nil
	case "depth":
		if p.peek() != '(' {
			return nil, nil, p.errf("'(' after :depth")
		}
		p.i++
		p.skipWS()
		lo, ok := p.readNonNegInt()
		if !ok {
			return nil, nil, p.errf("a non-negative integer (min)")
		}
		p.skipWS()
		if p.peek() != ',' {
			return nil, nil, p.errf("',' in :depth(min,max)")
		}
		p.i++
		p.skipWS()
		hi, ok := p.readNonNegInt()
		if !ok {
			return nil, nil, p.errf("a non-negative integer (max)")
		}
		p.skipWS()
		if p.peek() != ')' {
			return nil, nil, p.errf("')' to close :depth")
		}
		p.i++
		if hi < lo {
			return nil, nil, fmt.Errorf("bad :depth(%d,%d): max must be >= min", lo, hi)
		}
		return nil, &[2]int{lo, hi}, nil
	default:
		return nil, nil, fmt.Errorf("unknown pseudo-class %q: only :has(...) and :depth(min,max) are supported", ":"+name)
	}
}

// readNonNegInt reads a run of ASCII digits at the cursor.
func (p *selParser) readNonNegInt() (int, bool) {
	start := p.i
	for !p.eof() && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		p.i++
	}
	if p.i == start {
		return 0, false
	}
	n, err := strconv.Atoi(string(p.s[start:p.i]))
	if err != nil {
		return 0, false
	}
	return n, true
}

func (p *selParser) readIdent() string {
	start := p.i
	for !p.eof() {
		c := p.s[p.i]
		if isIdentPart(c) {
			p.i++
			continue
		}
		break
	}
	return string(p.s[start:p.i])
}

// readAttrValue reads an attribute value: a quoted string, or a bare run
// up to ']'. Surrounding whitespace is trimmed for bare values.
func (p *selParser) readAttrValue() string {
	if c := p.peek(); c == '"' || c == '\'' {
		quote := c
		p.i++
		start := p.i
		for !p.eof() && p.s[p.i] != quote {
			p.i++
		}
		v := string(p.s[start:p.i])
		if !p.eof() {
			p.i++ // closing quote
		}
		return v
	}
	start := p.i
	for !p.eof() && p.s[p.i] != ']' {
		p.i++
	}
	return strings.TrimSpace(string(p.s[start:p.i]))
}

func isIdentStart(c rune) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c rune) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '-'
}

// ----------------------------------------------------------- evaluation

// qNode is one symbol candidate the selector engine matches against.
type qNode struct {
	file  string // workspace-relative
	lang  string
	sym   string // dotted, file-relative
	class string
	leaf  string // last dotted segment, index-stripped
	at    [2]int
}

// matchAttr tests a [name …] filter against a node. The leaf name is the
// primary target; a dotted value is also compared against the full sym so
// an author can write `[name=Server.Start]`.
func matchAttr(n qNode, a qAttr) bool {
	test := func(s string) bool {
		switch a.op {
		case opExact:
			return s == a.value
		case opPrefix:
			return strings.HasPrefix(s, a.value)
		case opSuffix:
			return strings.HasSuffix(s, a.value)
		case opContains:
			return strings.Contains(s, a.value)
		}
		return false
	}
	return test(n.leaf) || test(n.sym)
}

// matchCompound tests a single compound (type + attrs + :has) against a
// node. :has ranges over descendant SYMBOLS in the same file.
func matchCompound(n qNode, comp qCompound, byFile *fileNodes) bool {
	if !comp.anyType && n.class != comp.class {
		return false
	}
	for _, a := range comp.attrs {
		if !matchAttr(n, a) {
			return false
		}
	}
	for _, inner := range comp.has {
		if !hasDescendant(n, inner, byFile) {
			return false
		}
	}
	return true
}

// hasDescendant reports whether any strict descendant symbol of n matches
// the inner selector list.
func hasDescendant(n qNode, inner qList, byFile *fileNodes) bool {
	prefix := n.sym + "."
	for _, d := range byFile.nodes {
		if d.sym == n.sym || !strings.HasPrefix(d.sym, prefix) {
			continue
		}
		if matchList(d, inner, byFile) {
			return true
		}
	}
	return false
}

// matchComplex tests a complex selector against a subject node, walking
// the combinator chain right-to-left through the node's ancestors.
func matchComplex(n qNode, cx qComplex, byFile *fileNodes) bool {
	last := len(cx.compounds) - 1
	return matchChain(n, cx, last, byFile)
}

// depthLevels returns the nesting-depth difference between an ancestor
// sym path and a candidate sym path: 0 when they're equal (the
// "anchor itself"), N when the candidate is N dotted segments below
// the ancestor. ancestorSym == "" means "the file's own root" — every
// top-level symbol (no dots) is level 1 below it, and level 0 matches
// only the root itself (sym == ""). ok is false when sym isn't
// ancestorSym's self-or-descendant.
//
// This is the SINGLE shared evaluator behind both node_query's `depth`
// field and the :depth(min,max) pseudo-class, so the two can't
// semantically drift.
func depthLevels(ancestorSym, sym string) (levels int, ok bool) {
	if ancestorSym == sym {
		return 0, true
	}
	if ancestorSym == "" {
		return strings.Count(sym, ".") + 1, true
	}
	prefix := ancestorSym + "."
	if !strings.HasPrefix(sym, prefix) {
		return 0, false
	}
	return strings.Count(sym[len(prefix):], ".") + 1, true
}

// inDepthRange reports whether sym is within [min,max] levels below
// ancestorSym (0 = ancestorSym itself). max < 0 means unlimited.
func inDepthRange(ancestorSym, sym string, min, max int) bool {
	levels, ok := depthLevels(ancestorSym, sym)
	if !ok {
		return false
	}
	return levels >= min && (max < 0 || levels <= max)
}

// combDefaultRange is the depth range a combinator implies on the
// compound to its right when that compound carries no :depth(min,max)
// override: child ≡ [1,1], descendant ≡ [1,∞) (self excluded).
func combDefaultRange(c qCombinator) (min, max int) {
	if c == combChild {
		return 1, 1
	}
	return 1, -1
}

// matchChain walks the combinator chain right-to-left. For each link
// it resolves a depth range (either the compound's own :depth(min,max)
// override, or the combinator's default) and searches every candidate
// ancestor within that range — uniformly for child, descendant, and
// :depth() links, so all three go through inDepthRange/depthLevels.
func matchChain(n qNode, cx qComplex, idx int, byFile *fileNodes) bool {
	if !matchCompound(n, cx.compounds[idx], byFile) {
		return false
	}
	if idx == 0 {
		return true
	}
	minD, maxD := combDefaultRange(cx.combs[idx-1])
	if dr := cx.compounds[idx].depth; dr != nil {
		minD, maxD = dr[0], dr[1]
	}
	for _, a := range byFile.nodes {
		levels, ok := depthLevels(a.sym, n.sym)
		if !ok || levels < minD || (maxD >= 0 && levels > maxD) {
			continue
		}
		if matchChain(a, cx, idx-1, byFile) {
			return true
		}
	}
	return false
}

// matchList reports whether any complex in the union matches the node.
func matchList(n qNode, list qList, byFile *fileNodes) bool {
	for _, cx := range list {
		if matchComplex(n, cx, byFile) {
			return true
		}
	}
	return false
}

// fileNodes holds one file's candidates plus a sym index for ancestor /
// child resolution.
type fileNodes struct {
	nodes []qNode
	bySym map[string]qNode
}

// ----------------------------------------------------------- tool handler

func handleLegacyNodeQuery(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var p struct {
		Select string `json:"select"`
		Path   string `json:"path"`
		Limit  *int   `json:"limit"`
	}
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, true, fmt.Errorf("bad arguments: %w", err)
		}
	}
	if strings.TrimSpace(p.Select) == "" {
		return nil, true, errors.New("select is required (a CSS-like selector, e.g. \"method[name=Start]\")")
	}
	list, err := parseSelector(p.Select)
	if err != nil {
		return nil, true, err
	}
	limit := 200
	if p.Limit != nil {
		if *p.Limit < 1 {
			return nil, true, errors.New("limit must be >= 1")
		}
		limit = *p.Limit
	}
	if p.Path == "" {
		p.Path = "."
	}
	abs, err := s.resolveFileArg(p.Path)
	if err != nil {
		return nil, true, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, true, fmt.Errorf("stat %s: %w", p.Path, err)
	}

	var files []string
	if info.IsDir() {
		_ = filepath.WalkDir(abs, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return nil
			}
			if d.IsDir() {
				if skipScanDir(d.Name()) {
					return fs.SkipDir
				}
				return nil
			}
			files = append(files, path)
			return nil
		})
	} else {
		files = append(files, abs)
	}
	sort.Strings(files)

	var items []matchItem
	truncated := false
	for _, f := range files {
		if len(items) >= limit {
			truncated = true
			break
		}
		lang := s.languageForFile(f)
		if lang == "" {
			continue
		}
		content, rerr := os.ReadFile(f)
		if rerr != nil {
			continue
		}
		syms, serr := symbols.FileSymbols(lang, content)
		if serr != nil || len(syms) == 0 {
			continue
		}
		rel := relPath(f, s.getRoot())
		fn := &fileNodes{bySym: make(map[string]qNode, len(syms))}
		for _, sym := range syms {
			n := qNode{
				file:  rel,
				lang:  lang,
				sym:   sym.Sym,
				class: sym.Class,
				leaf:  lastSeg(sym.Sym),
				at:    [2]int{sym.DeclStartLine, sym.DeclEndLine},
			}
			fn.nodes = append(fn.nodes, n)
			fn.bySym[sym.Sym] = n
		}
		for _, n := range fn.nodes {
			if len(items) >= limit {
				truncated = true
				break
			}
			if matchList(n, list, fn) {
				items = append(items, matchItem{
					file: n.file, lang: n.lang, sym: n.sym, class: n.class, at: n.at,
				})
			}
		}
	}

	payload := groupedMatches(items)
	payload["totalMatches"] = len(items)
	if truncated {
		payload["truncated"] = true
		payload["limit"] = limit
	}
	return jsonContent(payload), false, nil
}

// walkQueryFiles returns every file under abs (or abs itself if it's
// a file), sorted for determinism — the file-scope walk shared by the
// modern node_query handler. Language filtering happens per-file at
// the caller (matches the legacy handler's inline walk above).
func (s *Server) walkQueryFiles(abs string) ([]string, error) {
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	var files []string
	if info.IsDir() {
		_ = filepath.WalkDir(abs, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return nil
			}
			if d.IsDir() {
				if skipScanDir(d.Name()) {
					return fs.SkipDir
				}
				return nil
			}
			files = append(files, path)
			return nil
		})
	} else {
		files = append(files, abs)
	}
	sort.Strings(files)
	return files, nil
}
