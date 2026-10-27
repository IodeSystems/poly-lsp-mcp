package symbols

import (
	"strings"

	sitter "github.com/iodesystems/poly-lsp-mcp/internal/ts"
)

// ------------------------------------------------------- swift
//
// tree-sitter-swift (alex-pinkus, at smacker's pin) spells every nominal
// type `class_declaration` and tells class / struct / enum / actor /
// extension apart only by the anonymous keyword in its `declaration_kind`
// field. Its field names are loose: a parameter's TYPE and a function's
// RETURN type both sit in a field called `name`, so the helpers below take
// the first `name` child of the expected node type, and find the return
// type by position (the named child after `->`).
//
// A property is `property_declaration` > `pattern`, and one declaration can
// bind several patterns (`let a = 1, b = 2`); an enum `case a, b` is one
// enum_entry naming two cases. Both follow the java/typescript shape: the
// declaration is a container, each pattern / case name is a symbol, and a
// declaration naming exactly one takes the whole declaration as its span.

// swiftTypeBodies are the nodes whose direct children are members.
var swiftTypeBodies = map[string]bool{"class_body": true, "enum_class_body": true, "protocol_body": true}

// classifySwift maps Swift declarations onto the shared role vocabulary.
// An extension is a class_declaration too; FileSymbols decides its role by
// node (see swiftExtensions), since this sees types only.
func classifySwift(t, parent string) symRole {
	switch t {
	case "class_body", "enum_class_body", "protocol_body",
		"property_declaration", "enum_entry",
		// Walked through for kotlin's reason (see classifyKotlin): one
		// statement the grammar cannot read relabels the enclosing body
		// ERROR, and the members it recovered hang off that node.
		"ERROR":
		return roleContainer
	case "pattern":
		if parent == "property_declaration" {
			return roleSymbol
		}
		return roleSkip
	case "simple_identifier":
		// The case names of an enum_entry; its associated values and raw
		// value sit deeper or are literals.
		if parent == "enum_entry" {
			return roleSymbol
		}
		return roleSkip
	case "import_declaration", "class_declaration", "protocol_declaration",
		"function_declaration", "protocol_function_declaration",
		"init_declaration", "deinit_declaration", "subscript_declaration",
		"typealias_declaration", "associatedtype_declaration",
		"protocol_property_declaration":
		return roleSymbol
	}
	return roleSkip
}

// swiftDeclKind returns a class_declaration's keyword: class, struct, enum,
// actor or extension.
func swiftDeclKind(node *sitter.Node, content []byte) string {
	for i := range int(node.ChildCount()) {
		if node.FieldNameForChild(i) == "declaration_kind" {
			return node.Child(i).Content(content)
		}
	}
	return ""
}

// swiftInTypeBody reports whether a member node sits directly in a type's
// body (through a property_declaration / enum_entry / ERROR wrapper) rather
// than at file scope. Decided from the tree, not the parent symbol's class,
// because a member of an extension of a type declared elsewhere has no
// parent symbol at all.
func swiftInTypeBody(node *sitter.Node) bool {
	for p := node.Parent(); p != nil; p = p.Parent() {
		switch t := p.Type(); {
		case swiftTypeBodies[t]:
			return true
		case t == "property_declaration", t == "enum_entry", t == "ERROR":
			continue
		default:
			return false
		}
	}
	return false
}

// refinedClassSwift assigns the class for a Swift symbol node. The ok
// return is false for node types this arm doesn't own.
func refinedClassSwift(node *sitter.Node, content []byte) (class string, branch, ok bool) {
	switch node.Type() {
	case "import_declaration":
		return "import", false, true
	case "class_declaration":
		switch swiftDeclKind(node, content) {
		case "struct":
			return "struct", true, true
		case "enum":
			return "enum", true, true
		}
		// class and actor. An actor is a reference type with isolated
		// state; selectors written for class should find it.
		return "class", true, true
	case "protocol_declaration":
		return "interface", true, true
	case "function_declaration", "protocol_function_declaration", "subscript_declaration":
		if swiftInTypeBody(node) {
			return "method", false, true
		}
		return "func", false, true
	case "init_declaration", "deinit_declaration":
		// No dtor in the vocabulary; C++ files its destructors as ctor too.
		return "ctor", false, true
	case "typealias_declaration", "associatedtype_declaration":
		return "type", false, true
	case "protocol_property_declaration":
		return "field", false, true
	case "pattern":
		if swiftInTypeBody(node) {
			return "field", false, true
		}
		// File scope: `let` is an immutable binding, the distinction
		// kotlin's val and TypeScript's const draw.
		if decl := node.Parent(); decl != nil {
			if vb := firstNamedChildOfType(decl, "value_binding_pattern"); vb != nil && strings.HasPrefix(vb.Content(content), "let") {
				return "const", false, true
			}
		}
		return "var", false, true
	case "simple_identifier":
		return "const", false, true // an enum case
	}
	return "", false, false
}

// swiftName resolves a Swift symbol node to its local name. ok is false for
// node types the generic path should handle.
func swiftName(node *sitter.Node, content []byte) (string, *sitter.Node, bool) {
	switch node.Type() {
	case "import_declaration":
		// `import Foundation`, `import struct Foo.Bar`: the last segment.
		if id := firstNamedChildOfType(node, "identifier"); id != nil {
			if leaf := lastNamedChildOfType(id, "simple_identifier"); leaf != nil {
				return leaf.Content(content), leaf, true
			}
		}
		return "", nil, true
	case "class_declaration", "protocol_declaration", "typealias_declaration", "associatedtype_declaration":
		if n := firstFieldChildOfType(node, "name", "type_identifier"); n != nil {
			return n.Content(content), n, true
		}
		return "", nil, true
	case "function_declaration", "protocol_function_declaration":
		n := node.ChildByFieldName("name")
		if n == nil {
			return "", nil, true
		}
		// An operator function (`static func + (…)`) is named by its
		// operator token. A dot would split the sym path (`..<`).
		name := n.Content(content)
		if strings.Contains(name, ".") {
			return "", nil, true
		}
		return name, n, true
	case "init_declaration", "deinit_declaration", "subscript_declaration":
		// Named the way Swift code spells them (`self.init(…)`,
		// `Greeter.init`), so overloads read init[1], init[2]. The only
		// name token is the keyword, so there is no name range to point
		// at: it falls back to the declaration span, and rename does not
		// pin on a keyword.
		return strings.TrimSuffix(node.Type(), "_declaration"), nil, true
	case "pattern", "protocol_property_declaration":
		pat := node
		if node.Type() == "protocol_property_declaration" {
			if pat = firstNamedChildOfType(node, "pattern"); pat == nil {
				return "", nil, true
			}
		}
		if n := pat.ChildByFieldName("bound_identifier"); n != nil {
			return n.Content(content), n, true
		}
		// A destructuring `let (a, b) = t` answers to its first name.
		if n := findDescendantOfType(pat, "simple_identifier", 3); n != nil {
			return n.Content(content), n, true
		}
		return "", nil, true
	case "simple_identifier":
		return node.Content(content), node, true
	}
	return "", nil, false
}

// firstFieldChildOfType returns the first child of n in field `field`
// whose node type is t. Swift's grammar reuses `name` for a declaration's
// name AND its types, so the field alone is not enough.
func firstFieldChildOfType(n *sitter.Node, field, t string) *sitter.Node {
	for i := range int(n.ChildCount()) {
		if n.FieldNameForChild(i) == field {
			if c := n.Child(i); c.Type() == t {
				return c
			}
		}
	}
	return nil
}

// swiftParamInfos handles Swift's `parameter` node: an optional external
// label, the internal name, the type. The internal name is the declared
// binding, so it names the .argument node (`_ who: String` → who).
func swiftParamInfos(p *sitter.Node, content []byte) []paramInfo {
	if p.Type() != "parameter" {
		return nil
	}
	if n := firstFieldChildOfType(p, "name", "simple_identifier"); n != nil {
		return []paramInfo{{name: n.Content(content), nameNode: n, decl: p}}
	}
	return []paramInfo{{decl: p}}
}

// swiftReturnType is the named child after `->`, or nil.
func swiftReturnType(node *sitter.Node) *sitter.Node {
	arrow := false
	for i := range int(node.ChildCount()) {
		c := node.Child(i)
		if !c.IsNamed() {
			if c.Type() == "->" {
				arrow = true
			}
			continue
		}
		if arrow {
			return c
		}
	}
	return nil
}

// swiftTypeSegment renders a Swift type as a path segment plus the full
// spelling as an alias: the `some` / `any` prefix, an optional's `?` / `!`,
// generic arguments and a module qualifier come off (`some View` → View,
// `Result<Int, Error>?` → Result, `Foundation.URL` → URL). An array,
// dictionary, tuple or function type has no single name and is kept whole,
// as Go keeps `[]T`.
func swiftTypeSegment(full string) (seg, alias string) {
	seg = full
	for _, p := range [...]string{"some ", "any "} {
		seg = strings.TrimPrefix(seg, p)
	}
	seg = strings.TrimRight(seg, "?!")
	if strings.HasPrefix(seg, "[") || strings.HasPrefix(seg, "(") {
		return full, ""
	}
	if i := strings.Index(seg, "<"); i >= 0 {
		seg = seg[:i]
	}
	if i := strings.LastIndex(seg, "."); i >= 0 {
		seg = seg[i+1:]
	}
	seg = strings.TrimSpace(seg)
	if seg != full {
		alias = full
	}
	return seg, alias
}

// swiftAttributes collects the `attribute` nodes of a declaration's
// modifiers (@objc, @MainActor, @available(iOS 15, *)). A property's
// modifiers belong to its property_declaration, one level above the
// pattern symbol. The attribute's type name is the leaf; the type as
// written (Foo.Wrapper) is the alias.
func swiftAttributes(node *sitter.Node, content []byte) []annMark {
	mods := firstNamedChildOfType(node, "modifiers")
	if mods == nil && node.Type() == "pattern" {
		if p := node.Parent(); p != nil && p.Type() == "property_declaration" && countNamedOfType(p, "pattern") == 1 {
			mods = firstNamedChildOfType(p, "modifiers")
		}
	}
	if mods == nil {
		return nil
	}
	var out []annMark
	for i := range int(mods.NamedChildCount()) {
		attr := mods.NamedChild(i)
		if attr.Type() != "attribute" {
			continue
		}
		ut := firstNamedChildOfType(attr, "user_type")
		if ut == nil {
			continue
		}
		leaf := lastNamedChildOfType(ut, "type_identifier")
		if leaf == nil {
			continue
		}
		out = append(out, annMark{attr, leaf.Content(content), collapseType(nodeSlice(ut, content))})
	}
	return out
}

func countNamedOfType(n *sitter.Node, t string) int {
	c := 0
	for i := range int(n.NamedChildCount()) {
		if n.NamedChild(i).Type() == t {
			c++
		}
	}
	return c
}

// swiftSoleMember reports whether node is the only symbol its wrapping
// declaration names — the one pattern of a property_declaration or the one
// case of an enum_entry — so the wrapper (keyword, modifiers, doc comment)
// is the symbol's declaration.
func swiftSoleMember(node *sitter.Node) (*sitter.Node, bool) {
	p := node.Parent()
	if p == nil {
		return nil, false
	}
	switch {
	case p.Type() == "property_declaration" && node.Type() == "pattern":
		return p, countNamedOfType(p, "pattern") == 1
	case p.Type() == "enum_entry" && node.Type() == "simple_identifier":
		return p, countFieldChildren(p, "name") == 1
	}
	return nil, false
}

func countFieldChildren(n *sitter.Node, field string) int {
	c := 0
	for i := range int(n.ChildCount()) {
		if n.FieldNameForChild(i) == field {
			c++
		}
	}
	return c
}

// ------------------------------------------------------- swift extensions

// swiftExtensions decides where the members of each `extension` land.
//
// An extension of a type declared in the SAME file is merged into it: its
// members are gathered at the type's own level, so `Greeter.wave` is one
// path whether wave sits in the class body or an extension, and an overload
// split across the two disambiguates as greet[1], greet[2] instead of
// colliding as two `Greeter.greet`. An extension of a type declared
// elsewhere (`extension String`, or `Greeter+Networking.swift`) files its
// members under the extended type's name, the way a Go method is filed
// under its receiver; several extensions of that type share one level.
// The extension block itself is not a node.
type swiftExtensions struct {
	merged   map[string][]*sitter.Node // type path -> extensions merged into it
	of       map[*sitter.Node]string   // extension node -> extended type path
	isMerged map[*sitter.Node]bool
}

func newSwiftExtensions(root *sitter.Node, content []byte) *swiftExtensions {
	x := &swiftExtensions{merged: map[string][]*sitter.Node{}, of: map[*sitter.Node]string{}, isMerged: map[*sitter.Node]bool{}}
	declared := map[string]bool{}
	var types func(n *sitter.Node, prefix string)
	types = func(n *sitter.Node, prefix string) {
		for i := range int(n.NamedChildCount()) {
			c := n.NamedChild(i)
			switch c.Type() {
			case "class_declaration", "protocol_declaration":
				if swiftDeclKind(c, content) == "extension" {
					continue
				}
				name, _, _ := swiftName(c, content)
				if name == "" {
					continue
				}
				path := name
				if prefix != "" {
					path = prefix + "." + name
				}
				declared[path] = true
				if body := c.ChildByFieldName("body"); body != nil {
					types(body, path)
				}
			case "ERROR":
				types(c, prefix)
			}
		}
	}
	types(root, "")
	for i := range int(root.NamedChildCount()) {
		c := root.NamedChild(i)
		if c.Type() != "class_declaration" || swiftDeclKind(c, content) != "extension" {
			continue
		}
		path := swiftExtendedType(c, content)
		if path == "" {
			continue
		}
		x.of[c] = path
		if declared[path] {
			x.merged[path] = append(x.merged[path], c)
			x.isMerged[c] = true
		}
	}
	return x
}

// swiftExtendedType renders an extension's type as a sym path:
// `Greeter.Config` stays dotted, generic arguments come off.
func swiftExtendedType(ext *sitter.Node, content []byte) string {
	ut := firstFieldChildOfType(ext, "name", "user_type")
	if ut == nil {
		return ""
	}
	var segs []string
	for i := range int(ut.NamedChildCount()) {
		if c := ut.NamedChild(i); c.Type() == "type_identifier" {
			segs = append(segs, c.Content(content))
		}
	}
	return strings.Join(segs, ".")
}

// role overrides classify for an extension: skipped where it stands when
// merged into its type, walked through otherwise.
func (x *swiftExtensions) role(n *sitter.Node, r symRole) symRole {
	if x == nil {
		return r
	}
	if _, ok := x.of[n]; !ok {
		return r
	}
	if x.isMerged[n] {
		return roleSkip
	}
	return roleContainer
}

// override is the parent path for a member of an extension that was not
// merged: the extended type.
func (x *swiftExtensions) override(member *sitter.Node) string {
	if x == nil {
		return ""
	}
	for p := member.Parent(); p != nil; p = p.Parent() {
		if path, ok := x.of[p]; ok {
			if x.isMerged[p] {
				return ""
			}
			return path
		}
		switch t := p.Type(); {
		case swiftTypeBodies[t], t == "property_declaration", t == "enum_entry", t == "ERROR":
			continue
		}
		return ""
	}
	return ""
}
