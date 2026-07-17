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
			out = append(out, sym)

			// Parameter DECLARATIONS become addressable `.argument`
			// children of their func/method/ctor. Emitted here rather
			// than through the classify/gather walk because params are
			// not "symbols at this level" — they hang off the owning
			// node's `parameters` field, and routing them through
			// gather would force branch=true on every func (which would
			// then also drag in body-local declarations).
			appendParamSymbols(language, in.node, full, in.class, content, &out)

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
	// Go's method receiver lives on a separate `receiver` field, so
	// keying on `parameters` naturally excludes it.
	params := node.ChildByFieldName("parameters")
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
	}
	return nil
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
func declLineCols(n *sitter.Node) (startLine, startCol, endLine, endCol int) {
	startLine, startCol, endLine, endCol = nodeLineCols(n)
	for cur := n; ; {
		prev := cur.PrevSibling()
		if prev == nil || prev.Type() != "comment" {
			break
		}
		pStart, pCol, pEnd, _ := nodeLineCols(prev)
		if pEnd+1 != startLine { // blank line (or same line) → not a doc comment
			break
		}
		startLine, startCol = pStart, pCol
		cur = prev
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
		"lexical_declaration", "variable_declaration":
		return roleContainer
	case "function_declaration", "generator_function_declaration",
		"class_declaration", "abstract_class_declaration",
		"interface_declaration", "type_alias_declaration",
		"enum_declaration", "method_definition",
		"public_field_definition", "method_signature",
		"property_signature", "variable_declarator",
		"import_statement", "internal_module", "module":
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
	case "function_definition", "class_definition",
		"import_statement", "import_from_statement":
		return roleSymbol
	}
	return roleSkip
}

func classifySQL(t, parent string) symRole {
	switch t {
	case "statement", "column_definitions":
		return roleContainer
	case "create_table", "column_definition",
		"create_index", "create_view", "create_type":
		return roleSymbol
	}
	return roleSkip
}

// refinedClass returns the final class + whether the symbol has nested
// children worth recursing into.
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
	case "typescript":
		switch t {
		case "function_declaration", "generator_function_declaration":
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
		case "create_index":
			return "type", false
		case "create_view":
			return "type", false
		case "create_type":
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
	// TS import: last segment of the source module string.
	if lang == "typescript" && t == "import_statement" {
		if src := node.ChildByFieldName("source"); src != nil {
			return importBase(src.Content(content)), src
		}
		return "", node
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
// whose logical owner isn't its tree parent. Today: Go methods, whose
// owner is the receiver type (Server.Start), not the file root.
func parentOverride(lang string, node *sitter.Node, content []byte) string {
	if lang == "go" && node.Type() == "method_declaration" {
		return goReceiverType(node, content)
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
