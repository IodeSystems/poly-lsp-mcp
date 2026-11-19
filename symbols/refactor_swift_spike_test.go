package symbols

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sitter "github.com/iodesystems/poly-lsp-mcp/internal/ts"
)

// spikeSwift parses src with the Swift grammar and returns the root node
// and the source, so each subtest can dump the tree and make assertions.
func spikeSwift(t *testing.T, src string) (*sitter.Node, []byte) {
	t.Helper()
	p := sitter.NewParser()
	p.SetLanguage(sitter.Swift())
	tree, err := p.ParseCtx(context.Background(), nil, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tree.Close)
	return tree.RootNode(), []byte(src)
}

// dumpSpike renders a node subtree: indent, field, type, named, range, text.
func dumpSpike(n *sitter.Node, src []byte, depth int) string {
	var sb strings.Builder
	var walk func(n *sitter.Node, depth int)
	walk = func(n *sitter.Node, depth int) {
		field := ""
		if p := n.Parent(); p != nil {
			for i := range int(p.ChildCount()) {
				if p.Child(i) == n {
					if f := p.FieldNameForChild(i); f != "" {
						field = f + ": "
					}
				}
			}
		}
		text := ""
		if n.ChildCount() == 0 {
			text = fmt.Sprintf(" %q", n.Content(src))
		}
		fmt.Fprintf(&sb, "%s%s%s [%d-%d]%s\n",
			strings.Repeat("  ", depth), field, n.Type(), n.StartByte(), n.EndByte(), text)
		for i := range int(n.ChildCount()) {
			walk(n.Child(i), depth+1)
		}
	}
	walk(n, 0)
	return sb.String()
}

// findFirst returns the first descendant of n (at any depth) whose Type() is
// typ, or nil.
func findFirst(n *sitter.Node, typ string) *sitter.Node {
	if n.Type() == typ {
		return n
	}
	for i := range int(n.ChildCount()) {
		if c := findFirst(n.Child(i), typ); c != nil {
			return c
		}
	}
	return nil
}

// findFirstOfTypeInChildren returns the first DIRECT child of n whose Type()
// is typ, or nil.
func findFirstOfTypeInChildren(n *sitter.Node, typ string) *sitter.Node {
	for i := range int(n.ChildCount()) {
		c := n.Child(i)
		if c.Type() == typ {
			return c
		}
	}
	return nil
}

// TestSwiftSpikeVariadicParam pins the shape of a variadic parameter in a
// function declaration. The key question: is `...` a named child, an unnamed
// token, or part of the type node?
func TestSwiftSpikeVariadicParam(t *testing.T) {
	src := `func join(parts: String...) -> String { "" }`
	root, b := spikeSwift(t, src)

	// Print the full tree for the record.
	t.Log(dumpSpike(root, b, 0))

	fn := findFirst(root, "function_declaration")
	if fn == nil {
		t.Fatal("no function_declaration found")
	}

	// Find the parameter node.
	param := findFirst(fn, "parameter")
	if param == nil {
		t.Fatal("no parameter node found")
	}

	// The parameter should have a name, a type, and possibly a `...` marker.
	// Walk its children to enumerate what's there.
	var types []string
	for i := range int(param.ChildCount()) {
		types = append(types, param.Child(i).Type())
	}
	t.Logf("parameter children types: %v", types)

	// Check: is there a child of type "variadic_parameter" or a `...` token?
	hasVarToken := false
	for i := range int(param.ChildCount()) {
		c := param.Child(i)
		if c.Type() == "..." || c.Content(b) == "..." {
			hasVarToken = true
		}
	}
	t.Logf("has `...` token as direct child of parameter: %v", hasVarToken)

	// The type node: does it include the `...`?
	typeNode := param.ChildByFieldName("name")
	if typeNode != nil {
		// In the sample, the field is "name" for the type.
		// Check its content.
		t.Logf("type node content: %q", typeNode.Content(b))
	}
}

// TestSwiftSpikeClosureAtCallSite pins what a trailing-closure call looks
// like in the tree. The question: are closure arguments named children of
// call_suffix, and would they interfere with a call-site rewrite?
func TestSwiftSpikeClosureAtCallSite(t *testing.T) {
	// A call with a trailing closure.
	src := `let x = items.map { $0 * 2 }`
	root, b := spikeSwift(t, src)
	t.Log(dumpSpike(root, b, 0))

	// A call with both parens and a trailing closure.
	src2 := `let y = items.map(1) { $0 * 2 }`
	root2, b2 := spikeSwift(t, src2)
	t.Log(dumpSpike(root2, b2, 0))

	// Find the call_expression in the first case.
	call := findFirst(root, "call_expression")
	if call == nil {
		t.Fatal("no call_expression in trailing-closure call")
	}
	// Enumerate named children of call_expression.
	var named []string
	for i := range int(call.NamedChildCount()) {
		named = append(named, call.NamedChild(i).Type())
	}
	t.Logf("trailing-closure call named children: %v", named)

	// Find the call_suffix.
	suffix := findFirstOfTypeInChildren(call, "call_suffix")
	if suffix != nil {
		var suffixChildren []string
		for i := range int(suffix.ChildCount()) {
			suffixChildren = append(suffixChildren, suffix.Child(i).Type())
		}
		t.Logf("call_suffix children: %v", suffixChildren)
	}
}

// TestSwiftSpikeProtocolFunctionSemicolon pins the shape of a protocol
// function declaration that ends with a semicolon (no body). The question:
// does it have a body field, and where does the declaration end?
func TestSwiftSpikeProtocolFunctionSemicolon(t *testing.T) {
	src := `protocol P { func f(x: Int) -> Bool; func g(y: Int) }`
	root, b := spikeSwift(t, src)
	t.Log(dumpSpike(root, b, 0))

	// Find the first protocol_function_declaration.
	pfn := findFirst(root, "protocol_function_declaration")
	if pfn == nil {
		t.Fatal("no protocol_function_declaration found")
	}

	// Does it have a body field?
	body := pfn.ChildByFieldName("body")
	t.Logf("protocol_function_declaration has body field: %v", body != nil)

	// Find the semicolon after it.
	// The declaration should end right before the semicolon.
	lastChild := pfn.Child(int(pfn.ChildCount()) - 1)
	t.Logf("last child of protocol_function_declaration: type=%q content=%q",
		lastChild.Type(), lastChild.Content(b))

	// Check: the semicolon is a SIBLING of the declaration, not a child.
	parent := pfn.Parent()
	if parent != nil {
		for i := range int(parent.ChildCount()) {
			c := parent.Child(i)
			if c.EndByte() > pfn.EndByte() && c.Type() == ";" {
				t.Log("semicolon is a sibling after the declaration, not a child")
			}
		}
	}
}

// TestSwiftSpikeDefaultedParamOmitted pins what a call looks like when a
// defaulted parameter is omitted. This confirms that a call site with fewer
// arguments than the declaration is legal and parseable.
func TestSwiftSpikeDefaultedParamOmitted(t *testing.T) {
	src := `func greet(who: String, times: Int = 1) { }\nlet x = greet("hi")`
	root, b := spikeSwift(t, src)
	t.Log(dumpSpike(root, b, 0))

	// Find the call_expression for greet("hi").
	var calls []*sitter.Node
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n.Type() == "call_expression" {
			calls = append(calls, n)
		}
		for i := range int(n.ChildCount()) {
			walk(n.Child(i))
		}
	}
	walk(root)
	if len(calls) == 0 {
		t.Fatal("no call_expression found")
	}
	// The call is the last one (the declaration's body has none).
	call := calls[len(calls)-1]
	t.Logf("call_expression: %q", call.Content(b))

	// Count value_arguments.
	suffix := findFirstOfTypeInChildren(call, "call_suffix")
	if suffix == nil {
		t.Fatal("no call_suffix")
	}
	args := findFirstOfTypeInChildren(suffix, "value_arguments")
	if args == nil {
		t.Fatal("no value_arguments")
	}
	var argCount int
	for i := range int(args.ChildCount()) {
		if args.Child(i).Type() == "value_argument" {
			argCount++
		}
	}
	t.Logf("value_argument count in call: %d (expect 1, the defaulted param is omitted)", argCount)
}

// TestSwiftSpikeCallSideLabel pins the exact shape of a labeled call
// argument: the value_argument_label node's text (with/without trailing
// colon), and the field structure.
func TestSwiftSpikeCallSideLabel(t *testing.T) {
	src := `greet("x", times: 2)`
	root, b := spikeSwift(t, src)
	t.Log(dumpSpike(root, b, 0))

	call := findFirst(root, "call_expression")
	if call == nil {
		t.Fatal("no call_expression")
	}
	suffix := findFirstOfTypeInChildren(call, "call_suffix")
	if suffix == nil {
		t.Fatal("no call_suffix")
	}
	args := findFirstOfTypeInChildren(suffix, "value_arguments")
	if args == nil {
		t.Fatal("no value_arguments")
	}
	// Find the labeled argument.
	for i := range int(args.ChildCount()) {
		c := args.Child(i)
		if c.Type() != "value_argument" {
			continue
		}
		label := c.ChildByFieldName("name")
		if label == nil {
			continue
		}
		t.Logf("value_argument_label type: %q, content: %q", label.Type(), label.Content(b))
		// The colon should be a SIBLING of the label, not part of it.
		for j := range int(c.ChildCount()) {
			ch := c.Child(j)
			t.Logf("  child[%d]: type=%q content=%q", j, ch.Type(), ch.Content(b))
		}
	}
}

// TestSwiftSpikeUnderscoreParam pins the shape of a `_`-labeled parameter in
// a declaration: the external_name field, and how its call site looks (a bare
// value_argument with no label, by position).
func TestSwiftSpikeUnderscoreParam(t *testing.T) {
	src := `func greet(_ who: String) -> String { "" }\nlet x = greet("hi")`
	root, b := spikeSwift(t, src)
	t.Log(dumpSpike(root, b, 0))

	fn := findFirst(root, "function_declaration")
	if fn == nil {
		t.Fatal("no function_declaration")
	}
	param := findFirst(fn, "parameter")
	if param == nil {
		t.Fatal("no parameter")
	}
	external := param.ChildByFieldName("external_name")
	t.Logf("parameter external_name: present=%v content=%q", external != nil,
		func() string {
			if external == nil {
				return ""
			}
			return external.Content(b)
		}())
	var children []string
	for i := range int(param.ChildCount()) {
		children = append(children, param.Child(i).Type())
	}
	t.Logf("parameter children: %v", children)

	call := findFirst(root, "call_expression")
	if call == nil {
		t.Fatal("no call_expression")
	}
	suffix := findFirstOfTypeInChildren(call, "call_suffix")
	args := findFirstOfTypeInChildren(suffix, "value_arguments")
	for i := range int(args.ChildCount()) {
		c := args.Child(i)
		if c.Type() != "value_argument" {
			continue
		}
		t.Logf("call arg has label field: %v", c.ChildByFieldName("name") != nil)
	}
}

// TestSwiftSpikeUnlabeledCallArg pins what a bare (unlabeled) call argument
// looks like — no value_argument_label child.
func TestSwiftSpikeUnlabeledCallArg(t *testing.T) {
	src := `f(1, 2)`
	root, b := spikeSwift(t, src)
	t.Log(dumpSpike(root, b, 0))

	call := findFirst(root, "call_expression")
	if call == nil {
		t.Fatal("no call_expression")
	}
	suffix := findFirstOfTypeInChildren(call, "call_suffix")
	args := findFirstOfTypeInChildren(suffix, "value_arguments")
	for i := range int(args.ChildCount()) {
		c := args.Child(i)
		if c.Type() != "value_argument" {
			continue
		}
		label := c.ChildByFieldName("name")
		t.Logf("unlabeled arg: has label field: %v", label != nil)
		for j := range int(c.ChildCount()) {
			ch := c.Child(j)
			t.Logf("  child[%d]: type=%q content=%q", j, ch.Type(), ch.Content(b))
		}
	}
}
