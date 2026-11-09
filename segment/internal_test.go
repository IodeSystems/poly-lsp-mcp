package segment

import "testing"

func TestClassifyKotlinWalksThroughParseErrors(t *testing.T) {
	// A single unparsable statement deep inside a method re-labels the
	// whole enclosing class_body as ERROR, and the class then keeps its
	// name and loses EVERY member — while the ERROR node still holds all
	// of them as recovered children. Walking through ERROR reattaches
	// them.
	//
	// This asserts the routing decision, not an end-to-end recovery: the
	// failure was found on a live 504-file Kotlin repo
	// (E2ETestPlatform.kt, 22 -> 44 symbols with this in place) and does
	// NOT reduce to a minimal fixture — every candidate construct
	// extracted from that file parses cleanly on its own, so the trigger
	// needs whole-file context that cannot be checked in here.
	if got := classifyKotlin("ERROR", "class_declaration"); got != roleContainer {
		t.Errorf("classifyKotlin(ERROR) = %v, want roleContainer", got)
	}
	// The cost of that choice, stated so it is not mistaken for a bug:
	// when the broken region sits inside a method, that method's locals
	// flatten into the same ERROR and surface as fields of the class.
}

func TestTypeSegmentStripsGenericsAndArrays(t *testing.T) {
	// A `.return` node answers to a bare NAME. Measured on a 492-file
	// Java tree: 531 of 10,738 return nodes carried `<...>` or `[]` in
	// their path segment with an EMPTY alias, so `return#Field` could
	// not match a `Field<String>` result.
	cases := []struct{ in, seg, alias string }{
		{"String", "String", ""},
		{"Field<String>", "Field", "Field<String>"},
		{"Field<byte[]>", "Field", "Field<byte[]>"},
		{"Long[]", "Long", "Long[]"},
		{"String[][]", "String", "String[][]"},
		{"java.util.Map.Entry", "Entry", "java.util.Map.Entry"},
		{"kotlin.text.Regex?", "Regex", "kotlin.text.Regex?"},
		{"List<Map<String, Int>>", "List", "List<Map<String, Int>>"},
		{"RestResponse<DataSetResponse<AccountView>>", "RestResponse", "RestResponse<DataSetResponse<AccountView>>"},
		// A union has no single leaf to answer to; inventing one would
		// misstate which type the callable returns.
		{"string | null", "string | null", ""},
	}
	for _, c := range cases {
		seg, alias := typeSegment(c.in)
		if seg != c.seg || alias != c.alias {
			t.Errorf("typeSegment(%q) = (%q, %q), want (%q, %q)", c.in, seg, alias, c.seg, c.alias)
		}
	}
}

func TestGoTypeSegmentKeepsDecorationButNotFalseLeaves(t *testing.T) {
	// Decoration is KEPT — `*Config` and `[]Schema` are how Go code names
	// the result, and existing sym paths depend on it.
	//
	// What is fixed is the blind dot-split reaching INSIDE a composite and
	// reporting its last identifier as the leaf: sitesByFile returns
	// map[string][]symbols.InvSite and used to answer to `return#InvSite`,
	// a type it does not return.
	cases := []struct{ in, seg, alias string }{
		{"error", "error", ""},
		{"*Config", "*Config", ""},
		{"[]Schema", "[]Schema", ""},
		{"io.Writer", "Writer", "io.Writer"},
		{"*sitter.Node", "Node", "*sitter.Node"},
		// Composites claim nothing.
		{"map[string][]symbols.InvSite", "map[string][]symbols.InvSite", ""},
		{"map[string]int", "map[string]int", ""},
		{"[]map[string]symbols.Hit", "[]map[string]symbols.Hit", ""},
		{"chan Result", "chan Result", ""},
		{"func() error", "func() error", ""},
		{"*[2]int", "*[2]int", ""},
		{"interface{}", "interface{}", ""},
	}
	for _, c := range cases {
		seg, alias := goTypeSegment(c.in)
		if seg != c.seg || alias != c.alias {
			t.Errorf("goTypeSegment(%q) = (%q, %q), want (%q, %q)", c.in, seg, alias, c.seg, c.alias)
		}
	}
}

func TestClassifySwiftWalksThroughParseErrors(t *testing.T) {
	if got := classifySwift("ERROR", "class_body"); got != roleContainer {
		t.Errorf("ERROR role = %v, want container", got)
	}
}
