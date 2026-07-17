package symbols

import (
	"context"

	sitter "github.com/smacker/go-tree-sitter"
)

// RefKind classifies how a reference SITE uses a name. The vocabulary
// is closed and deliberately small — only what tree-sitter context
// makes cheap and unambiguous:
//
//	call    the name is in the function position of a call/new
//	type    the name is used AS a type (annotation, decl, composite)
//	import  the name appears inside an import statement
//	""      everything else (pointer refs, plain reads, …) — a bare,
//	        unclassified reference. More kinds are a follow-up, not a
//	        reshaping: sites carry the kind, selectors filter on it.
//
// Anything semantic (Go's implicit interface satisfaction, aliasing)
// belongs to a child-LSP precision pass, not here.

// SiteClass is a reference site on two ORTHOGONAL axes: Kind is WHAT the
// reference is (call/type/import), Pos is WHERE the occurrence sits
// (return/param/field/var). They compose in the selector as
// ::in.return.type — position filters any kind, not just type.
type SiteClass struct {
	Kind string
	Pos  string
}

// SiteKinds classifies many (line, col) positions in one parse. Input
// positions are 1-based (the Site convention); unknown languages or
// unparseable content classify everything as the zero SiteClass.
func SiteKinds(language string, content []byte, positions [][2]int) []SiteClass {
	out := make([]SiteClass, len(positions))
	lang := LanguageByName(language)
	if lang == nil {
		return out
	}
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil || tree == nil {
		return out
	}
	defer tree.Close()
	root := tree.RootNode()
	for i, pos := range positions {
		pt := sitter.Point{Row: uint32(pos[0] - 1), Column: uint32(pos[1] - 1)}
		n := root.NamedDescendantForPointRange(pt, pt)
		if n == nil {
			continue
		}
		out[i] = SiteClass{Kind: classifySiteNode(language, n), Pos: sitePosition(n)}
	}
	return out
}

// sitePosition walks up from the occurrence to the syntactic slot it
// sits in — return / param / field / var — orthogonal to its kind. The
// slot is decided by the nearest enclosing context; a return type is
// recognised by the occurrence lying in a function's result /
// return_type field (Go / TS / Python respectively). "" when the
// occurrence is in no distinguished slot (a body expression).
func sitePosition(n *sitter.Node) string {
	for cur := n; cur != nil; cur = cur.Parent() {
		switch cur.Type() {
		case "parameter_declaration", "variadic_parameter_declaration", // go
			"required_parameter", "optional_parameter", // ts
			"typed_parameter", "typed_default_parameter", "default_parameter": // py
			return "param"
		case "field_declaration", // go struct field
			"public_field_definition", "property_signature": // ts
			return "field"
		case "var_spec", "const_spec", "short_var_declaration": // go
			return "var"
		}
		// A return type is the occurrence lying in the function's result
		// (Go) or return_type (TS/Python) field. Checked at every level so
		// a pointer/generic wrapper between the type and the field is fine.
		if r := cur.ChildByFieldName("result"); r != nil && nodeContainsNode(r, n) {
			return "return"
		}
		if r := cur.ChildByFieldName("return_type"); r != nil && nodeContainsNode(r, n) {
			return "return"
		}
	}
	return ""
}

// classifySiteNode walks up from the identifier node, nearest context
// wins. The ancestor walk is bounded — a site's kind is decided within
// a few levels or not at all.
func classifySiteNode(language string, n *sitter.Node) string {
	// The name used AS a type is visible on the node itself in the
	// grammars we ship (go/ts/tsx emit type_identifier).
	if n.Type() == "type_identifier" {
		return "type"
	}
	cur := n
	for depth := 0; cur != nil && depth < 8; depth, cur = depth+1, cur.Parent() {
		switch cur.Type() {
		case "call_expression", "new_expression", "call": // go/ts, ts, python
			// Only the FUNCTION position is a call — an argument that
			// happens to be a func name is a plain reference.
			if f := cur.ChildByFieldName("function"); f != nil && nodeContainsNode(f, n) {
				return "call"
			}
			if cur.Type() == "new_expression" {
				if c := cur.ChildByFieldName("constructor"); c != nil && nodeContainsNode(c, n) {
					return "call"
				}
			}
			return ""
		case "import_declaration", "import_spec", "import_statement",
			"import_from_statement", "import_clause":
			return "import"
		case "type_annotation", "type_arguments", "type_parameter",
			"extends_clause", "implements_clause", "class_heritage":
			return "type"
		}
	}
	_ = language
	return ""
}

func nodeContainsNode(outer, inner *sitter.Node) bool {
	return outer.StartByte() <= inner.StartByte() && inner.EndByte() <= outer.EndByte()
}
