package symbols

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

// Symbol is one named construct in a file, flattened into a source-
// ordered list. Nesting is encoded in the dotted Sym path (NOT nested
// arrays):
//
//   - Sym   — dotted path RELATIVE to the file, e.g. "Server.Start".
//     Same-named same-class siblings (and anonymous members) are
//     disambiguated with a 1-based "[n]" suffix: "init[1]", "init[2]",
//     or "Server.[1]" for an anonymous member. A bare name is the only
//     one / the first (bare `init` == `init[1]`).
//   - Class — normalized kind from the controlled vocabulary: func,
//     method, type, struct, interface, class, const, var, field, enum,
//     ctor, module, import, argument. An `argument` is a parameter
//     DECLARATION, nested under its callable ("Server.Start.ctx");
//     call-site arguments are not indexed.
//   - Decl* — the whole declaration range (1-based, end-exclusive).
//     node_read / node_edit / node_delete address this.
//   - Name* — just the identifier range. node_references / node_refactor
//     address this. Falls back to the decl range for anonymous nodes.
type Symbol struct {
	Sym   string
	Class string

	// Alias is an extra id the symbol answers to, beyond its leaf and
	// its dotted Sym path. Used for an annotation's own name: an
	// @app.route node lives at "handler.route" (a child of handler) but
	// also answers to "app.route" — the decorator as written.
	Alias string

	DeclStartLine, DeclStartCol int
	DeclEndLine, DeclEndCol     int

	NameStartLine, NameStartCol int
	NameEndLine, NameEndCol     int

	// CommentStart/End span the joined doc-comment block above the
	// declaration (0 = none). Carried as metadata, not a child symbol:
	// ::comment generates the node on demand so it stays invisible to
	// `*` and the containment walk.
	CommentStartLine, CommentStartCol int
	CommentEndLine, CommentEndCol     int

	// BodyStartLine is the 1-based line where a callable's body block
	// begins (0 = no body / not a callable). It splits the declaration
	// into a SIGNATURE (decl start .. here) and a BODY (here .. decl end)
	// that ::signature / ::body generate on demand — like the comment
	// span, metadata rather than a child symbol.
	BodyStartLine int
}

// symRole classifies a node during the FileSymbols walk.
type symRole int

const (
	roleSkip      symRole = iota // ignore this node
	roleContainer                // not a symbol; descend to find symbols at the SAME level
	roleSymbol                   // a symbol; emit it, then (if branch) recurse into it
)

// FileSymbols parses content with the language's tree-sitter grammar and
// returns a FLAT, source-ordered list of every symbol (top-level and
// nested). Nesting lives in the dotted Sym path, never in structure.
//
// Returns an error for languages with no tree-sitter grammar (yaml /
// json / markdown / unregistered) — callers handle those with a single
// whole-file entry.
func FileSymbols(language string, content []byte) ([]Symbol, error) {
	// XML has no vendored grammar (and the html grammar mis-parses Android
	// XML: dotted tag names split, entities in strings.xml error out), so it
	// gets a purpose-built encoding/xml walk instead of a whole-file fallback.
	if language == "xml" {
		return XMLFileSymbols(content), nil
	}
	lang := LanguageByName(language)
	if lang == nil {
		return nil, fmt.Errorf("no tree-sitter grammar for language %q", language)
	}
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if tree == nil {
		return nil, fmt.Errorf("parse returned nil tree")
	}
	defer tree.Close()

	var out []Symbol

	var visit func(container *sitter.Node, prefix, parentClass string)
	visit = func(container *sitter.Node, prefix, parentClass string) {
		// Gather this level's symbol nodes, descending through
		// transparent container nodes (which are not symbols themselves
		// but wrap symbols at this level — e.g. Go's type_declaration
		// wraps type_spec, TS's class_body wraps method_definition).
		var kids []*sitter.Node
		var gather func(n *sitter.Node)
		gather = func(n *sitter.Node) {
			cnt := int(n.NamedChildCount())
			for i := 0; i < cnt; i++ {
				c := n.NamedChild(i)
				switch classify(language, c.Type(), n.Type()) {
				case roleSymbol:
					kids = append(kids, c)
				case roleContainer:
					gather(c)
				}
			}
		}
		gather(container)

		type kidInfo struct {
			node           *sitter.Node
			localName      string
			class          string
			parentOverride string
			nameNode       *sitter.Node
			branch         bool
		}
		infos := make([]kidInfo, 0, len(kids))
		counts := map[string]int{}
		for _, k := range kids {
			class, branch := refinedClass(language, k, parentClass, content)
			if class == "" {
				// A language arm may DECLINE a node it can only recognize
				// by content, not by node type. Groovy needs this: its
				// grammar parses the Jenkins DSL's `agent any` as a
				// declaration, indistinguishable from `String x` until
				// you look for an initializer.
				continue
			}
			localName, nameNode := symbolLocalName(language, k, content)
			override := parentOverride(language, k, content)
			infos = append(infos, kidInfo{k, localName, class, override, nameNode, branch})
			counts[groupKey(override, localName, class)]++
		}

		seen := map[string]int{}
		for _, in := range infos {
			key := groupKey(in.parentOverride, in.localName, in.class)
			seen[key]++
			seg := renderSegment(in.localName, seen[key], counts[key])

			base := prefix
			if in.parentOverride != "" {
				if base != "" {
					base += "." + in.parentOverride
				} else {
					base = in.parentOverride
				}
			}
			full := seg
			if base != "" {
				full = base + "." + seg
			}

			decl := declRangeNode(in.node)
			sym := Symbol{
				Sym:   full,
				Class: in.class,
			}
			// declLineCols, not nodeLineCols: a declaration OWNS its doc
			// comment. Arguments (below) deliberately keep the raw span —
			// reaching up from a parameter would swallow the enclosing
			// function's comment.
			sym.DeclStartLine, sym.DeclStartCol, sym.DeclEndLine, sym.DeclEndCol = declLineCols(decl)
			if in.nameNode != nil {
				sym.NameStartLine, sym.NameStartCol, sym.NameEndLine, sym.NameEndCol = nodeLineCols(in.nameNode)
			} else {
				sym.NameStartLine, sym.NameStartCol = sym.DeclStartLine, sym.DeclStartCol
				sym.NameEndLine, sym.NameEndCol = sym.DeclEndLine, sym.DeclEndCol
			}
			// The doc block above the declaration, as METADATA — ::comment
			// generates the node from it, so it stays out of the tree.
			sym.CommentStartLine, sym.CommentStartCol, sym.CommentEndLine, sym.CommentEndCol = docCommentSpan(in.node)
			// The signature/body split point, for callables that have a
			// body block (::signature / ::body generate from it).
			if classTakesParams(in.class) {
				if body := in.node.ChildByFieldName("body"); body != nil {
					sym.BodyStartLine = int(body.StartPoint().Row) + 1
				}
			}
			out = append(out, sym)

			// Parameter DECLARATIONS become addressable `.argument`
			// children of their func/method/ctor. Emitted here rather
			// than through the classify/gather walk because params are
			// not "symbols at this level" — they hang off the owning
			// node's `parameters` field, and routing them through
			// gather would force branch=true on every func (which would
			// then also drag in body-local declarations).
			appendParamSymbols(language, in.node, full, in.class, content, &out)

			// Return TYPES become `.return` children of their callable,
			// so `func:any(return#error)` composes like containment.
			appendReturnSymbols(language, in.node, full, in.class, content, &out)

			// Decorators / annotations / struct tags become `.annotation`
			// children of the symbol they mark — the SYMBOL carrying the
			// mark, addressable and composable, not a comment line.
			appendAnnotationSymbols(language, in.node, full, in.class, content, &out)

			if in.branch {
				visit(in.node, full, in.class)
			}
		}
	}
	visit(tree.RootNode(), "", "")
	return out, nil
}

func groupKey(parentOverride, name, class string) string {
	return parentOverride + "\x00" + name + "\x00" + class
}

// ------------------------------------------------------- .argument nodes

// paramInfo is one parameter DECLARATION resolved out of a grammar's
// parameter list: the name to render (empty = anonymous, e.g. Go's
// unnamed `func f(int)` or TS's destructured `{a, b}: Props`), the
// identifier node answering rename/references, and the node whose span
// is the argument's declaration range.
type paramInfo struct {
	name     string
	nameNode *sitter.Node
	decl     *sitter.Node
}

// classTakesParams gates which symbol classes get `.argument`
// children. Only callables declare a parameter list; a const bound to
// an arrow function is class const/var (not func), and is deliberately
// left alone — the node model stays declaration-oriented and keyed on
// the class vocabulary.
func classTakesParams(class string) bool {
	switch class {
	case "func", "method", "ctor":
		return true
	}
	return false
}

// appendParamSymbols emits one Class:"argument" Symbol per parameter
// declaration of a callable node, as a dotted child of the owner's sym
// path ("Server.Start.ctx"). Cardinality/anonymity is rendered by the
// same renderSegment the rest of the index uses, so duplicate-named or
// unnamed params disambiguate identically ("[1]", "x[2]", …).
//
// Call-site arguments are deliberately NOT indexed: this model is
// declaration-oriented like every other symbol class.
func appendParamSymbols(lang string, node *sitter.Node, owner, class string, content []byte, out *[]Symbol) {
	if !classTakesParams(class) {
		return
	}
	params := paramListNode(lang, node)
	if params == nil {
		return
	}

	var infos []paramInfo
	cnt := int(params.NamedChildCount())
	for i := 0; i < cnt; i++ {
		infos = append(infos, paramInfos(lang, params.NamedChild(i), content)...)
	}
	if len(infos) == 0 {
		return
	}

	counts := map[string]int{}
	for _, in := range infos {
		counts[in.name]++
	}
	seen := map[string]int{}
	for _, in := range infos {
		seen[in.name]++
		seg := renderSegment(in.name, seen[in.name], counts[in.name])
		sym := Symbol{Sym: owner + "." + seg, Class: "argument"}
		sym.DeclStartLine, sym.DeclStartCol, sym.DeclEndLine, sym.DeclEndCol = nodeLineCols(in.decl)
		nameNode := in.nameNode
		if nameNode == nil {
			nameNode = in.decl
		}
		sym.NameStartLine, sym.NameStartCol, sym.NameEndLine, sym.NameEndCol = nodeLineCols(nameNode)
		*out = append(*out, sym)
	}
}

// paramListNode returns the node holding a callable's parameter
// declarations. Everywhere but C/C++ that is the callable's own
// `parameters` field — which for Go also naturally excludes the method
// receiver, since that sits on a separate `receiver` field. In C/C++ the
// parameter list belongs to the function_declarator buried in the
// declarator chain, not to the definition node.
func paramListNode(lang string, node *sitter.Node) *sitter.Node {
	if lang == "c" || lang == "cpp" {
		fd := cFunctionDeclarator(node)
		if fd == nil {
			return nil
		}
		return fd.ChildByFieldName("parameters")
	}
	if lang == "kotlin" {
		// Positional, like everything else in this grammar. A secondary
		// constructor carries the same node as a function does.
		return firstNamedChildOfType(node, "function_value_parameters")
	}
	return node.ChildByFieldName("parameters")
}

// ------------------------------------------------------- .return nodes

// appendReturnSymbols emits one Class:"return" child per declared return
// TYPE of a callable, so `func:any(return#error)` = funcs returning error
// and `#'T.M' > return` lists a method's result types. Like .argument
// nodes these are synthesized off the owning node's result/return_type
// field, not walked.
//
// A Go multi-return `(int, error)` becomes TWO return children — one per
// type — so `return#error` matches it, which is the whole point for Go's
// `(T, error)` idiom. TS/Python keep a single node (a union `A | B` is one
// type expression). Each node answers to its type's LEAF (the last dotted
// segment: io.Writer -> Writer) via its Sym path, and to the FULL type
// (io.Writer) via its Alias — mirroring how .annotation carries both.
func appendReturnSymbols(lang string, node *sitter.Node, owner, class string, content []byte, out *[]Symbol) {
	if !classTakesParams(class) {
		return
	}
	nodes := returnTypeNodes(lang, node, content)
	if len(nodes) == 0 {
		return
	}
	type retInfo struct {
		seg, alias string
		node       *sitter.Node
	}
	var infos []retInfo
	for _, n := range nodes {
		full := collapseType(n.Content(content))
		if full == "" {
			continue
		}
		seg := full
		alias := ""
		if lang == "c" || lang == "cpp" {
			seg, alias = cTypeSegment(full)
		} else if lang == "go" {
			seg, alias = goTypeSegment(full)
		} else if lang == "kotlin" || lang == "java" || lang == "typescript" {
			seg, alias = typeSegment(full)
		} else if i := strings.LastIndex(full, "."); i >= 0 {
			// A qualified type (io.Writer): the path segment must be
			// dot-free, so the leaf is the last component and the full
			// form is preserved as the alias.
			seg = full[i+1:]
			alias = full
		}
		if seg == "" {
			continue
		}
		infos = append(infos, retInfo{seg: seg, alias: alias, node: n})
	}
	counts := map[string]int{}
	for _, in := range infos {
		counts[in.seg]++
	}
	seen := map[string]int{}
	for _, in := range infos {
		seen[in.seg]++
		seg := renderSegment(in.seg, seen[in.seg], counts[in.seg])
		sym := Symbol{Sym: owner + "." + seg, Class: "return", Alias: in.alias}
		sym.DeclStartLine, sym.DeclStartCol, sym.DeclEndLine, sym.DeclEndCol = nodeLineCols(in.node)
		sym.NameStartLine, sym.NameStartCol, sym.NameEndLine, sym.NameEndCol = nodeLineCols(in.node)
		*out = append(*out, sym)
	}
}

// returnTypeNodes resolves the individual return-TYPE nodes of a callable.
// Go's result field may be a bare type or a parameter_list (the tuple
// case, split into one node per element); TS/Python carry a single
// return_type (unwrapped from TS's `: T` type_annotation).
func returnTypeNodes(lang string, node *sitter.Node, content []byte) []*sitter.Node {
	switch lang {
	case "go":
		res := node.ChildByFieldName("result")
		if res == nil {
			return nil
		}
		if res.Type() != "parameter_list" {
			return []*sitter.Node{res}
		}
		var out []*sitter.Node
		cnt := int(res.NamedChildCount())
		for i := 0; i < cnt; i++ {
			c := res.NamedChild(i)
			if ty := c.ChildByFieldName("type"); ty != nil {
				out = append(out, ty)
			} else {
				out = append(out, c)
			}
		}
		return out
	case "typescript":
		rt := node.ChildByFieldName("return_type")
		if rt == nil {
			return nil
		}
		// The field is a `type_annotation` (": T"); the type itself is
		// its last named child.
		if rt.Type() == "type_annotation" && rt.NamedChildCount() > 0 {
			return []*sitter.Node{rt.NamedChild(int(rt.NamedChildCount()) - 1)}
		}
		return []*sitter.Node{rt}
	case "python":
		rt := node.ChildByFieldName("return_type")
		if rt == nil {
			return nil
		}
		return []*sitter.Node{rt}
	case "java":
		// method_declaration carries the return type in `type`.
		// Constructors have none, which is correctly nil here.
		rt := node.ChildByFieldName("type")
		if rt == nil {
			return nil
		}
		return []*sitter.Node{rt}
	case "c", "cpp":
		// A definition carries `type` itself; a prototype's symbol node
		// is the declarator, and the type sits on the enclosing
		// declaration / field_declaration. Pointer and reference depth
		// lives in the declarator, not the type node, so `char *` yields
		// a return of `char` — the type NAME is what `return#T` asks
		// about. Constructors and destructors have no type field, which
		// is correctly nil here.
		owner := node
		if owner.ChildByFieldName("type") == nil {
			owner = node.Parent()
		}
		if owner == nil {
			return nil
		}
		rt := owner.ChildByFieldName("type")
		if rt == nil {
			return nil
		}
		return []*sitter.Node{rt}
	case "groovy":
		// Callables carry their declared type in `type`. `def` lands in
		// that field too, but it is the ABSENCE of a declared type — a
		// `.return` node named def would answer `return#def` for every
		// dynamically-typed method in the file.
		rt := node.ChildByFieldName("type")
		if rt == nil || rt.Content(content) == "def" {
			return nil
		}
		return []*sitter.Node{rt}
	case "kotlin":
		// The return type is the type node AFTER the parameter list; a
		// type before the name is the extension receiver, not a result.
		// A constructor has neither, which is correctly nil here.
		if _, _, rt := kotlinFuncParts(node); rt != nil {
			return []*sitter.Node{rt}
		}
		return nil
	}
	return nil
}

// collapseType renders a type node's source as a single trimmed line,
// collapsing internal runs of whitespace so a multi-line signature still
// yields one clean leaf.
func collapseType(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ------------------------------------------------------- .annotation nodes

// appendAnnotationSymbols emits one Class:"annotation" child per
// decorator (Python/TS) or struct-tag key (Go) attached to a symbol.
// Like .argument nodes, these are synthesized rather than walked: the
// decorator sits beside or within the declaration in the AST, and the
// point is to hang it OFF the symbol it marks so `func:any(annotation#route)`
// and `#'T.Name' > annotation` compose the way containment does.
//
// Each annotation answers to its LEAF (the last identifier: route,
// requires_auth, Component, json) via its Sym path, and to its Alias
// (the decorator as written: app.route) via the extra id.
func appendAnnotationSymbols(lang string, node *sitter.Node, owner, class string, content []byte, out *[]Symbol) {
	var marks []annMark
	switch lang {
	case "python":
		marks = pythonDecorators(node, content)
	case "typescript":
		marks = tsDecorators(node, content)
	case "go":
		marks = goStructTags(node, class, content)
	case "java":
		marks = javaAnnotations(node, content)
	case "kotlin":
		marks = kotlinAnnotations(node, content)
	case "groovy":
		marks = groovyAnnotations(node, content)
	}
	if len(marks) == 0 {
		return
	}
	counts := map[string]int{}
	for _, m := range marks {
		counts[m.leaf]++
	}
	seen := map[string]int{}
	for _, m := range marks {
		seen[m.leaf]++
		seg := renderSegment(m.leaf, seen[m.leaf], counts[m.leaf])
		alias := m.fqn
		if alias == m.leaf {
			alias = "" // no separate fqn to record
		}
		sym := Symbol{Sym: owner + "." + seg, Class: "annotation", Alias: alias}
		sym.DeclStartLine, sym.DeclStartCol, sym.DeclEndLine, sym.DeclEndCol = nodeLineCols(m.node)
		sym.NameStartLine, sym.NameStartCol, sym.NameEndLine, sym.NameEndCol = nodeLineCols(m.node)
		*out = append(*out, sym)
	}
}

// docCommentSpan returns the span of the doc comment attached to a
// declaration: the contiguous run of `comment` sibling nodes immediately
// above it, joined. Go parses each `//` line as its own comment node, so
// a three-line doc is three siblings — this rejoins them into one span
// so ::comment is the whole block, not its first line. Returns zeros
// when there is no attached doc.
//
// Contiguity is required (each comment line directly above the next,
// none separated from the declaration by a blank), so a floating comment
// earlier in the file is not mistaken for the doc. Decorated (Python)
// and exported (TS) declarations look above their wrapper.
// docCommentAnchor climbs from a symbol's node to the node the doc comment
// actually sits above. The symbol is often the declarATOR, wrapped in one or
// more nodes the comment precedes instead — and the wrappers NEST, which is
// why this loops: `export const x = 1` puts a variable_declarator inside a
// lexical_declaration inside an export_statement, and stopping at the first
// wrapper finds no comment at all.
func docCommentAnchor(node *sitter.Node) *sitter.Node {
	anchor := node
	for {
		p := anchor.Parent()
		if p == nil {
			return anchor
		}
		switch p.Type() {
		case "decorated_definition", "export_statement",
			// C/C++ wrappers: the symbol is the declarator (or the
			// templated declaration), but the comment sits above the
			// whole declaration.
			"declaration", "field_declaration", "template_declaration":
		case "lexical_declaration", "variable_declaration":
			// `const x = 1` — but `const a = 1, b = 2` has two symbols
			// under one declaration, and neither may claim the whole
			// thing or they would overlap.
			if countVariableDeclarators(p) != 1 {
				return anchor
			}
		default:
			if !isStatementWrapper(p) {
				return anchor
			}
		}
		anchor = p
	}
}

func docCommentSpan(node *sitter.Node) (startLine, startCol, endLine, endCol int) {
	anchor := docCommentAnchor(node)
	nextTop := int(anchor.StartPoint().Row) + 1 // 1-based line the block must butt against
	found := false
	for s := anchor.PrevNamedSibling(); s != nil && isCommentNode(s.Type()); s = s.PrevNamedSibling() {
		sl, sc, el, ec := nodeLineCols(s)
		if el != nextTop-1 {
			break // a blank line (or code) breaks the doc block
		}
		if isTrailingComment(s) {
			break // it documents the declaration it trails, not this one
		}
		if !found {
			endLine, endCol = el, ec
			found = true
		}
		startLine, startCol = sl, sc
		nextTop = sl
	}
	if found {
		return startLine, startCol, endLine, endCol
	}
	// No block above: a comment trailing the declaration is its doc instead —
	// the Go convention for struct fields and enum members. A leading block
	// wins when both exist, since that is the fuller documentation.
	if sl, sc, el, ec := trailingCommentSpan(anchor); el != 0 {
		return sl, sc, el, ec
	}
	return 0, 0, 0, 0
}

// markdownHeadingNode returns the `inline` node holding a section's
// title text, or nil when the section has no heading.
func markdownHeadingNode(section *sitter.Node) *sitter.Node {
	for i := range int(section.NamedChildCount()) {
		h := section.NamedChild(i)
		switch h.Type() {
		case "atx_heading", "setext_heading":
			if in := firstNamedChildOfType(h, "inline"); in != nil {
				return in
			}
			return nil
		}
		// Only a LEADING heading titles the section; anything else means
		// this section is untitled preamble.
		return nil
	}
	return nil
}

// markdownHeadingText renders a section's title as a path segment.
// Dots would split the path, so they are folded to spaces — a heading is
// prose, not a dotted name.
func markdownHeadingText(section *sitter.Node, content []byte) string {
	in := markdownHeadingNode(section)
	if in == nil {
		return ""
	}
	txt := collapseType(in.Content(content))
	txt = strings.ReplaceAll(txt, ".", " ")
	return strings.TrimSpace(txt)
}

// isCommentNode reports whether a node type is a comment. Every grammar
// here but Kotlin's spells it `comment`; Kotlin splits the two forms,
// and without both a Kotlin declaration would silently lose the doc
// block that ::comment, :contains and the decl range all depend on.
func isCommentNode(t string) bool {
	switch t {
	case "comment", "line_comment", "multiline_comment", "block_comment",
		// tree-sitter-sql spells a /* … */ block this way, so a sql block
		// comment was not recognised as a comment at all.
		"marginalia":
		return true
	}
	return false
}

// isStatementWrapper reports whether p is a bare `statement` node holding a
// single declaration — tree-sitter-sql wraps every CREATE in one, and the doc
// comment is the WRAPPER's sibling, two levels up from the symbol.
//
// The single-child guard keeps this from firing on any other grammar that
// happens to name a node `statement`: a wrapper around exactly one named
// child is a pass-through, and adopting its range cannot swallow a sibling.
func isStatementWrapper(p *sitter.Node) bool {
	return p.Type() == "statement" && p.NamedChildCount() == 1
}

// isTrailingComment reports whether a comment node trails code on its own
// line — `Enabled bool // why` — rather than standing on a line of its own.
//
// This is the distinction both span rules were missing. They asked only
// whether a comment ENDED on the line directly above a declaration, which a
// trailing comment does, so the comment documenting one field was claimed by
// the field BELOW it. In go and java that only mis-aimed ::comment; in
// typescript and kotlin it pulled the neighbour's comment into the next
// declaration's span, and `delete` on that declaration destroyed it while
// stranding the comment that really was its own.
//
// The test is structural rather than textual: a comment is trailing when
// something on the same line precedes it. Anonymous siblings count — the `;`
// after a typescript field is exactly what precedes its comment.
//
// The column check is what makes that sound. Go's grammar puts an anonymous
// newline terminator between declarations, and it ends at column 0 OF THE
// COMMENT'S OWN ROW — so "the previous sibling ends on this row" alone marks
// every doc comment in the language as trailing. A sibling ending at column 0
// contributes nothing to that line; only one ending past it does.
func isTrailingComment(c *sitter.Node) bool {
	prev := c.PrevSibling()
	if prev == nil {
		return false
	}
	end := prev.EndPoint()
	return end.Row == c.StartPoint().Row && end.Column > 0
}

// trailingCommentSpan returns the span of the run of comments trailing decl
// on its final line, or zeros when there is none. A declaration OWNS the
// comment that trails it for the same reasons it owns the doc block above it
// (see declLineCols): node_read that omits it hands back undocumented code,
// node_edit that excludes it cannot rewrite the comment and strands it, and
// delete orphans it.
func trailingCommentSpan(decl *sitter.Node) (startLine, startCol, endLine, endCol int) {
	_, _, line, lineEndCol := nodeLineCols(decl)
	// A node can END at the start of the following line because it swallowed
	// its terminating newline — c's preproc_def does, so `#define A 1` claims
	// to end at column 1 of the NEXT line. It occupies none of that line, and
	// treating it as the declaration's own would hand it the comment trailing
	// the next #define. Column 1 is zero-width, so the real last line is the
	// one before it. (This is the downward twin of the column check in
	// isTrailingComment; measured at 10,758 sibling span overlaps across a
	// 22,858-file sweep before the guard, 0 after.)
	if lineEndCol == 1 && line > 1 {
		line--
	}
	for cur := decl; ; {
		next := cur.NextSibling()
		if next == nil {
			// The declaration can be the TAIL of a wrapper node, with the
			// comment attached to the wrapper instead: python's assignment
			// sits inside an expression_statement, and `# why` is the
			// statement's sibling. Rise only while the parent ends exactly
			// where this node does, so we never step out past a closing
			// brace and claim a comment belonging to the enclosing scope.
			p := cur.Parent()
			if p == nil || p.EndPoint() != cur.EndPoint() {
				break
			}
			cur = p
			continue
		}
		sl, sc, el, ec := nodeLineCols(next)
		if sl != line { // starts on a later line — it belongs to what follows
			break
		}
		if isCommentNode(next.Type()) {
			if startLine == 0 {
				startLine, startCol = sl, sc
			}
			endLine, endCol, line = el, ec, el
			cur = next
			continue
		}
		// Anonymous punctuation can sit between a declaration and its
		// comment — the `;` typescript and java close a field with. Step
		// over it, but only while it stays on this line: go's newline
		// terminator is also anonymous and starts here, and following it
		// would hand this declaration the DOC comment of the next one.
		if next.IsNamed() || el != line {
			break
		}
		cur = next
	}
	return startLine, startCol, endLine, endCol
}

// annMark is one resolved annotation: the AST node for its span, the
// leaf name (#route matches this) and the fqn as written (app.route).
type annMark struct {
	node *sitter.Node
	leaf string
	fqn  string
}

func nodeSlice(n *sitter.Node, content []byte) string {
	return string(content[n.StartByte():n.EndByte()])
}

// pythonDecorators collects the `decorator` siblings of a function/class
// under a `decorated_definition`.
func pythonDecorators(node *sitter.Node, content []byte) []annMark {
	parent := node.Parent()
	if parent == nil || parent.Type() != "decorated_definition" {
		return nil
	}
	var out []annMark
	cnt := int(parent.NamedChildCount())
	for i := 0; i < cnt; i++ {
		c := parent.NamedChild(i)
		if c.Type() != "decorator" || c.NamedChildCount() == 0 {
			continue
		}
		leaf, fqn := decoratorName(c.NamedChild(0), content, "call", "attribute", "attribute")
		if leaf != "" {
			out = append(out, annMark{c, leaf, fqn})
		}
	}
	return out
}

// tsDecorators collects a symbol's `decorator` nodes. Fields and methods
// carry them as direct children; an EXPORTED class has them lifted to a
// sibling under the wrapping export_statement (like Python's
// decorated_definition), so both the node and that wrapper are scanned.
func tsDecorators(node *sitter.Node, content []byte) []annMark {
	var out []annMark
	collect := func(parent *sitter.Node) {
		cnt := int(parent.NamedChildCount())
		for i := 0; i < cnt; i++ {
			c := parent.NamedChild(i)
			if c.Type() != "decorator" || c.NamedChildCount() == 0 {
				continue
			}
			leaf, fqn := decoratorName(c.NamedChild(0), content, "call_expression", "member_expression", "property")
			if leaf != "" {
				out = append(out, annMark{c, leaf, fqn})
			}
		}
	}
	collect(node)
	if p := node.Parent(); p != nil && p.Type() == "export_statement" {
		collect(p)
	}
	return out
}

// decoratorName resolves a decorator's expression to (leaf, fqn). It
// unwraps a call (callType) to its function, then a member/attribute
// access (memberType) to its last segment (via field memberField),
// leaving a plain identifier as both leaf and fqn.
func decoratorName(expr *sitter.Node, content []byte, callType, memberType, memberField string) (string, string) {
	if expr == nil {
		return "", ""
	}
	if expr.Type() == callType {
		if f := expr.ChildByFieldName("function"); f != nil {
			expr = f
		}
	}
	fqn := nodeSlice(expr, content)
	if expr.Type() == memberType {
		if last := expr.ChildByFieldName(memberField); last != nil {
			return nodeSlice(last, content), fqn
		}
		if i := strings.LastIndexByte(fqn, '.'); i >= 0 {
			return fqn[i+1:], fqn
		}
	}
	return fqn, fqn
}

var goTagKeyRe = regexp.MustCompile("([A-Za-z_][A-Za-z0-9_.-]*):\"")

// goStructTags reads a field's raw-string struct tag and emits one
// annotation per KEY (`json:"name" validate:"required"` → json, validate)
// — Go's structured annotation. Directive comments (//go:generate,
// // Deprecated:) have no AST node and stay with :annotated.
func goStructTags(node *sitter.Node, class string, content []byte) []annMark {
	if class != "field" {
		return nil
	}
	var tag *sitter.Node
	cnt := int(node.NamedChildCount())
	for i := 0; i < cnt; i++ {
		if c := node.NamedChild(i); c.Type() == "raw_string_literal" {
			tag = c
			break
		}
	}
	if tag == nil {
		return nil
	}
	var out []annMark
	for _, m := range goTagKeyRe.FindAllStringSubmatch(nodeSlice(tag, content), -1) {
		out = append(out, annMark{tag, m[1], m[1]})
	}
	return out
}

// paramInfos resolves one node from a parameter list into zero or more
// parameter declarations. Language-dispatched, mirroring classify /
// refinedClass. "typescript" covers .tsx too: both extensions map to
// the `typescript` language name, which LanguageByName backs with the
// tsx grammar — so there is one codepath, not two.
func paramInfos(lang string, p *sitter.Node, content []byte) []paramInfo {
	switch lang {
	case "go":
		return goParamInfos(p, content)
	case "typescript":
		return tsParamInfos(p, content)
	case "python":
		return pyParamInfos(p, content)
	case "java":
		return javaParamInfos(p, content)
	case "c", "cpp":
		return cParamInfos(p, content)
	case "kotlin":
		return kotlinParamInfos(p, content)
	case "groovy":
		return groovyParamInfos(p, content)
	}
	return nil
}

// javaAnnotations collects a declaration's annotations. Java spells a
// bare one `marker_annotation` (@Override, @Id) and an argument-bearing
// one `annotation` (@Table(name = "users")); both sit under `modifiers`.
//
// A FIELD is the wrinkle: classifyJava makes the variable_declarator the
// symbol, but the modifiers hang off the enclosing field_declaration, so
// the search climbs one level for those. Without that, every JPA column
// annotation would be invisible.
func javaAnnotations(node *sitter.Node, content []byte) []annMark {
	mods := firstNamedChildOfType(node, "modifiers")
	if mods == nil {
		if p := node.Parent(); p != nil {
			switch p.Type() {
			case "field_declaration", "local_variable_declaration", "constant_declaration":
				mods = firstNamedChildOfType(p, "modifiers")
			}
		}
	}
	if mods == nil {
		return nil
	}
	var out []annMark
	for i := range int(mods.NamedChildCount()) {
		ann := mods.NamedChild(i)
		switch ann.Type() {
		case "marker_annotation", "annotation":
		default:
			continue
		}
		name := ann.ChildByFieldName("name")
		if name == nil {
			name = firstNamedChildOfType(ann, "identifier")
		}
		if name == nil {
			continue
		}
		fqn := name.Content(content)
		leaf := fqn
		// @com.example.Audited arrives as a scoped_identifier; the index
		// answers to the leaf, with the written form kept as the alias.
		if i := strings.LastIndexByte(leaf, '.'); i >= 0 && i+1 < len(leaf) {
			leaf = leaf[i+1:]
		}
		out = append(out, annMark{ann, leaf, fqn})
	}
	return out
}

// javaParamInfos handles Java's formal_parameter, spread_parameter
// (`String... args`) and receiver_parameter. Java names exactly one
// parameter per declaration, so there is no Go-style `a, b int` case.
func javaParamInfos(p *sitter.Node, content []byte) []paramInfo {
	switch p.Type() {
	case "formal_parameter", "spread_parameter", "receiver_parameter":
	default:
		return nil
	}
	if name := p.ChildByFieldName("name"); name != nil {
		return []paramInfo{{name: name.Content(content), nameNode: name, decl: p}}
	}
	// spread_parameter wraps its declarator rather than naming a field.
	for i := 0; i < int(p.NamedChildCount()); i++ {
		if c := p.NamedChild(i); c.Type() == "variable_declarator" {
			if name := c.ChildByFieldName("name"); name != nil {
				return []paramInfo{{name: name.Content(content), nameNode: name, decl: p}}
			}
		}
	}
	return []paramInfo{{decl: p}}
}

// goParamInfos handles Go's parameter_declaration /
// variadic_parameter_declaration.
func goParamInfos(p *sitter.Node, content []byte) []paramInfo {
	switch p.Type() {
	case "parameter_declaration", "variadic_parameter_declaration":
	default:
		return nil
	}
	var names []*sitter.Node
	for i := 0; i < int(p.ChildCount()); i++ {
		if p.FieldNameForChild(i) == "name" {
			names = append(names, p.Child(i))
		}
	}
	switch len(names) {
	case 0:
		// Unnamed param (`func f(int)`) — anonymous; the whole
		// declaration is the span.
		return []paramInfo{{decl: p}}
	case 1:
		// One name: span is name+type together.
		return []paramInfo{{name: names[0].Content(content), nameNode: names[0], decl: p}}
	default:
		// `a, b int` is ONE declaration carrying several names. Each
		// gets its own node, spanned by its identifier — using the
		// shared declaration span for both would make siblings overlap.
		out := make([]paramInfo, 0, len(names))
		for _, n := range names {
			out = append(out, paramInfo{name: n.Content(content), nameNode: n, decl: n})
		}
		return out
	}
}

// tsParamInfos handles TypeScript/TSX's required_parameter /
// optional_parameter, whose `pattern` field carries the binding.
func tsParamInfos(p *sitter.Node, content []byte) []paramInfo {
	switch p.Type() {
	case "required_parameter", "optional_parameter":
	default:
		return nil
	}
	pat := p.ChildByFieldName("pattern")
	if pat == nil {
		return []paramInfo{{decl: p}}
	}
	switch pat.Type() {
	case "identifier", "shorthand_property_identifier_pattern":
		return []paramInfo{{name: pat.Content(content), nameNode: pat, decl: p}}
	case "rest_pattern":
		if id := firstNamedChildOfType(pat, "identifier"); id != nil {
			return []paramInfo{{name: id.Content(content), nameNode: id, decl: p}}
		}
	}
	// Destructuring patterns ({a, b}: Props) bind no single name —
	// anonymous, addressable positionally via "[n]".
	return []paramInfo{{decl: p}}
}

// pyParamInfos handles Python's parameter forms. `self` is a real
// parameter declaration and is indexed as one.
func pyParamInfos(p *sitter.Node, content []byte) []paramInfo {
	switch p.Type() {
	case "identifier":
		return []paramInfo{{name: p.Content(content), nameNode: p, decl: p}}
	case "typed_parameter", "default_parameter", "typed_default_parameter":
		if n := p.ChildByFieldName("name"); n != nil {
			return []paramInfo{{name: n.Content(content), nameNode: n, decl: p}}
		}
		// typed_parameter exposes its identifier positionally rather
		// than via a `name` field.
		if id := firstNamedChildOfType(p, "identifier"); id != nil {
			return []paramInfo{{name: id.Content(content), nameNode: id, decl: p}}
		}
		return []paramInfo{{decl: p}}
	case "list_splat_pattern", "dictionary_splat_pattern":
		if id := firstNamedChildOfType(p, "identifier"); id != nil {
			return []paramInfo{{name: id.Content(content), nameNode: id, decl: p}}
		}
	}
	return nil
}

// firstNamedChildOfType returns n's first named child of type t, or nil.
func firstNamedChildOfType(n *sitter.Node, t string) *sitter.Node {
	for i := 0; i < int(n.NamedChildCount()); i++ {
		if c := n.NamedChild(i); c.Type() == t {
			return c
		}
	}
	return nil
}

// renderSegment renders one path segment. Anonymous nodes (empty name)
// are always bracketed ("[1]"). Named nodes are bare when unique and
// "name[n]" when there are same-named same-class siblings.
func renderSegment(name string, idx, count int) string {
	if name == "" {
		return "[" + strconv.Itoa(idx) + "]"
	}
	if count == 1 {
		return name
	}
	return name + "[" + strconv.Itoa(idx) + "]"
}

func nodeLineCols(n *sitter.Node) (startLine, startCol, endLine, endCol int) {
	sp := n.StartPoint()
	ep := n.EndPoint()
	return int(sp.Row) + 1, int(sp.Column) + 1, int(ep.Row) + 1, int(ep.Column) + 1
}

// declLineCols is nodeLineCols extended UPWARD over the declaration's doc
// comment — the contiguous run of comment lines directly above it, no blank
// line between.
//
// tree-sitter models comments as SIBLINGS of the declaration, not children, so
// the raw node span stops at `func`. A doc comment is part of the declaration
// in every sense that matters here:
//
//   - node_read returned the function WITHOUT its documentation.
//   - node_edit replaces the span, so rewriting a function left its old comment
//     stranded above the new body — silently describing code that no longer
//     exists.
//   - delete excised the function and orphaned its comment.
//   - :contains('TODO') missed TODOs written where people actually write them.
//
// A blank line ends the block: that is the language-level convention for "this
// comment belongs to the next thing" in Go and TS alike. Python needs nothing —
// its docstrings live inside the body, already in the span.
//
// It is extended DOWNWARD too, over a comment trailing the declaration on its
// last line, which is where the documentation for a struct field actually
// gets written (and what godoc reads). Both directions stop at a trailing
// comment belonging to something else — see isTrailingComment.
func declLineCols(n *sitter.Node) (startLine, startCol, endLine, endCol int) {
	startLine, startCol, endLine, endCol = nodeLineCols(n)
	for cur := n; ; {
		prev := cur.PrevSibling()
		if prev == nil || !isCommentNode(prev.Type()) {
			break
		}
		pStart, pCol, pEnd, _ := nodeLineCols(prev)
		if pEnd+1 != startLine { // blank line (or same line) → not a doc comment
			break
		}
		if isTrailingComment(prev) { // documents the line it sits on, not this one
			break
		}
		startLine, startCol = pStart, pCol
		cur = prev
	}
	if _, _, tl, tc := trailingCommentSpan(n); tl != 0 {
		endLine, endCol = tl, tc
	}
	return startLine, startCol, endLine, endCol
}

// classify assigns a role to a node given its type and its parent's
// type. Language-dispatched.
func classify(lang, t, parent string) symRole {
	switch lang {
	case "go":
		return classifyGo(t, parent)
	case "typescript":
		return classifyTS(t, parent)
	case "python":
		return classifyPython(t, parent)
	case "sql":
		return classifySQL(t, parent)
	case "java":
		return classifyJava(t, parent)
	case "c", "cpp":
		return classifyC(t, parent)
	case "markdown":
		return classifyMarkdown(t, parent)
	case "kotlin":
		return classifyKotlin(t, parent)
	case "groovy":
		return classifyGroovy(t, parent)
	}
	return roleSkip
}

// classifyGroovy maps Groovy declarations onto the shared role
// vocabulary. This grammar is the WEAKEST of the set and the arm is
// shaped around that:
//
//   - A class body is a plain `closure`, not a dedicated node, so
//     closures are containers. That also means a body the grammar
//     DETACHES from its owner (see refinedClassGroovy for trait/enum)
//     still surfaces its members — at file scope rather than under the
//     type, which is a wrong PARENT but not an invented symbol.
//   - `declaration` is genuinely ambiguous — `String x = "hi"`, `enum
//     Mode`, and the Jenkins DSL's `agent any` are all the same node —
//     so refinedClassGroovy decides by content and DECLINES the DSL
//     case.
//   - Constructors are not modeled at all (`Widget(String n) {}` parses
//     as a function_call plus a detached closure), so Groovy emits no
//     ctor nodes. Nothing here can recover that.
func classifyGroovy(t, parent string) symRole {
	switch t {
	case "closure",
		// A Jenkinsfile's whole body is one `pipeline` node.
		"pipeline":
		return roleContainer
	case "groovy_package", "groovy_import", "class_definition",
		"function_definition", "function_declaration", "declaration":
		return roleSymbol
	}
	return roleSkip
}

// classifyMarkdown treats a titled SECTION as the unit of a document.
// The block grammar nests them (a `##` section sits inside its `#`), and
// a section's span already covers its heading plus everything under it,
// so the node model comes out as the document's own outline.
//
// Nothing else is a symbol: paragraphs, lists and code blocks are the
// section's CONTENT, reachable through its range, not siblings of it.
func classifyMarkdown(t, parent string) symRole {
	if t == "section" {
		return roleSymbol
	}
	return roleSkip
}

// classifyKotlin maps Kotlin declarations onto the shared role
// vocabulary.
//
// Two Kotlin-specific container choices carry the weight here:
//
//   - companion_object is walked THROUGH, so its members land directly
//     on the enclosing class (Widget.MAX, Widget.create) — which is
//     exactly how the code addresses them.
//   - primary_constructor is walked through too, because its
//     class_parameters are Kotlin's dominant way of declaring fields:
//     `class Widget(val id: Int)` declares a property, not just a
//     parameter.
//
// getter / setter are skipped: they are SIBLINGS of the property they
// belong to in this grammar, and they carry no name of their own, so
// emitting them would add anonymous "[n]" noise beside every custom
// accessor.
func classifyKotlin(t, parent string) symRole {
	switch t {
	case "class_body", "enum_class_body", "companion_object",
		"import_list", "primary_constructor",
		// ERROR is walked through, for the same reason c/cpp does it: the
		// parser recovers the DECLARATIONS and only mislabels the node
		// that holds them. One unparsable statement deep inside a method
		// re-labels the whole enclosing class_body as ERROR, so a class
		// keeps its name and loses every member. Measured on a live
		// 504-file Kotlin repo: 30 files parse with an ERROR somewhere,
		// and in the worst case a 134-line class body was reduced to
		// nothing while the ERROR still held all 4 properties, the
		// companion object and all 4 methods as children.
		//
		// The cost, accepted knowingly: when the broken region is inside
		// a method, that method's LOCALS flatten into the same ERROR and
		// surface as fields of the class. A few invented fields beat a
		// class with no members at all — a missing method is a hard
		// failure for a code tool, a spurious field is one the reader
		// can check.
		"ERROR":
		return roleContainer
	case "package_header", "import_header",
		"class_declaration", "object_declaration", "type_alias",
		"property_declaration", "function_declaration",
		"secondary_constructor", "enum_entry", "class_parameter":
		return roleSymbol
	}
	return roleSkip
}

// classifyC maps C and C++ declarations onto the shared role
// vocabulary. One arm serves both: the C++ grammar inherits C's node
// names, and every type this switch adds for C++ (namespace_definition,
// class_specifier, alias_declaration, …) simply never appears in a C
// tree.
//
// The structural difference from every other language here is that C has
// no per-symbol declarator node the way Java has variable_declarator:
// `int a, b;` is ONE declaration carrying two declarators, and a
// function prototype is that same `declaration` node with a
// function_declarator inside. So declaration / field_declaration are
// containers and the DECLARATORS are the symbols — which also makes the
// multi-name case fall out for free.
func classifyC(t, parent string) symRole {
	switch parent {
	case "declaration", "field_declaration":
		// Inside a declaration, only the declarator half names a symbol.
		// The type half (type_identifier, qualified_identifier,
		// template_type, struct_specifier, …) is a REFERENCE to a type,
		// not a declaration of one, and indexing it would invent symbols
		// that don't exist — `struct Point *make_point(void);` does not
		// declare Point.
		if isCDeclarator(t) {
			return roleSymbol
		}
		return roleSkip
	case "type_definition":
		// `typedef struct { int x; } Foo;` — the typedef IS the symbol
		// (see refinedClassC), so its underlying specifier is walked
		// through to reach the fields, not emitted on its own.
		switch t {
		case "struct_specifier", "union_specifier", "enum_specifier":
			return roleContainer
		}
		return roleSkip
	}
	switch t {
	case "declaration_list", "field_declaration_list", "enumerator_list",
		"declaration", "field_declaration",
		"template_declaration", "linkage_specification",
		// Header include guards wrap the WHOLE file body in a
		// preproc_ifdef; without descending, a .h file indexes as empty.
		"preproc_ifdef", "preproc_if", "preproc_else", "preproc_elif",
		// ERROR is walked THROUGH, which no other language here does.
		// tree-sitter cannot run the preprocessor, so one GNU extension
		// it doesn't model (`int x[32] __attribute__((aligned(V)))`) makes
		// recovery swallow the enclosing #ifdef — i.e. the entire body of
		// a guarded header. The parser still builds correct subtrees for
		// the parts it understood; they hang off the ERROR node. Measured
		// on llama.cpp/ggml: 3 of 504 files indexed as completely empty
		// without this, each one a real header full of real declarations.
		"ERROR":
		return roleContainer
	case "preproc_include", "preproc_def", "preproc_function_def",
		"namespace_definition", "class_specifier", "struct_specifier",
		"union_specifier", "enum_specifier", "enumerator",
		"type_definition", "alias_declaration", "using_declaration",
		"function_definition":
		return roleSymbol
	}
	return roleSkip
}

// isCDeclarator reports whether a node type is a C/C++ DECLARATOR — the
// half of a declaration that names something, as opposed to the type
// half.
//
// qualified_identifier is deliberately absent: as a direct child of a
// declaration it is almost always the TYPE (`std::string s;`), and the
// declarator spelling that matters (`int App::counter = 5;`) arrives
// wrapped in an init_declarator, which is listed. Classifying it as a
// declarator here would turn every `std::foo x;` type into a phantom
// symbol.
func isCDeclarator(t string) bool {
	switch t {
	case "init_declarator", "pointer_declarator", "array_declarator",
		"function_declarator", "reference_declarator",
		"parenthesized_declarator", "structured_binding_declarator",
		"identifier", "field_identifier", "destructor_name",
		"operator_name", "template_function":
		return true
	}
	return false
}

// ------------------------------------------------------- groovy

// refinedClassGroovy assigns the class for a Groovy symbol node, or
// DECLINES (class "") when the node is only a declaration by accident of
// the grammar. ok is false for node types this arm doesn't own.
func refinedClassGroovy(node *sitter.Node, parentClass string, content []byte) (class string, branch, ok bool) {
	switch node.Type() {
	case "groovy_package":
		return "module", false, true
	case "groovy_import":
		return "import", false, true
	case "class_definition":
		// `class` and `interface` are unnamed TOKENS whose node type is
		// the keyword itself — there is no modifier node to read.
		for i := range int(node.ChildCount()) {
			if c := node.Child(i); !c.IsNamed() && c.Type() == "interface" {
				return "interface", true, true
			}
		}
		return "class", true, true
	case "function_definition", "function_declaration":
		switch parentClass {
		case "class", "interface", "enum", "struct":
			return "method", false, true
		}
		return "func", false, true
	case "declaration":
		return groovyDeclarationClass(node, parentClass, content), false, true
	}
	return "", false, false
}

// groovyDeclarationClass resolves Groovy's overloaded `declaration`
// node, which is three different things wearing one node type:
//
//  1. `enum Mode` / `trait Loggable` — the grammar models NEITHER
//     keyword, so it reads them as a type name and produces a
//     declaration whose `type` is the keyword. Their bodies are parsed
//     as a DETACHED sibling closure, so the members surface at file
//     scope rather than under the type. Emitting the type is still
//     right: it exists, and the name resolves.
//  2. A real variable or field — recognized by an initializer, a
//     modifier, or a primitive type.
//  3. Neither. The Jenkins DSL's `agent any` and `branch "main"` parse
//     as declarations because juxtaposition looks like `Type name`.
//     Emitting those would invent a variable named `any` in every
//     Jenkinsfile, so this DECLINES them.
func groovyDeclarationClass(node *sitter.Node, parentClass string, content []byte) string {
	ty := node.ChildByFieldName("type")
	if ty != nil {
		switch ty.Content(content) {
		case "enum":
			return "enum"
		case "trait":
			return "interface"
		}
	}
	declares := node.ChildByFieldName("value") != nil ||
		(ty != nil && ty.Type() == "builtintype")
	if !declares {
		for i := range int(node.NamedChildCount()) {
			switch node.NamedChild(i).Type() {
			case "modifier", "access_modifier":
				declares = true
			}
		}
	}
	if !declares {
		return "" // DSL juxtaposition, not a declaration
	}
	switch parentClass {
	case "class", "interface", "enum", "struct":
		return "field"
	}
	return "var"
}

// groovyName resolves a Groovy symbol node's local name. ok is false for
// node types the generic path should handle.
func groovyName(node *sitter.Node, content []byte) (string, *sitter.Node, bool) {
	switch node.Type() {
	case "groovy_package":
		if q := firstNamedChildOfType(node, "qualified_name"); q != nil {
			if leaf := lastNamedChildOfType(q, "identifier"); leaf != nil {
				return leaf.Content(content), leaf, true
			}
		}
		return "", nil, true
	case "groovy_import":
		if alias := node.ChildByFieldName("import_alias"); alias != nil {
			return alias.Content(content), alias, true
		}
		if q := node.ChildByFieldName("import"); q != nil {
			if leaf := lastNamedChildOfType(q, "identifier"); leaf != nil {
				return leaf.Content(content), leaf, true
			}
		}
		return "", nil, true
	case "class_definition", "declaration", "function_definition", "function_declaration":
		// The `name` field is not reliable on its own: a class with an
		// `implements` clause parses that clause as an ERROR node
		// carrying the SAME field name, so anything not spelled as an
		// identifier is rejected and the first identifier child wins.
		if n := node.ChildByFieldName("name"); n != nil && n.Type() == "identifier" {
			return n.Content(content), n, true
		}
		// Callables name themselves through `function`, not `name`.
		if n := node.ChildByFieldName("function"); n != nil && n.Type() == "identifier" {
			return n.Content(content), n, true
		}
		if n := firstNamedChildOfType(node, "identifier"); n != nil {
			return n.Content(content), n, true
		}
		return "", nil, true
	}
	return "", nil, false
}

// groovyParamInfos handles Groovy's `parameter` node. An untyped
// parameter (`def f(a, b)`) still names itself through `name`.
func groovyParamInfos(p *sitter.Node, content []byte) []paramInfo {
	if p.Type() != "parameter" {
		return nil
	}
	if n := p.ChildByFieldName("name"); n != nil {
		return []paramInfo{{name: n.Content(content), nameNode: n, decl: p}}
	}
	if n := firstNamedChildOfType(p, "identifier"); n != nil {
		return []paramInfo{{name: n.Content(content), nameNode: n, decl: p}}
	}
	return []paramInfo{{decl: p}}
}

// groovyAnnotations collects a declaration's `annotation` children
// (@CompileStatic, @Grab(...)). The annotation's last identifier is the
// leaf; a qualified spelling keeps the full form as the alias.
func groovyAnnotations(node *sitter.Node, content []byte) []annMark {
	var out []annMark
	for i := range int(node.NamedChildCount()) {
		ann := node.NamedChild(i)
		if ann.Type() != "annotation" {
			continue
		}
		leaf := findDescendantOfType(ann, "identifier", 3)
		if leaf == nil {
			continue
		}
		out = append(out, annMark{ann, leaf.Content(content), leaf.Content(content)})
	}
	return out
}

// ------------------------------------------------------- kotlin
//
// The vendored tree-sitter-kotlin grammar exposes NO field names — every
// child is positional. So where the Java arm reads
// ChildByFieldName("name"), the Kotlin helpers below walk named children
// and select by node TYPE and ORDER. That is also why a function's
// receiver, name and return type all have to be resolved in one pass
// (kotlinFuncParts): they are told apart only by which side of
// function_value_parameters they sit on.

// refinedClassKotlin assigns the class for a Kotlin symbol node. The ok
// return is false for node types this arm doesn't own.
func refinedClassKotlin(node *sitter.Node, parentClass string, content []byte) (class string, branch, ok bool) {
	switch node.Type() {
	case "package_header":
		// Answers to the LEAF segment (app, not com.example.app),
		// matching Go's package_clause and Java's package_declaration.
		return "module", false, true
	case "import_header":
		return "import", false, true
	case "type_alias":
		return "type", false, true
	case "enum_entry":
		return "const", false, true
	case "secondary_constructor":
		return "ctor", false, true
	case "object_declaration":
		// A Kotlin `object` is a singleton class; there is no separate
		// class for it and selectors written for class should find it.
		return "class", true, true
	case "class_declaration":
		return kotlinClassKind(node, content), true, true
	case "class_parameter":
		// `class Widget(val id: Int, name: String)` — the val/var half
		// declares a PROPERTY, the bare half is only a constructor
		// parameter. Both are addressable under the class; only the
		// first is a field.
		if firstNamedChildOfType(node, "binding_pattern_kind") != nil {
			return "field", false, true
		}
		return "argument", false, true
	case "property_declaration":
		switch parentClass {
		case "class", "struct", "interface", "enum":
			return "field", false, true
		}
		// Top-level: `val` is an immutable binding, the same distinction
		// TypeScript's const/let draws.
		if k := firstNamedChildOfType(node, "binding_pattern_kind"); k != nil && k.Content(content) == "val" {
			return "const", false, true
		}
		return "var", false, true
	case "function_declaration":
		// An extension function is morally a method on its receiver, and
		// parentOverride files it under that type (String.shout).
		if recv, _, _ := kotlinFuncParts(node); recv != nil {
			return "method", false, true
		}
		switch parentClass {
		case "class", "struct", "interface", "enum":
			return "method", false, true
		}
		return "func", false, true
	}
	return "", false, false
}

// kotlinClassKind tells class / interface / enum / struct apart. The
// grammar spells `interface` as an ANONYMOUS token (no modifiers node,
// no field), so the keyword is read off the unnamed children; `enum
// class` is recognized by its body node instead, and a `data class` maps
// to struct — the same call the Java arm makes for a record.
func kotlinClassKind(node *sitter.Node, content []byte) string {
	if firstNamedChildOfType(node, "enum_class_body") != nil {
		return "enum"
	}
	for i := range int(node.ChildCount()) {
		c := node.Child(i)
		if c.IsNamed() {
			continue
		}
		switch c.Content(content) {
		case "interface":
			return "interface"
		case "class":
			// keep looking only for modifiers, which precede it
		}
	}
	if mods := firstNamedChildOfType(node, "modifiers"); mods != nil {
		for i := range int(mods.NamedChildCount()) {
			if m := mods.NamedChild(i); m.Content(content) == "data" {
				return "struct"
			}
		}
	}
	return "class"
}

// isKotlinType reports whether a node is a TYPE expression. Used to pick
// the receiver and return type out of a function_declaration's
// positional children.
func isKotlinType(t string) bool {
	switch t {
	case "user_type", "nullable_type", "function_type",
		"parenthesized_type", "dynamic_type", "not_nullable_type":
		return true
	}
	return false
}

// kotlinFuncParts resolves a function_declaration's three positional
// pieces in one pass: the extension RECEIVER type (a type node before
// the name), the NAME, and the RETURN type (a type node after the
// parameter list). Any of them may be nil.
func kotlinFuncParts(node *sitter.Node) (receiver, name, returnType *sitter.Node) {
	seenParams := false
	for i := range int(node.NamedChildCount()) {
		c := node.NamedChild(i)
		switch {
		case c.Type() == "function_value_parameters":
			seenParams = true
		case c.Type() == "simple_identifier" && name == nil && !seenParams:
			name = c
		case isKotlinType(c.Type()):
			if seenParams {
				if returnType == nil {
					returnType = c
				}
			} else if name == nil && receiver == nil {
				// A type BEFORE the name is the extension receiver.
				receiver = c
			}
		}
	}
	return receiver, name, returnType
}

// kotlinName resolves a Kotlin symbol node to its local name. ok is
// false for node types the generic path should handle.
func kotlinName(node *sitter.Node, content []byte) (string, *sitter.Node, bool) {
	switch node.Type() {
	case "package_header":
		if id := firstNamedChildOfType(node, "identifier"); id != nil {
			if leaf := lastNamedChildOfType(id, "simple_identifier"); leaf != nil {
				return leaf.Content(content), leaf, true
			}
		}
		return "", nil, true
	case "import_header":
		// An aliased import answers to the alias — that is the name the
		// rest of the file actually writes.
		if alias := firstNamedChildOfType(node, "import_alias"); alias != nil {
			if n := firstNamedChildOfType(alias, "type_identifier"); n != nil {
				return n.Content(content), n, true
			}
		}
		if id := firstNamedChildOfType(node, "identifier"); id != nil {
			if leaf := lastNamedChildOfType(id, "simple_identifier"); leaf != nil {
				return leaf.Content(content), leaf, true
			}
		}
		return "", nil, true
	case "class_declaration", "object_declaration", "type_alias":
		if n := firstNamedChildOfType(node, "type_identifier"); n != nil {
			return n.Content(content), n, true
		}
		return "", nil, true
	case "property_declaration":
		// property_declaration > variable_declaration > simple_identifier.
		// A destructuring `val (a, b) = p` answers to its first name.
		if vd := firstNamedChildOfType(node, "variable_declaration"); vd != nil {
			if n := firstNamedChildOfType(vd, "simple_identifier"); n != nil {
				return n.Content(content), n, true
			}
		}
		if mv := firstNamedChildOfType(node, "multi_variable_declaration"); mv != nil {
			if vd := firstNamedChildOfType(mv, "variable_declaration"); vd != nil {
				if n := firstNamedChildOfType(vd, "simple_identifier"); n != nil {
					return n.Content(content), n, true
				}
			}
		}
		return "", nil, true
	case "function_declaration":
		_, name, _ := kotlinFuncParts(node)
		if name != nil {
			return name.Content(content), name, true
		}
		return "", nil, true
	case "class_parameter", "enum_entry":
		if n := firstNamedChildOfType(node, "simple_identifier"); n != nil {
			return n.Content(content), n, true
		}
		return "", nil, true
	case "secondary_constructor":
		// Named after its class, matching Java's ctor path
		// (Widget.Widget). There is no name TOKEN to point at — the
		// declaration spells `constructor` — so the name range falls
		// back to the declaration span, and rename correctly refuses to
		// pin on a keyword.
		return kotlinEnclosingTypeName(node, content), nil, true
	}
	return "", nil, false
}

// kotlinEnclosingTypeName returns the name of the class/object whose
// body contains node, or "".
func kotlinEnclosingTypeName(node *sitter.Node, content []byte) string {
	for p := node.Parent(); p != nil; p = p.Parent() {
		switch p.Type() {
		case "class_declaration", "object_declaration":
			if n := firstNamedChildOfType(p, "type_identifier"); n != nil {
				return n.Content(content)
			}
			return ""
		case "source_file":
			return ""
		}
	}
	return ""
}

// kotlinParamInfos handles Kotlin's `parameter` nodes. `vararg` arrives
// as a parameter_modifiers SIBLING rather than a child, and a default
// value as a trailing expression sibling, so both are skipped here and
// only the parameter itself names anything.
func kotlinParamInfos(p *sitter.Node, content []byte) []paramInfo {
	if p.Type() != "parameter" {
		return nil
	}
	if n := firstNamedChildOfType(p, "simple_identifier"); n != nil {
		return []paramInfo{{name: n.Content(content), nameNode: n, decl: p}}
	}
	return []paramInfo{{decl: p}}
}

// kotlinAnnotations collects the `annotation` children of a
// declaration's `modifiers` node. An annotation is written either bare
// (@Composable) or as a call (@Suppress("unused")); both bottom out in a
// user_type, whose last type_identifier is the leaf the index answers
// to, with the type as written kept as the alias.
func kotlinAnnotations(node *sitter.Node, content []byte) []annMark {
	mods := firstNamedChildOfType(node, "modifiers")
	if mods == nil {
		return nil
	}
	var out []annMark
	for i := range int(mods.NamedChildCount()) {
		ann := mods.NamedChild(i)
		if ann.Type() != "annotation" {
			continue
		}
		ut := findDescendantOfType(ann, "user_type", 3)
		if ut == nil {
			continue
		}
		leaf := lastNamedChildOfType(ut, "type_identifier")
		if leaf == nil {
			continue
		}
		out = append(out, annMark{ann, leaf.Content(content), collapseType(nodeSlice(ut, content))})
	}
	return out
}

// lastNamedChildOfType returns n's last named child of type t, or nil.
func lastNamedChildOfType(n *sitter.Node, t string) *sitter.Node {
	var found *sitter.Node
	for i := range int(n.NamedChildCount()) {
		if c := n.NamedChild(i); c.Type() == t {
			found = c
		}
	}
	return found
}

// findDescendantOfType returns the first descendant of type t within
// `depth` levels, or nil. Bounded so it can't wander into a body.
func findDescendantOfType(n *sitter.Node, t string, depth int) *sitter.Node {
	if n == nil || depth < 0 {
		return nil
	}
	if n.Type() == t {
		return n
	}
	if depth == 0 {
		return nil
	}
	for i := range int(n.NamedChildCount()) {
		if got := findDescendantOfType(n.NamedChild(i), t, depth-1); got != nil {
			return got
		}
	}
	return nil
}

// goTypeSegment renders a Go type as a path segment plus the full
// spelling as an alias.
//
// Decoration is KEPT: `*Config` and `[]Schema` stay whole, because the
// pointer and slice markers are part of how Go code names the result and
// existing sym paths depend on it. Only the qualifier is split off, as
// before — `*sitter.Node` → Node aliased `*sitter.Node`.
//
// What this fixes is narrower and outright wrong: the blind dot-split
// used to reach INSIDE a composite type and report its last identifier
// as the leaf. `func sitesByFile() map[string][]symbols.InvSite` answered
// to `return#InvSite` — a type it does not return. A map, channel, func
// type, fixed-size array or inline interface/struct has no single leaf,
// so it is left whole and claims nothing.
func goTypeSegment(full string) (seg, alias string) {
	if isGoCompositeType(full) {
		return full, ""
	}
	if i := strings.LastIndex(full, "."); i >= 0 && i+1 < len(full) {
		return full[i+1:], full
	}
	return full, ""
}

// isGoCompositeType reports whether a type is a composite whose last
// dotted identifier is NOT its name. Pointer and slice decoration is
// peeled only to see what is underneath — the caller keeps the full
// spelling either way.
func isGoCompositeType(full string) bool {
	t := full
	for {
		switch {
		case strings.HasPrefix(t, "*"):
			t = t[1:]
			continue
		case strings.HasPrefix(t, "[]"):
			t = t[2:]
			continue
		}
		break
	}
	switch {
	case strings.HasPrefix(t, "map["),
		strings.HasPrefix(t, "chan "),
		strings.HasPrefix(t, "<-chan"),
		strings.HasPrefix(t, "chan<-"),
		strings.HasPrefix(t, "func("),
		strings.HasPrefix(t, "func "),
		strings.HasPrefix(t, "interface{"),
		strings.HasPrefix(t, "struct{"),
		strings.HasPrefix(t, "["): // fixed-size array
		return true
	}
	return false
}

// typeSegment renders a Java, Kotlin or TypeScript type as a path
// segment plus the full spelling as an alias. A `.return` node has to
// answer to a bare NAME, so the type arguments, the array brackets, the
// package qualifier and Kotlin's nullable `?` all come off:
// `List<Int>` → List, `java.util.Map.Entry` → Entry, `Field<byte[]>` →
// Field, `Long[]` → Long, `RestResponse<DataSetResponse<AccountView>>` →
// RestResponse. The alias is "" when nothing was stripped.
//
// Measured before this existed: 531 of 10,738 Java return nodes (4.9%)
// and 199 of 208 TypeScript ones (96%) carried `<...>` or `[]` in their
// segment with an EMPTY alias, so `return#RestResponse` matched nothing
// and the full spelling was not recoverable either.
//
// A UNION (`string | null`) is left whole on purpose: it has no single
// leaf name to answer to, and inventing one would be a lie about which
// type the callable returns.
func typeSegment(full string) (seg, alias string) {
	seg = full
	if i := strings.Index(seg, "<"); i >= 0 {
		seg = seg[:i]
	}
	seg = strings.TrimSpace(seg)
	seg = strings.TrimSuffix(seg, "?")
	for strings.HasSuffix(seg, "[]") {
		seg = strings.TrimSuffix(seg, "[]")
	}
	seg = strings.TrimSpace(seg)
	if i := strings.LastIndex(seg, "."); i >= 0 {
		seg = seg[i+1:]
	}
	if seg != full {
		alias = full
	}
	return seg, alias
}

// refinedClassC assigns the class for a C/C++ symbol node. The ok
// return is false for node types this arm doesn't own, so refinedClass
// falls through to its shared default.
//
// There is no `const` class for variables on purpose: `const` in C
// qualifies a TYPE, not a binding (`const char *p` is a mutable pointer
// to constant chars), so deciding which side a qualifier applies to
// would be guesswork. The constants C actually declares — #define and
// enumerators — get the class directly.
func refinedClassC(node *sitter.Node, parentClass string, content []byte) (class string, branch, ok bool) {
	switch node.Type() {
	case "preproc_include":
		return "import", false, true
	case "preproc_def":
		return "const", false, true
	case "preproc_function_def":
		return "func", false, true
	case "using_declaration":
		return "import", false, true
	case "namespace_definition":
		return "module", true, true
	case "class_specifier":
		return "class", true, true
	case "struct_specifier", "union_specifier":
		// No separate `union` class in the vocabulary; a union is a
		// struct whose fields overlap, and selectors written for struct
		// should find it.
		return "struct", true, true
	case "enum_specifier":
		return "enum", true, true
	case "enumerator":
		return "const", false, true
	case "alias_declaration":
		return "type", false, true
	case "type_definition":
		// A typedef'd struct/enum with a body IS the type declaration in
		// C's dominant idiom (`typedef struct { int x; } Foo;`), so it
		// takes the underlying kind and branches to reach its fields.
		if u := cTypedefUnderlying(node); u != nil {
			switch u.Type() {
			case "struct_specifier", "union_specifier":
				return "struct", true, true
			case "enum_specifier":
				return "enum", true, true
			}
		}
		return "type", false, true
	case "function_definition":
		return cCallableClass(node, parentClass, content), false, true
	}
	if isCDeclarator(node.Type()) {
		if cFunctionDeclarator(node) != nil {
			return cCallableClass(node, parentClass, content), false, true
		}
		switch parentClass {
		case "class", "struct", "enum":
			return "field", false, true
		}
		return "var", false, true
	}
	return "", false, false
}

// cTypedefUnderlying returns the struct/union/enum specifier a typedef
// declares inline, or nil when the typedef merely renames an existing
// type (`typedef unsigned long ulong_t;`).
func cTypedefUnderlying(node *sitter.Node) *sitter.Node {
	ty := node.ChildByFieldName("type")
	if ty == nil {
		return nil
	}
	switch ty.Type() {
	case "struct_specifier", "union_specifier", "enum_specifier":
		// A body is what makes it a declaration rather than a reference
		// to a tag declared elsewhere.
		if ty.ChildByFieldName("body") != nil {
			return ty
		}
	}
	return nil
}

// cCallableClass decides func / method / ctor for a C/C++ callable.
// Constructors and destructors are both `ctor` (the vocabulary has no
// dtor): a destructor is spelled destructor_name, and a constructor is
// the member whose name equals its enclosing type's — in-class via the
// enclosing class_specifier, out-of-line via the qualified name's own
// scope (`Widget::Widget`).
func cCallableClass(node *sitter.Node, parentClass string, content []byte) string {
	name := cInnermostDeclarator(node)
	if name == nil {
		return "func"
	}
	if name.Type() == "destructor_name" {
		return "ctor"
	}
	if name.Type() == "qualified_identifier" {
		scope, leaf := cQualifiedParts(name, content)
		if leaf != nil && leaf.Type() == "destructor_name" {
			return "ctor"
		}
		if scope != "" && leaf != nil && scope == leaf.Content(content) {
			return "ctor"
		}
		return "method"
	}
	switch parentClass {
	case "class", "struct":
		if owner := cEnclosingTypeName(node, content); owner != "" && owner == name.Content(content) {
			return "ctor"
		}
		return "method"
	}
	return "func"
}

// cEnclosingTypeName walks up to the nearest class/struct/union
// specifier and returns its declared name ("" when anonymous or absent).
func cEnclosingTypeName(node *sitter.Node, content []byte) string {
	for p := node.Parent(); p != nil; p = p.Parent() {
		switch p.Type() {
		case "class_specifier", "struct_specifier", "union_specifier":
			if n := p.ChildByFieldName("name"); n != nil {
				return n.Content(content)
			}
			return ""
		case "translation_unit":
			return ""
		}
	}
	return ""
}

// cInnermostDeclarator peels a declarator chain (pointer, array,
// reference, parenthesized, init, function) down to the node that
// actually spells the name. Returns nil when there is none — an
// abstract declarator, e.g. the unnamed `int` in `void f(int);`.
func cInnermostDeclarator(node *sitter.Node) *sitter.Node {
	cur := node
	for range 16 { // depth guard: declarator chains are shallow in practice
		if cur == nil {
			return nil
		}
		switch cur.Type() {
		case "identifier", "field_identifier", "type_identifier",
			"qualified_identifier", "destructor_name", "operator_name":
			return cur
		case "template_function":
			// A template specialization (`Holder<T>::get`) — the name
			// field holds the callable's own name.
			if n := cur.ChildByFieldName("name"); n != nil {
				cur = n
				continue
			}
			return cur
		}
		if d := cur.ChildByFieldName("declarator"); d != nil {
			cur = d
			continue
		}
		// reference_declarator and parenthesized_declarator carry their
		// inner declarator positionally rather than under a field.
		switch cur.Type() {
		case "reference_declarator", "parenthesized_declarator":
			if cur.NamedChildCount() > 0 {
				cur = cur.NamedChild(0)
				continue
			}
		}
		return nil
	}
	return nil
}

// cFunctionDeclarator returns the function_declarator inside a
// declarator chain, or nil when the declaration declares data.
//
// A function-POINTER variable (`int (*fp)(void);`) also carries a
// function_declarator and is therefore classed func. That is a known
// mislabel, kept because the alternative — deciding by where the
// parentheses sit — is more machinery than the case is worth.
func cFunctionDeclarator(node *sitter.Node) *sitter.Node {
	cur := node
	for range 16 {
		if cur == nil {
			return nil
		}
		if cur.Type() == "function_declarator" {
			return cur
		}
		d := cur.ChildByFieldName("declarator")
		if d == nil {
			switch cur.Type() {
			case "reference_declarator", "parenthesized_declarator":
				if cur.NamedChildCount() > 0 {
					d = cur.NamedChild(0)
				}
			}
		}
		cur = d
	}
	return nil
}

// cQualifiedParts splits a qualified_identifier into its scope's LEAF
// name (Widget for Widget::area, Holder for Holder<T>::get) and the node
// naming the member. Nested scopes (a::b::c) recurse, so the scope
// returned is the innermost one — the type or namespace that owns the
// member.
func cQualifiedParts(node *sitter.Node, content []byte) (scope string, leaf *sitter.Node) {
	s := node.ChildByFieldName("scope")
	n := node.ChildByFieldName("name")
	if n != nil && n.Type() == "qualified_identifier" {
		return cQualifiedParts(n, content)
	}
	if s != nil {
		if s.Type() == "template_type" {
			// Holder<T> — the owner is the template's name.
			if tn := s.ChildByFieldName("name"); tn != nil {
				s = tn
			}
		}
		scope = s.Content(content)
	}
	return scope, n
}

// cSymbolName resolves a C/C++ symbol node to its local name. The ok
// return is false for nodes whose `name` field the generic path already
// handles (class/struct/enum specifiers, namespaces, enumerators,
// aliases, #define).
func cSymbolName(node *sitter.Node, content []byte) (string, *sitter.Node, bool) {
	switch node.Type() {
	case "preproc_include":
		path := node.ChildByFieldName("path")
		if path == nil {
			return "", nil, true
		}
		return includeBase(path.Content(content)), path, true
	case "using_declaration":
		if node.NamedChildCount() == 0 {
			return "", nil, true
		}
		inner := node.NamedChild(0)
		if inner.Type() == "qualified_identifier" {
			if _, leaf := cQualifiedParts(inner, content); leaf != nil {
				return leaf.Content(content), leaf, true
			}
		}
		return inner.Content(content), inner, true
	case "function_definition":
	default:
		if !isCDeclarator(node.Type()) {
			return "", nil, false
		}
	}
	name := cInnermostDeclarator(node)
	if name == nil {
		return "", nil, true // abstract declarator — anonymous
	}
	if name.Type() == "qualified_identifier" {
		if _, leaf := cQualifiedParts(name, content); leaf != nil {
			return leaf.Content(content), leaf, true
		}
	}
	return name.Content(content), name, true
}

// includeBase renders an #include path as a path segment: angle brackets
// and quotes stripped, directories dropped, extension cut — <sys/stat.h>
// and "app/widget.h" become `stat` and `widget`.
//
// The extension MUST go: a dot is the Sym path separator, so "widget.h"
// would read as a nested symbol.
func includeBase(s string) string {
	s = strings.Trim(s, "<>\"'")
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	return s
}

// cTypeSegment renders a C/C++ type as a path segment plus the full
// spelling as an alias. A `.return` node has to answer to a bare name,
// so the elaborated-type keyword, the namespace scope and the template
// arguments all come off: `struct Point` → Point, `std::vector<int>` →
// vector (alias std::vector<int>). The alias is "" when nothing was
// stripped.
func cTypeSegment(full string) (seg, alias string) {
	seg = full
	for _, kw := range [...]string{"struct ", "union ", "enum ", "class ", "const ", "typename "} {
		seg = strings.TrimPrefix(seg, kw)
	}
	if i := strings.Index(seg, "<"); i >= 0 {
		seg = seg[:i]
	}
	if i := strings.LastIndex(seg, "::"); i >= 0 {
		seg = seg[i+2:]
	}
	seg = strings.TrimSpace(seg)
	if seg != full {
		alias = full
	}
	return seg, alias
}

// cParamInfos handles C/C++ parameter forms. A lone `void` is C's way of
// spelling "no parameters" — it is not a parameter and must not become
// an anonymous `.argument` child.
func cParamInfos(p *sitter.Node, content []byte) []paramInfo {
	switch p.Type() {
	case "parameter_declaration", "optional_parameter_declaration",
		"variadic_parameter_declaration":
	case "variadic_parameter":
		return []paramInfo{{decl: p}}
	default:
		return nil
	}
	d := p.ChildByFieldName("declarator")
	if d == nil {
		if ty := p.ChildByFieldName("type"); ty != nil && ty.Content(content) == "void" {
			return nil
		}
		// Unnamed parameter (`void f(int);`) — anonymous, addressable
		// positionally, same as Go's `func f(int)`.
		return []paramInfo{{decl: p}}
	}
	if name := cInnermostDeclarator(d); name != nil {
		return []paramInfo{{name: name.Content(content), nameNode: name, decl: p}}
	}
	return []paramInfo{{decl: p}}
}

// classifyJava maps Java declarations onto the shared role vocabulary.
// Fields and locals follow the TypeScript shape: the declaration is the
// container and each variable_declarator is the symbol, so
// `int a, b;` yields two nodes rather than one.
func classifyJava(t, parent string) symRole {
	switch t {
	case "class_body", "interface_body", "enum_body",
		"enum_body_declarations", "annotation_type_body",
		"field_declaration", "local_variable_declaration",
		"constant_declaration":
		return roleContainer
	case "package_declaration", "import_declaration",
		// A JPMS module-info.java declares the MODULE. Without this the
		// file indexes as completely empty — measured on JDK 21's
		// java.base, the only one of 3,498 files that yielded nothing.
		//
		// Its `exports`/`requires` directives are deliberately NOT
		// symbols. Their identity is a full dotted path (java.io,
		// java.lang.annotation) and a sym-path segment cannot hold the
		// dots, so they would collapse to generic leaves — `io`, `lang`,
		// `util` — that collide with each other and bury real names.
		// One module name is worth more than 159 ambiguous ones.
		"module_declaration",
		"class_declaration", "interface_declaration",
		"enum_declaration", "record_declaration",
		"annotation_type_declaration", "annotation_type_element_declaration",
		"method_declaration", "constructor_declaration",
		"compact_constructor_declaration",
		"variable_declarator", "enum_constant":
		return roleSymbol
	}
	return roleSkip
}

func classifyGo(t, parent string) symRole {
	switch t {
	case "import_declaration", "import_spec_list",
		"const_declaration", "var_declaration",
		"type_declaration",
		"struct_type", "field_declaration_list",
		"interface_type":
		return roleContainer
	case "package_clause",
		"import_spec", "const_spec", "var_spec",
		"type_spec", "type_alias",
		"field_declaration", "method_elem",
		"function_declaration", "method_declaration":
		return roleSymbol
	}
	return roleSkip
}

func classifyTS(t, parent string) symRole {
	switch t {
	case "export_statement", "class_body", "interface_body", "enum_body",
		"lexical_declaration", "variable_declaration",
		// `.d.ts` files wrap everything in ambient_declaration
		// (`declare global { … }`, `declare module "react" { … }`),
		// nesting a statement_block inside. Without descending, an
		// ambient declaration file indexes as COMPLETELY EMPTY — which
		// is what a module augmentation looks like today, and those
		// files are precisely where a project states the contracts a
		// cross-language index wants.
		//
		// statement_block is also a function BODY, but callables have
		// branch=false so the walk never enters one; only these
		// declaration-level blocks are ever reached.
		"ambient_declaration", "statement_block":
		return roleContainer
	case "function_declaration", "generator_function_declaration",
		"class_declaration", "abstract_class_declaration",
		"interface_declaration", "type_alias_declaration",
		"enum_declaration", "method_definition",
		"public_field_definition", "method_signature",
		"property_signature", "variable_declarator",
		"import_statement", "internal_module", "module",
		// A bodyless function inside a `declare module` block.
		"function_signature":
		return roleSymbol
	}
	// Enum members appear as bare property_identifier / identifier under
	// enum_body.
	if parent == "enum_body" && (t == "property_identifier" || t == "identifier") {
		return roleSymbol
	}
	return roleSkip
}

func classifyPython(t, parent string) symRole {
	switch t {
	case "decorated_definition":
		return roleContainer
	case "block":
		if parent == "class_definition" {
			return roleContainer
		}
		return roleSkip
	case "expression_statement":
		// The wrapper around a module-level binding or a class
		// attribute. Reached only at module scope and inside a class
		// body — a function's `block` is not a container, so statements
		// in a function body are never walked.
		return roleContainer
	case "assignment":
		return roleSymbol
	case "function_definition", "class_definition",
		"import_statement", "import_from_statement":
		return roleSymbol
	}
	return roleSkip
}

func classifySQL(t, parent string) symRole {
	switch t {
	case "statement", "column_definitions",
		// ALTER TABLE is not itself a declaration, but ADD CONSTRAINT
		// inside it is: measured on a 34-file Flyway corpus, 492 of the
		// 620 alter_table statements carry one, and they were the single
		// largest unindexed declaration in a migration tree. The other
		// 128 are alter_column, which MODIFIES an existing column and
		// declares nothing.
		"alter_table":
		return roleContainer
	case "create_table", "column_definition", "add_constraint",
		"create_index", "create_view", "create_type",
		// Functions, triggers and sequences are declarations too, and a
		// migration tree is full of them: measured on a 34-file Flyway
		// corpus, 48 create_function / 80 create_trigger / 128
		// create_sequence, with SIX files — every PL/pgSQL function and
		// trigger file — indexing as completely empty without these.
		"create_function", "create_trigger", "create_sequence":
		return roleSymbol
	}
	return roleSkip
}

// refinedClass returns the final class + whether the symbol has nested
// children worth recursing into. An empty class means DECLINE: the node
// is not a symbol after all and is dropped along with its subtree.
func refinedClass(lang string, node *sitter.Node, parentClass string, content []byte) (class string, branch bool) {
	t := node.Type()
	switch lang {
	case "go":
		switch t {
		case "package_clause":
			return "module", false
		case "import_spec":
			return "import", false
		case "const_spec":
			return "const", false
		case "var_spec":
			return "var", false
		case "field_declaration":
			return "field", false
		case "method_elem":
			return "method", false
		case "function_declaration":
			return "func", false
		case "method_declaration":
			return "method", false
		case "type_spec", "type_alias":
			if u := node.ChildByFieldName("type"); u != nil {
				switch u.Type() {
				case "struct_type":
					return "struct", true
				case "interface_type":
					return "interface", true
				}
			}
			return "type", false
		}
	case "markdown":
		// A section with no heading is the preamble above the first `#`.
		// It names nothing, so it is DECLINED rather than emitted as an
		// anonymous node; its text still belongs to the file.
		if markdownHeadingText(node, content) == "" {
			return "", false
		}
		// `module` is the shared vocabulary's named-container slot — the
		// same one a Go package, a Kotlin package and a TS namespace use.
		return "module", true
	case "c", "cpp":
		if class, branch, ok := refinedClassC(node, parentClass, content); ok {
			return class, branch
		}
	case "kotlin":
		if class, branch, ok := refinedClassKotlin(node, parentClass, content); ok {
			return class, branch
		}
	case "groovy":
		if class, branch, ok := refinedClassGroovy(node, parentClass, content); ok {
			return class, branch
		}
	case "java":
		switch t {
		case "package_declaration":
			return "module", false
		case "module_declaration":
			return "module", false
		case "import_declaration":
			return "import", false
		case "class_declaration":
			return "class", true
		case "record_declaration":
			return "struct", true
		case "interface_declaration", "annotation_type_declaration":
			return "interface", true
		case "enum_declaration":
			return "enum", true
		case "enum_constant":
			return "const", false
		case "constructor_declaration", "compact_constructor_declaration":
			return "ctor", false
		case "method_declaration", "annotation_type_element_declaration":
			return "method", false
		case "variable_declarator":
			// A declarator under a field_declaration is a field; under a
			// local_variable_declaration it is a local var. constant_declaration
			// is an interface field, which is implicitly static final.
			switch parentClass {
			case "class", "interface", "enum", "struct":
				return "field", false
			}
			return "var", false
		}
	case "typescript":
		switch t {
		case "function_declaration", "generator_function_declaration",
			"function_signature":
			return "func", false
		case "class_declaration", "abstract_class_declaration":
			return "class", true
		case "interface_declaration":
			return "interface", true
		case "type_alias_declaration":
			return "type", false
		case "enum_declaration":
			return "enum", true
		case "method_definition":
			if n := node.ChildByFieldName("name"); n != nil && n.Content(content) == "constructor" {
				return "ctor", false
			}
			return "method", false
		case "method_signature":
			return "method", false
		case "public_field_definition", "property_signature":
			return "field", false
		case "variable_declarator":
			if p := node.Parent(); p != nil && p.Type() == "lexical_declaration" {
				if c := p.Child(0); c != nil && c.Content(content) == "const" {
					return "const", false
				}
			}
			return "var", false
		case "import_statement":
			return "import", false
		case "internal_module", "module":
			return "module", true
		case "property_identifier", "identifier":
			return "field", false // enum member
		}
	case "python":
		switch t {
		case "assignment":
			// Python declares its module constants and its
			// dataclass/pydantic model fields as plain assignments.
			// Measured on a live 194-file tree: 891 such declarations
			// (343 module-level, 548 class attributes, 541 of them
			// type-annotated) were invisible, while every other language
			// arm indexes its consts and fields.
			//
			// A binding whose left side is not a plain name — tuple
			// unpacking `a, b = f()`, an attribute or subscript target —
			// is DECLINED rather than given a made-up segment.
			left := node.ChildByFieldName("left")
			if left == nil || left.Type() != "identifier" {
				return "", false
			}
			if parentClass == "class" {
				return "field", false
			}
			// No `const` class: Python has no const keyword, and
			// UPPER_CASE is a convention the parser cannot verify.
			return "var", false
		case "class_definition":
			return "class", true
		case "import_statement", "import_from_statement":
			return "import", false
		case "function_definition":
			name := ""
			if n := node.ChildByFieldName("name"); n != nil {
				name = n.Content(content)
			}
			if parentClass == "class" {
				if name == "__init__" {
					return "ctor", false
				}
				return "method", false
			}
			return "func", false
		}
	case "sql":
		switch t {
		case "create_table":
			return "struct", true
		case "column_definition":
			return "field", false
		case "create_function":
			// A stored function is the one SQL object that is genuinely
			// callable.
			return "func", false
		case "create_index", "create_view", "create_type",
			// Triggers, sequences and constraints join index/view under
			// `type`: named database objects with no better slot in the
			// shared vocabulary. Keeping them together is the existing
			// convention here, not a new judgement.
			"create_trigger", "create_sequence", "add_constraint":
			return "type", false
		}
	}
	return "type", false
}

// symbolLocalName returns the symbol's local (undotted) name and the
// identifier node whose range answers node_references / rename. Empty
// name marks an anonymous member (rendered "[n]").
func symbolLocalName(lang string, node *sitter.Node, content []byte) (string, *sitter.Node) {
	t := node.Type()

	// Go import: alias if present, else the path's last segment.
	if lang == "go" && t == "import_spec" {
		if alias := node.ChildByFieldName("name"); alias != nil {
			return alias.Content(content), alias
		}
		if path := node.ChildByFieldName("path"); path != nil {
			return importBase(path.Content(content)), path
		}
		return "", nil
	}
	// TS `declare module "react"` / `module "*.svg"`: the name node is a
	// STRING, so the quotes would land in the path segment. Reuse the
	// import spelling — the last meaningful path segment — so an ambient
	// module answers to the same name an `import` of it would.
	if lang == "typescript" && (t == "module" || t == "internal_module") {
		if n := node.ChildByFieldName("name"); n != nil && n.Type() == "string" {
			return importBase(n.Content(content)), n
		}
	}
	// TS import: last segment of the source module string.
	if lang == "typescript" && t == "import_statement" {
		if src := node.ChildByFieldName("source"); src != nil {
			return importBase(src.Content(content)), src
		}
		return "", node
	}
	// C/C++ names hide inside declarator chains, so they resolve before
	// the generic `name`-field lookups (which would find the TYPE of a
	// declaration, not the thing being declared).
	if lang == "c" || lang == "cpp" {
		if name, node, ok := cSymbolName(node, content); ok {
			return name, node
		}
	}
	if lang == "groovy" {
		if name, node, ok := groovyName(node, content); ok {
			return name, node
		}
	}
	// A markdown section answers to its heading TEXT, markers stripped.
	if lang == "markdown" {
		if h := markdownHeadingNode(node); h != nil {
			if txt := markdownHeadingText(node, content); txt != "" {
				return txt, h
			}
		}
		return "", nil
	}
	// Python assignment: the bound NAME is the `left` field.
	if lang == "python" && t == "assignment" {
		if n := node.ChildByFieldName("left"); n != nil {
			return n.Content(content), n
		}
		return "", nil
	}
	// A Java module declaration names itself with a scoped_identifier
	// (java.base). Like a package, it answers to the LEAF — a dotted
	// path would split the sym path.
	if lang == "java" {
		switch t {
		case "module_declaration":
			if sc := firstNamedChildOfType(node, "scoped_identifier"); sc != nil {
				if leaf := lastNamedChildOfType(sc, "identifier"); leaf != nil {
					return leaf.Content(content), leaf
				}
			}
			if id := firstNamedChildOfType(node, "identifier"); id != nil {
				return id.Content(content), id
			}
			return "", nil
		}
	}
	// Kotlin's grammar has no field names at all, so every name is
	// resolved positionally before the generic lookups.
	if lang == "kotlin" {
		if name, node, ok := kotlinName(node, content); ok {
			return name, node
		}
	}
	// SQL objects introduced by CREATE name themselves through an
	// object_reference, whose LAST identifier is the object and whose
	// leading ones are the schema (redline.account → account). This
	// mirrors how every other language answers to the leaf.
	if lang == "sql" {
		switch t {
		case "create_function", "create_trigger", "create_sequence":
			if ref := firstNamedChildOfType(node, "object_reference"); ref != nil {
				if leaf := lastNamedChildOfType(ref, "identifier"); leaf != nil {
					return leaf.Content(content), leaf
				}
			}
		case "add_constraint":
			// ADD CONSTRAINT <name> — the identifier sits directly under
			// the clause, before the `constraint` body that says what
			// kind it is.
			if n := firstNamedChildOfType(node, "identifier"); n != nil {
				return n.Content(content), n
			}
		}
	}
	// SQL create_index names via the `column` field (index name).
	if lang == "sql" && t == "create_index" {
		if n := node.ChildByFieldName("column"); n != nil {
			return n.Content(content), n
		}
	}
	// Enum members are bare identifiers — the node IS the name.
	if t == "property_identifier" || (t == "identifier" && lang == "typescript") {
		return node.Content(content), node
	}

	if n := node.ChildByFieldName("name"); n != nil {
		return n.Content(content), n
	}
	if n := findStructureNameNode(node); n != nil {
		return n.Content(content), n
	}
	return "", nil
}

// importBase strips quotes and returns the last MEANINGFUL path segment
// of an import/module string: "encoding/json" -> "json", and Go major-
// version suffixes are skipped ("github.com/…/huma/v2" -> "huma") —
// that's the package the code actually says ("huma.Register"), and it's
// what makes `import#huma` the decl node qualified references resolve to.
func importBase(s string) string {
	s = strings.Trim(s, "\"'`")
	segs := strings.Split(s, "/")
	i := len(segs) - 1
	for i > 0 && goVersionSeg.MatchString(segs[i]) {
		i--
	}
	s = segs[i]
	if j := strings.LastIndexByte(s, '.'); j >= 0 && j < len(s)-1 {
		s = s[j+1:]
	}
	return s
}

var goVersionSeg = regexp.MustCompile(`^v\d+$`)

// parentOverride returns a synthetic parent-path segment for a symbol
// whose logical owner isn't its tree parent: Go methods, whose owner is
// the receiver type (Server.Start), and C++ out-of-line definitions,
// whose owner is the qualified name's scope (Widget::area lands at
// Widget.area even though the AST parents it to the file).
func parentOverride(lang string, node *sitter.Node, content []byte) string {
	if lang == "go" && node.Type() == "method_declaration" {
		return goReceiverType(node, content)
	}
	if lang == "c" || lang == "cpp" {
		if name := cInnermostDeclarator(node); name != nil && name.Type() == "qualified_identifier" {
			scope, _ := cQualifiedParts(name, content)
			return scope
		}
	}
	if lang == "sql" && node.Type() == "add_constraint" {
		// A constraint belongs to the table it is added to, which the
		// enclosing ALTER TABLE names — the same reasoning that files a
		// Go method under its receiver. The table is frequently declared
		// in an EARLIER migration, so the prefix often has no parent
		// node in this file; that is fine, and matches how a Go method
		// whose type lives in another file behaves.
		if p := node.Parent(); p != nil && p.Type() == "alter_table" {
			if ref := firstNamedChildOfType(p, "object_reference"); ref != nil {
				if leaf := lastNamedChildOfType(ref, "identifier"); leaf != nil {
					return leaf.Content(content)
				}
			}
		}
		return ""
	}
	if lang == "kotlin" && node.Type() == "function_declaration" {
		// An extension function is owned by its receiver type
		// (String.shout), the Kotlin analogue of a Go receiver.
		if recv, _, _ := kotlinFuncParts(node); recv != nil {
			seg, _ := typeSegment(collapseType(nodeSlice(recv, content)))
			return seg
		}
	}
	return ""
}

// goReceiverType extracts the receiver type name from a Go method
// declaration ("(s *Server)" -> "Server"). Empty if it can't be found.
func goReceiverType(node *sitter.Node, content []byte) string {
	recv := node.ChildByFieldName("receiver")
	if recv == nil {
		return ""
	}
	cnt := int(recv.NamedChildCount())
	for i := 0; i < cnt; i++ {
		pd := recv.NamedChild(i)
		if pd.Type() != "parameter_declaration" {
			continue
		}
		typ := pd.ChildByFieldName("type")
		if typ == nil {
			continue
		}
		// Strip pointer_type wrapper.
		if typ.Type() == "pointer_type" && typ.NamedChildCount() > 0 {
			typ = typ.NamedChild(0)
		}
		return typ.Content(content)
	}
	return ""
}

// declRangeNode returns the node whose range is the symbol's whole
// declaration. For Go grouped declarations with a SINGLE spec, this is
// the outer declaration (so "type X struct{...}" including the keyword),
// while multi-spec groups keep per-spec ranges.
func declRangeNode(node *sitter.Node) *sitter.Node {
	p := node.Parent()
	if p == nil {
		return node
	}
	switch p.Type() {
	case "type_declaration", "const_declaration", "var_declaration":
		if countSpecChildren(p) == 1 {
			return p
		}
	case "declaration", "field_declaration":
		// Java spells its declarator variable_declarator, which the C
		// count deliberately ignores — so a java field's range used to be
		// the bare name (`name`, not `String name;`), and node_read handed
		// back an identifier with no type. Same single-declarator rule:
		// `int a, b;` keeps per-declarator ranges.
		if countVariableDeclarators(p) == 1 {
			return p
		}
		// C/C++: the symbol is the declarator, but the DECLARATION is
		// what a reader (and node_edit) means by the thing — type,
		// storage class and semicolon included. `int a, b;` keeps
		// per-declarator ranges so the two siblings don't overlap.
		// Java also has field_declaration, but its declarator children
		// are variable_declarator nodes, which this count ignores.
		if countCDeclarators(p) == 1 {
			return p
		}
	case "template_declaration":
		// The template header is part of the declaration it introduces.
		return p
	case "export_statement":
		// TypeScript: `export` is part of the declaration, and the doc
		// comment sits above the EXPORT, not above `function`. Without
		// this the span was the bare `function f() {}` — every exported
		// symbol, which is to say the ones a caller cares about, read
		// back undocumented, and replacing one stranded its comment.
		return p
	case "lexical_declaration", "variable_declaration":
		// `const x = 1` — the symbol is the declarator, but the keyword
		// and the semicolon are part of the thing, and the doc comment
		// precedes the keyword. `const a = 1, b = 2` keeps per-declarator
		// ranges so the two do not overlap.
		if countVariableDeclarators(p) != 1 {
			return node
		}
		if g := p.Parent(); g != nil && g.Type() == "export_statement" {
			return g
		}
		return p
	}
	// SQL wraps every CREATE in a `statement` node, and the doc comment sits
	// above the WRAPPER. Without this the span was the bare create_table and
	// `-- doc` above it was dropped.
	if isStatementWrapper(p) {
		return p
	}
	return node
}

func countSpecChildren(p *sitter.Node) int {
	n := 0
	cnt := int(p.NamedChildCount())
	for i := 0; i < cnt; i++ {
		if strings.HasSuffix(p.NamedChild(i).Type(), "_spec") {
			n++
		}
	}
	return n
}

// countVariableDeclarators counts the variable_declarator children of a
// declaration. Java, groovy and typescript all spell declarators this way —
// c and c++ use init_declarator and friends — so this stays zero for them and
// the C rule is left untouched. More than one means `int a, b;`, where each
// symbol must keep its own range or the two would overlap.
func countVariableDeclarators(p *sitter.Node) int {
	n := 0
	for i := range int(p.NamedChildCount()) {
		if p.NamedChild(i).Type() == "variable_declarator" {
			n++
		}
	}
	return n
}

func countCDeclarators(p *sitter.Node) int {
	n := 0
	cnt := int(p.NamedChildCount())
	for i := 0; i < cnt; i++ {
		if isCDeclarator(p.NamedChild(i).Type()) {
			n++
		}
	}
	return n
}
