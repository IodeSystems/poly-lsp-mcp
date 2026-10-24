package symbols

import (
	"context"

	sitter "github.com/iodesystems/poly-lsp-mcp/internal/ts"
)

// WrittenNames returns the names ASSIGNED TO in content, with how many times.
//
// This exists for one question an edit cannot otherwise answer: did this change
// remove the last thing that ever SET a name that is still read? Measured on
// the volcycle bench, four separate agent runs deleted
//
//	mVirtualControlKeyDown = down;
//
// leaving the field declared and still read by readControlKey(), so Ctrl was
// silently always-false on a device with no hardware keyboard. Every compiler
// involved was happy — the field IS still read, it is simply never true again —
// so no diagnostic gate can catch it, and a textual bench check scored it as
// passing while it shipped.
//
// It is deliberately NOT the general `.write` edge kind that icebox.md parks:
// no cross-file resolution, no aliasing, no pointer writes. It answers "is this
// occurrence an assignment target, syntactically" and nothing more, because
// that is all the guard needs and it is the part that is cheap and sound.
func WrittenNames(language string, content []byte) map[string]int {
	w, _ := NameUses(language, content)
	return w
}

// NameUses splits every identifier occurrence into WRITES (assignment targets,
// including a declaration's initializer) and READS.
//
// A DECLARATION is neither. That distinction is what keeps the lost-write guard
// quiet on a dead field: after the last write and the last reader are both
// gone, `flag` still occurs — in `type C struct{ flag bool }` — and treating
// that as a read would warn about deleting something nobody uses.
func NameUses(language string, content []byte) (writes, reads map[string]int) {
	writes, reads = map[string]int{}, map[string]int{}
	lang := LanguageByName(language)
	if lang == nil {
		return writes, reads
	}
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil || tree == nil {
		return writes, reads
	}
	defer tree.Close()

	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if isIdentifierNode(n.Type()) {
			switch {
			case isAssignmentTarget(n):
				writes[n.Content(content)]++
			case !isDeclarationName(n):
				reads[n.Content(content)]++
			}
		}
		for i := 0; i < int(n.NamedChildCount()); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(tree.RootNode())
	return writes, reads
}

// isDeclarationName reports whether n is the NAME being introduced by a
// declaration rather than a use of one. Every grammar we ship spells it as the
// parent's `name` field; go's struct fields are the exception that matters, so
// field_declaration is checked by containment.
func isDeclarationName(n *sitter.Node) bool {
	p := n.Parent()
	if p == nil {
		return false
	}
	if name := p.ChildByFieldName("name"); name != nil && name.Equal(n) {
		return true
	}
	if p.Type() == "field_declaration" || p.Type() == "variable_declarator" {
		if name := p.ChildByFieldName("name"); name == nil {
			// go: `flag bool` — the identifiers before the type are the names.
			if t := p.ChildByFieldName("type"); t != nil && n.StartByte() < t.StartByte() {
				return true
			}
		}
	}
	return false
}

func isIdentifierNode(t string) bool {
	switch t {
	case "identifier", "field_identifier", "property_identifier", "simple_identifier", "type_identifier":
		return true
	}
	return false
}

// isAssignmentTarget reports whether n sits on the LEFT of an assignment.
//
// The `left` field is what every grammar we ship names it, so the walk is
// language-agnostic: rise until some ancestor's `left` child contains us.
//
// The one refinement that matters is qualified targets. In `c.f = d` the whole
// selector is the left child, so a naive containment test reports BOTH `c` and
// `f` as written — and `c` is only read. Only the LAST segment of a qualified
// target is assigned, which is the same rule the kotlin call classifier needs
// for `a.b.c()`.
func isAssignmentTarget(n *sitter.Node) bool {
	// A declaration WITH an initializer is a write: `int f = 1` sets f, and a
	// guard that ignored it would warn about removing the only assignment to a
	// name that is still initialized. Grammars spell this name/value rather
	// than left/right (ts and java variable_declarator, go var_spec).
	if p := n.Parent(); p != nil {
		if name := p.ChildByFieldName("name"); name != nil && name.Equal(n) {
			if p.ChildByFieldName("value") != nil {
				return true
			}
		}
	}
	for cur := n; cur != nil; cur = cur.Parent() {
		parent := cur.Parent()
		if parent == nil {
			return false
		}
		left := parent.ChildByFieldName("left")
		if left == nil || !nodeContainsNode(left, n) {
			continue
		}
		// Found the assignment. If the target is qualified, only its final
		// identifier is the thing being written.
		return n.Equal(lastIdentifierIn(left))
	}
	return false
}

// lastIdentifierIn returns the deepest, right-most identifier under n — the
// segment a qualified assignment target actually writes.
func lastIdentifierIn(n *sitter.Node) *sitter.Node {
	var last *sitter.Node
	var walk func(x *sitter.Node)
	walk = func(x *sitter.Node) {
		if isIdentifierNode(x.Type()) {
			last = x
		}
		for i := 0; i < int(x.NamedChildCount()); i++ {
			walk(x.NamedChild(i))
		}
	}
	walk(n)
	return last
}
