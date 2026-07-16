package symbols

import (
	"context"
	"fmt"
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
//     ctor, module, import.
//   - Decl* — the whole declaration range (1-based, end-exclusive).
//     node_read / node_edit / node_delete address this.
//   - Name* — just the identifier range. node_references / node_refactor
//     address this. Falls back to the decl range for anonymous nodes.
type Symbol struct {
	Sym   string
	Class string

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
			sym.DeclStartLine, sym.DeclStartCol, sym.DeclEndLine, sym.DeclEndCol = nodeLineCols(decl)
			if in.nameNode != nil {
				sym.NameStartLine, sym.NameStartCol, sym.NameEndLine, sym.NameEndCol = nodeLineCols(in.nameNode)
			} else {
				sym.NameStartLine, sym.NameStartCol = sym.DeclStartLine, sym.DeclStartCol
				sym.NameEndLine, sym.NameEndCol = sym.DeclEndLine, sym.DeclEndCol
			}
			out = append(out, sym)

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

// importBase strips quotes and returns the last path segment of an
// import/module string ("encoding/json" -> "json").
func importBase(s string) string {
	s = strings.Trim(s, "\"'`")
	if i := strings.LastIndexAny(s, "/."); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	return s
}

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
