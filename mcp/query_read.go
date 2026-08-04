package mcp

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// The READING — what the parser understood, clause by clause, in prose.
//
// This is the counterpart to the parse errors, and it covers the case they
// structurally cannot: a selector that parses FINE and means something other
// than the caller intended. `[name=foo|bar]` is well-formed; it is just wrong,
// and the only signal is an empty result that looks identical to "no such
// symbol". The errors fire when the text is unparseable. The reading fires
// when the text parsed — which is when a wrong mental model survives.
//
// Three misreadings this exists to catch, each measured on a real selector:
//
//   - The SUBJECT is the LAST element. `func > argument` returns arguments,
//     not funcs. Read left-to-right as English it says the opposite, and a
//     caller who believes that reads the whole result set backwards without
//     ever seeing an error.
//   - `=` is LITERAL, `~=` is regex. The parser rejects a bare `|` under `=`,
//     but every other metacharacter (`.` `*` `+` `[`) passes and quietly
//     matches nothing.
//   - `:parents` MOVES the tip upstream rather than filtering in place, so
//     pseudos written after it test a different set than the ones before it.
//
// It renders above the matches rather than on request: a reading nobody asked
// for is the only kind that reaches the caller who did not know to doubt.
type readRow struct {
	Clause  string `json:"clause"`
	Means   string `json:"means"`
	Depth   int    `json:"depth,omitempty"`
	Caution bool   `json:"caution,omitempty"`
}

// readMaxDepth bounds recursion into pseudo arguments. A reading is a glance,
// not a proof: past two levels the indentation costs more comprehension than
// the detail buys, and the inner selector is quoted verbatim instead.
const readMaxDepth = 2

// describeSelector renders a parsed selector as prose rows. It works from the
// AST, never the source text, so it reports what the engine will actually run
// — a reading derived from the input string could agree with the caller's
// misconception and confirm the bug it exists to expose.
func describeSelector(list selectorList) []readRow {
	var rows []readRow
	for i := range list {
		if i > 0 {
			rows = append(rows, readRow{Clause: ",", Means: "OR — a separate alternative, matched independently"})
		}
		rows = append(rows, describeComplex(&list[i], 0)...)
	}
	return rows
}

func describeComplex(cx *selComplex, depth int) []readRow {
	var rows []readRow
	if r, ok := describeRel(cx.rel); ok && depth > 0 {
		rows = append(rows, readRow{Clause: relClause(cx.rel), Means: r, Depth: depth})
	}
	for i := range cx.elems {
		el := &cx.elems[i]
		// A pseudo-element attaches to its host; the child combinator that
		// carries it is synthesized by the parser, not typed by the caller.
		// `func::signature` is two elements internally and one clause on the
		// page, so reporting a '>' invents a step the caller never wrote.
		attached := el.comp != nil && (el.comp.isGenerated() || el.comp.isRef)
		if i > 0 && !attached {
			rows = append(rows, readRow{Clause: combClause(el.comb), Means: combMeaning(el.comb), Depth: depth})
		}
		rows = append(rows, describeElem(el, depth)...)
	}
	return rows
}

func describeElem(el *selElem, depth int) []readRow {
	var rows []readRow
	switch {
	case el.group != nil:
		rows = append(rows, readRow{Clause: "( … )", Means: "a grouped sub-chain:", Depth: depth})
		rows = append(rows, describeComplex(el.group, depth+1)...)
	case el.comp == nil:
		rows = append(rows, readRow{Clause: "?", Means: "an empty element", Depth: depth})
	default:
		rows = append(rows, describeCompound(el.comp, depth)...)
	}
	if el.min != 1 || el.max != 1 {
		// {m,n} means two different things and the reading has to say WHICH.
		// On an edge it counts HOPS CROSSED — a transitive walk. Everywhere
		// else it is regex repetition joined by the child axis. Same syntax,
		// opposite reading, and describing containment on an edge walk is the
		// misreading this row exists to prevent.
		unit, tail := "times, each repeat a DIRECT child of the last", ""
		if el.comp != nil && el.comp.isRef {
			unit, tail = "edge HOPS crossed", " — a transitive walk, not containment"
		}
		var count string
		switch {
		case el.max < 0:
			count = fmt.Sprintf("%d or more", el.min)
		case el.min == el.max:
			count = fmt.Sprintf("exactly %d", el.min)
		default:
			count = fmt.Sprintf("%d to %d", el.min, el.max)
		}
		m := fmt.Sprintf("%s %s%s", count, unit, tail)
		rows = append(rows, readRow{Clause: repClause(el), Means: m, Depth: depth})
	}
	return rows
}

func describeCompound(c *selCompound, depth int) []readRow {
	var rows []readRow
	// An IMPLIED universal was never written: `#Nope` is not `*#Nope`. A row
	// for it explains a clause the caller cannot see in their own selector,
	// and teaches syntax nobody used — the same rule renderElem follows.
	// Only skipped when the attrs will render something, so a bare `*`
	// (which WAS written, or is all there is) still gets its line.
	// attrExpr is the truth; c.attrs holds only the top-level conjuncts, so
	// reading it would render NOTHING for an OR — the one shape where a
	// caller most needs to see which way `|` was taken. Whether a pipe was
	// read as a boolean operator or as part of a pattern is decided by
	// lookahead (see boolOpFollows), and this is where that decision becomes
	// visible instead of something to infer from the result count.
	wrote := c.attrExpr != nil
	if !(c.anyType && c.implied && wrote) {
		rows = append(rows, readRow{Clause: baseClause(c), Means: baseMeaning(c), Depth: depth})
	}
	rows = append(rows, attrExprRows(c.attrExpr, depth)...)
	switch c.ordSel {
	case 1:
		rows = append(rows, readRow{Clause: ":first", Means: "only the FIRST match per anchor, in document order", Depth: depth})
	case 2:
		rows = append(rows, readRow{Clause: ":last", Means: "only the LAST match per anchor, in document order", Depth: depth})
	}
	for _, ps := range c.pseudos {
		rows = append(rows, describePseudo(ps, depth)...)
	}
	for _, k := range c.positionClaims {
		rows = append(rows, readRow{
			Clause: ":" + claimName(k),
			Means:  claimMeaning(k) + " — judged on the set ARRIVING at this position",
			Depth:  depth,
		})
	}
	return rows
}

func describePseudo(ps selPseudo, depth int) []readRow {
	clause := ":" + pseudoName(ps.kind)
	var means string
	switch ps.kind {
	case pseudoContains:
		return []readRow{{Clause: clause + grepClause(ps.grep), Means: "whose OWN source text " + grepMeaning(ps.grep), Depth: depth}}
	case pseudoAnnotated:
		return []readRow{{Clause: clause + grepClause(ps.grep), Means: "whose DOC BLOCK above the declaration " + grepMeaning(ps.grep), Depth: depth}}
	case pseudoParents:
		means = "then MOVES UP — the tip becomes everything upstream (containing ancestors, plus the sources of incoming references). Filters written after this test the UPSTREAM set, not the node"
	case pseudoWhere:
		means = "kept only if a path below it matches"
	case pseudoAny:
		means = claimMeaning(pseudoAny)
	case pseudoAll:
		means = claimMeaning(pseudoAll)
	case pseudoEmpty:
		means = claimMeaning(pseudoEmpty)
	case pseudoNot:
		means = "the node ITSELF must NOT match"
	case pseudoIs:
		means = "the node ITSELF must match"
	case pseudoRecursive:
		return []readRow{{Clause: clause, Means: "callables with an LSP-confirmed call to themselves", Depth: depth}}
	case pseudoArity:
		return []readRow{{Clause: arityClause(ps), Means: arityMeaning(ps), Depth: depth}}
	}
	if len(ps.inner) == 0 {
		if ps.kind == pseudoParents {
			return []readRow{{Clause: clause, Means: means + ". Bare, so everything upstream is kept", Depth: depth}}
		}
		return []readRow{{Clause: clause, Means: means + " (bare — closes the open :parents excursion)", Depth: depth}}
	}
	rows := []readRow{{Clause: clause + "(…)", Means: means + ":", Depth: depth}}
	if depth+1 >= readMaxDepth {
		rows = append(rows, readRow{Clause: "…", Means: "(nested selector — run it on its own for a full reading)", Depth: depth + 1})
		return rows
	}
	for i := range ps.inner {
		rows = append(rows, describeComplex(&ps.inner[i], depth+1)...)
	}
	return rows
}

// ------------------------------------------------------------ clauses

func baseClause(c *selCompound) string {
	switch {
	case c.selfRef:
		return "&"
	case c.root:
		return ":root"
	case c.isRef:
		s := "::" + c.refDir
		for _, rc := range c.refClasses {
			s += "." + rc
		}
		return s
	case c.isFrag:
		return "::grep"
	case c.isComment:
		return "::comment"
	case c.genPart != "":
		return "::" + c.class
	case c.anyType:
		return "*"
	}
	s := c.class
	if c.langClass != "" {
		s += "." + c.langClass
	}
	return s
}

func baseMeaning(c *selCompound) string {
	switch {
	case c.selfRef:
		return "the anchor node ITSELF (not its descendants)"
	case c.root:
		return "the single workspace-root node"
	case c.isRef:
		dir := "pointing IN to it (who references this)"
		if c.refDir == "out" {
			dir = "pointing OUT of it (what this references)"
		}
		s := "reference edges " + dir
		if len(c.refClasses) > 0 {
			s += ", only of kind " + strings.Join(c.refClasses, "/")
		} else {
			s += ", of every kind"
		}
		return s + ". A generated node — `*` never matches these"
	case c.isFrag:
		return "one generated node per source LINE matching the pattern"
	case c.isComment:
		return "the doc-comment block above the symbol, as one generated node"
	case c.genPart == "sig":
		return "just the declaration HEAD of a callable, not its body"
	case c.genPart == "body":
		return "just the BODY block of a callable, not its declaration"
	case c.anyType && c.implied:
		return "any node type (no type was written, so the type is unconstrained)"
	case c.anyType:
		return "nodes of ANY type"
	}
	s := fmt.Sprintf("nodes of type `%s`", c.class)
	if c.langClass != "" {
		s += fmt.Sprintf(", written in %s only", c.langClass)
	}
	return s
}

func attrMeaning(a selAttr) string {
	if a.axis == attrID && a.op == selExact {
		return fmt.Sprintf("named `%s` — a dir, file or symbol id (also matches the `file#symbol` address form)", a.value)
	}
	// NAME and PATH are disjoint axes — a node is CALLED something and it
	// LIVES somewhere — and conflating them made [name] quietly answer path
	// questions. The capitals carry that; spelling the distinction out on
	// every row costs more attention than it returns.
	axis := "NAME"
	if a.axis == attrPath {
		axis = "PATH"
	} else if a.axis == attrID {
		axis = "id"
	}
	switch a.op {
	case selPrefix:
		return fmt.Sprintf("whose %s starts with `%s`", axis, a.value)
	case selSuffix:
		return fmt.Sprintf("whose %s ends with `%s`", axis, a.value)
	case selContains:
		return fmt.Sprintf("whose %s contains `%s`", axis, a.value)
	case selRegex:
		// "unanchored" describes the ENGINE's default, but printed against a
		// pattern the caller anchored themselves (/^New/) it reads as a
		// contradiction of the text right beside it. Say it only where it is
		// still news.
		if strings.ContainsAny(a.value, "^$") {
			return fmt.Sprintf("whose %s matches the REGEX /%s/", axis, a.value)
		}
		return fmt.Sprintf("whose %s matches the REGEX /%s/ — anywhere in it, so no anchoring", axis, a.value)
	}
	return fmt.Sprintf("whose %s is EXACTLY `%s` — a literal string, not a pattern", axis, a.value)
}

// attrCaution flags a literal op carrying regex punctuation. The parser
// already rejects a bare `|` outright, because that one silently no-ops. The
// rest (`.` `*` `+` `?` `(` `[`) are legal literals — `[path$=.go]` is a
// perfectly good exact match — so they cannot be errors. But they are also
// exactly what a caller reaching for a pattern types, and under `=` they match
// themselves and nothing else. Naming it in the reading is the only place this
// can be said without refusing a valid selector.
func attrCaution(a selAttr) (string, bool) {
	if a.op == selRegex || a.op == selPrefix || a.op == selSuffix {
		return "", false
	}
	if i := strings.IndexAny(a.value, `*+?[]()^$\`); i >= 0 {
		return fmt.Sprintf("note: `%c` is LITERAL here. For a pattern use ~=",
			a.value[i]), true
	}
	return "", false
}

func grepClause(g *grepSpec) string {
	if g == nil {
		return "(…)"
	}
	return fmt.Sprintf("('%s')", g.pattern)
}

func grepMeaning(g *grepSpec) string {
	if g == nil {
		return "matches the pattern"
	}
	var s string
	if g.regex {
		s = fmt.Sprintf("matches the regex /%s/", g.pattern)
	} else {
		s = fmt.Sprintf("contains the literal text `%s`", g.pattern)
	}
	var mods []string
	if g.ignoreCase {
		mods = append(mods, "case-insensitive")
	}
	if g.word {
		mods = append(mods, "whole words only")
	}
	if g.invert {
		s = "does NOT " + strings.TrimPrefix(s, "")
	}
	if len(mods) > 0 {
		s += " (" + strings.Join(mods, ", ") + ")"
	}
	return s
}

func arityClause(ps selPseudo) string {
	if ps.arityHi < 0 {
		return fmt.Sprintf(":arity(%d,)", ps.arityLo)
	}
	if ps.arityLo == ps.arityHi {
		return fmt.Sprintf(":arity(%d)", ps.arityLo)
	}
	return fmt.Sprintf(":arity(%d,%d)", ps.arityLo, ps.arityHi)
}

func arityMeaning(ps selPseudo) string {
	switch {
	case ps.arityHi < 0:
		return fmt.Sprintf("taking %d or more arguments", ps.arityLo)
	case ps.arityLo == 0 && ps.arityHi == 0:
		return "taking NO arguments"
	case ps.arityLo == ps.arityHi && ps.arityLo == 1:
		return "taking exactly 1 argument"
	case ps.arityLo == ps.arityHi:
		return fmt.Sprintf("taking exactly %d arguments", ps.arityLo)
	}
	return fmt.Sprintf("taking between %d and %d arguments", ps.arityLo, ps.arityHi)
}

func repClause(el *selElem) string {
	if el.max < 0 {
		return fmt.Sprintf("{%d,}", el.min)
	}
	return fmt.Sprintf("{%d,%d}", el.min, el.max)
}

func combClause(c selComb) string {
	if c == selChild {
		return ">"
	}
	return "(space)"
}

func combMeaning(c selComb) string {
	if c == selChild {
		return "DIRECT children of the above — one level only, not any descendant"
	}
	return "anywhere BELOW the above, at any depth"
}

func relClause(r selRel) string {
	switch r {
	case relChild:
		return "> (leading)"
	case relScope:
		return "&"
	}
	return ""
}

func describeRel(r selRel) (string, bool) {
	switch r {
	case relDescendant:
		return "matched against descendants of the anchor node", true
	case relChild:
		return "matched against DIRECT children of the anchor node only", true
	case relScope:
		return "matched against the anchor node ITSELF", true
	}
	return "", false
}

func pseudoName(k selPseudoKind) string {
	switch k {
	case pseudoContains:
		return "contains"
	case pseudoAnnotated:
		return "annotated"
	case pseudoParents:
		return "parents"
	case pseudoWhere:
		return "where"
	case pseudoAny:
		return "any"
	case pseudoAll:
		return "all"
	case pseudoEmpty:
		return "empty"
	case pseudoNot:
		return "not"
	case pseudoIs:
		return "is"
	case pseudoArity:
		return "arity"
	case pseudoRecursive:
		return "recursive"
	}
	return "?"
}

func claimName(k selPseudoKind) string { return pseudoName(k) }

func claimMeaning(k selPseudoKind) string {
	switch k {
	case pseudoAny:
		return "∃ — at least one must match"
	case pseudoAll:
		return "∀ — every one must match"
	case pseudoEmpty:
		return "∄ — none may match"
	}
	return ""
}

// attrExprRows renders an attribute expression as reading rows: a leaf reads
// as it always did, and an operator gets its own line with its operands
// nested under it, so a grouped expression reads as the tree it is.
func attrExprRows(x *attrExpr, depth int) []readRow {
	if x == nil {
		return nil
	}
	if x.op == attrExprLeaf {
		row := readRow{Clause: renderAttr(x.leaf), Means: attrMeaning(x.leaf), Depth: depth}
		if why, risky := attrCaution(x.leaf); risky {
			row.Means += " — " + why
			row.Caution = true
		}
		return []readRow{row}
	}
	clause, means := "|", "EITHER side matches (boolean OR over whole tests — a `|` inside one value is part of the pattern)"
	if x.op == attrExprAnd {
		clause, means = "&", "BOTH sides match (boolean AND over whole tests)"
	}
	rows := []readRow{{Clause: clause, Means: means, Depth: depth}}
	for _, k := range x.kids {
		rows = append(rows, attrExprRows(k, depth+1)...)
	}
	return rows
}

// ------------------------------------------------------------ subject

// subjectLine names what comes back. The subject is the LAST element, which is
// the single most reliable misreading of this language: `func > argument` is
// read left-to-right as "funcs, with arguments" and actually returns the
// arguments. Stating it costs one line and removes a whole class of silently
// wrong result sets.
//
// It is only worth saying when a chain has somewhere else to be wrong — a
// single-element selector returns the only thing it mentions.
func subjectLine(list selectorList) string {
	if len(list) != 1 {
		return ""
	}
	cx := &list[0]
	if len(cx.elems) < 2 {
		return ""
	}
	subj := cx.subjectComp()
	first := cx.elems[0].comp
	if subj == nil || first == nil || subj == first {
		return ""
	}
	sc, fc := baseClause(subj), baseClause(first)
	if sc != fc {
		return fmt.Sprintf("returns the `%s` nodes — NOT the `%s` nodes, which only constrain them", sc, fc)
	}
	// Same base on both ends — two filtered wildcards, say. Attributes
	// usually separate them; when they do not, say WHICH END in prose rather
	// than smuggling a position into a code span ("`* (last)`" read as a
	// selector nobody could write).
	sf, ff := fullClause(subj), fullClause(first)
	if sf != ff {
		return fmt.Sprintf("returns the `%s` nodes — NOT the `%s` nodes, which only constrain them", sf, ff)
	}
	return fmt.Sprintf("returns the LAST `%s` — NOT the first, which only constrains it", sc)
}

// fullClause is baseClause plus the compound's attributes — what separates
// two ends that render the same bare tag. baseClause drops attributes, so two
// filtered wildcards both render `*` and the subject line degenerated to
// "returns the `*` nodes — NOT the `*` nodes": a sentence naming nothing, on
// exactly the cross-file selectors where knowing which end returns matters
// most.
func fullClause(c *selCompound) string {
	out := baseClause(c)
	for _, a := range c.attrs {
		out += renderAttr(a)
	}
	return out
}

// ------------------------------------------------------------ render

// renderReading writes the reading above the matches. Rows carrying no
// information are not worth a reader's attention: a bare `func` explains
// itself, so a single-row reading collapses to one line and a selector with
// real structure gets the full table.
func renderReading(w io.Writer, list selectorList) error {
	rows := describeSelector(list)
	if len(rows) == 0 {
		return nil
	}
	if len(rows) == 1 {
		_, err := fmt.Fprintf(w, "read as ─ %s: %s\n\n", rows[0].Clause, rows[0].Means)
		return err
	}
	if _, err := fmt.Fprintln(w, "read as ─"); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		indent := strings.Repeat("  ", r.Depth+1)
		mark := ""
		if r.Caution {
			mark = "⚠ "
		}
		if _, err := fmt.Fprintf(tw, "%s%s\t%s%s\n", indent, r.Clause, mark, r.Means); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if s := subjectLine(list); s != "" {
		if _, err := fmt.Fprintf(w, "  ⇒ %s\n", s); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}
