package mcp

import (
	"reflect"
	"testing"
)

// parseOne parses a single-complex selector and returns it, failing the test
// on a parse error. These tests are about the PARSER's output — the AST it
// builds — not about evaluation, so a selector only needs to be well-formed.
func parseOne(t *testing.T, sel string) selComplex {
	t.Helper()
	list, err := parseModernSelector(sel)
	if err != nil {
		t.Fatalf("parse %q: %v", sel, err)
	}
	if len(list) != 1 {
		t.Fatalf("parse %q: expected one complex, got %d", sel, len(list))
	}
	return list[0]
}

func TestParseDeclarationBlock_Fields(t *testing.T) {
	cx := parseOne(t, "#save::in.call > * { name; path; line; signature }")
	if !cx.hasFields {
		t.Fatal("a declaration block must be recorded on the complex")
	}
	want := []string{"name", "path", "line", "signature"}
	if !reflect.DeepEqual(cx.fields, want) {
		t.Errorf("fields = %v, want %v", cx.fields, want)
	}
	// The block is terminal: it does not swallow the chain into a spurious
	// element. `#save::in.call > *` is three elements.
	if len(cx.elems) != 3 {
		t.Errorf("element count = %d, want 3 (id, ref, subject)", len(cx.elems))
	}
}

func TestParseDeclarationBlock_EdgeFieldSet(t *testing.T) {
	// An edge field set projects the reference's own fields, not a node's.
	cx := parseOne(t, "func::in.call { site; conf; kind }")
	if !cx.hasFields || !reflect.DeepEqual(cx.fields, []string{"site", "conf", "kind"}) {
		t.Errorf("edge field set = %v (has=%v), want [site conf kind]", cx.fields, cx.hasFields)
	}
}

func TestParseDeclarationBlock_NoTrailingSemicolon(t *testing.T) {
	cx := parseOne(t, "func { name; path }")
	if !reflect.DeepEqual(cx.fields, []string{"name", "path"}) {
		t.Errorf("fields = %v, want [name path] (trailing ';' optional)", cx.fields)
	}
}

func TestParseDeclarationBlock_Empty(t *testing.T) {
	// `{ }` is present but empty — distinct from no block at all.
	cx := parseOne(t, "func { }")
	if !cx.hasFields {
		t.Fatal("an empty block is still a block (hasFields)")
	}
	if len(cx.fields) != 0 {
		t.Errorf("empty block fields = %v, want none", cx.fields)
	}
}

func TestParseDeclarationBlock_AbsentByDefault(t *testing.T) {
	// A selector with no block is byte-for-byte F1: no projection, so the
	// renderer prints every field. The parser must NOT invent one.
	cx := parseOne(t, "func > argument")
	if cx.hasFields || len(cx.fields) != 0 {
		t.Errorf("a no-block selector must carry no fields; has=%v fields=%v", cx.hasFields, cx.fields)
	}
}

func TestParseDeclarationBlock_RejectsBadShape(t *testing.T) {
	for _, sel := range []string{
		"func { name path }",      // missing ';'
		"func { name; } extra",    // something after the terminal block
		"func { name; [path=x] }", // a field is an identifier, not an attr
		"func { name; }",          // fine on its own — control: see below
	} {
		// The last one is legal; parse the bad ones and require an error.
		if sel == "func { name; }" {
			if _, err := parseModernSelector(sel); err != nil {
				t.Errorf("control %q should parse, got: %v", sel, err)
			}
			continue
		}
		if _, err := parseModernSelector(sel); err == nil {
			t.Errorf("%q must be rejected", sel)
		}
	}
}

func TestParsePathCapture_Suffix(t *testing.T) {
	cx := parseOne(t, "#main::out.call{1,}:path > *:empty(::out.call) { path; length }")
	// The ref element is the one that captures the path.
	var ref selElem
	found := false
	for _, el := range cx.elems {
		if el.comp != nil && el.comp.isRef {
			ref = el
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no ref element in the chain")
	}
	if !ref.pathCapture {
		t.Error("{1,}:path must set pathCapture on the ref element")
	}
	// The range still parses: {1,} = min 1, unbounded.
	if ref.min != 1 || ref.max != -1 {
		t.Errorf("ref range = {%d,%d}, want {1,-1}", ref.min, ref.max)
	}
	// The trailing block is recorded too.
	if !cx.hasFields || !reflect.DeepEqual(cx.fields, []string{"path", "length"}) {
		t.Errorf("fields = %v (has=%v), want [path length]", cx.fields, cx.hasFields)
	}
}

func TestParsePathCapture_BoundedRange(t *testing.T) {
	cx := parseOne(t, "func::out.call{1,3}:path")
	var ref selElem
	for _, el := range cx.elems {
		if el.comp != nil && el.comp.isRef {
			ref = el
			break
		}
	}
	if !ref.pathCapture {
		t.Error("{1,3}:path must set pathCapture")
	}
	if ref.min != 1 || ref.max != 3 {
		t.Errorf("ref range = {%d,%d}, want {1,3}", ref.min, ref.max)
	}
}

func TestParsePathCapture_NotSetWithoutSuffix(t *testing.T) {
	// A bare ref (no :path) does NOT capture: `::out.call` is the F1 edge.
	cx := parseOne(t, "func::out.call{1,}")
	for _, el := range cx.elems {
		if el.comp != nil && el.comp.isRef && el.pathCapture {
			t.Error("a {1,} ref without :path must not set pathCapture")
		}
	}
}

func TestParsePathCapture_SuffixDoesNotBreakChainedPseudo(t *testing.T) {
	// `X::out::grep(…)` chains a pseudo after the ref; the ref has no range,
	// so there is no :path, and the chained pseudo must still be its own
	// element.
	list, err := parseModernSelector(`func::out.call::grep('x')`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// ref element + frag element under the host = 3 elements.
	cx := list[0]
	var frag bool
	for _, el := range cx.elems {
		if el.comp == nil {
			continue
		}
		if el.comp.isFrag {
			frag = true
		}
		if el.comp.isRef && el.pathCapture {
			t.Error("a ref with no range must not be path-capturing")
		}
	}
	if !frag {
		t.Error("the ::grep fragment must be parsed as its own element")
	}
}

// runPathWalk evaluates a :path selector on a real workspace tree and
// returns the engine's recorded chains.
func runPathWalk(t *testing.T, files map[string]string, sel string) (map[*treeNode]*walkPath, []*treeNode) {
	t.Helper()
	dir := writeTree(t, files)
	s := startSession(t, dir)
	defer s.close()
	s.request("initialize", map[string]any{})
	e, err := s.srv.buildTreeFor(sel)
	if err != nil {
		t.Fatal(err)
	}
	list, err := parseModernSelector(sel)
	if err != nil {
		t.Fatalf("parse %q: %v", sel, err)
	}
	e.evaluate(list)
	return e.pathChain, e.evaluate(list)
}

// pathsSimple reports whether no chain repeats a node.
func pathsSimple(chains map[*treeNode]*walkPath) (bad string) {
	for r, p := range chains {
		seen := map[*treeNode]bool{}
		for _, rr := range p.Refs {
			if seen[rr] {
				return r.leaf
			}
			seen[rr] = true
		}
	}
	return ""
}

func TestEvalPathCapture_ChainSimpleAndBounded(t *testing.T) {
	// A→B→C→D: the unbounded walk records D's route as the three edges
	// A→B, B→C, C→D — length 3, no node repeated.
	files := map[string]string{
		"app.go": "package p\n\nfunc A() { B() }\nfunc B() { C() }\nfunc C() { D() }\nfunc D() {}\n",
	}
	chains, _ := runPathWalk(t, files, "#A::out.call{1,}:path")
	if chains == nil {
		t.Fatal("a :path walk must record chains")
	}
	var dChain *walkPath
	for r, p := range chains {
		if r.leaf == "D" {
			dChain = p
		}
	}
	if dChain == nil {
		t.Fatal("no chain to D")
	}
	if len(dChain.Refs) != 3 {
		t.Fatalf("D's chain has %d edges, want 3 (A→B, B→C, C→D)", len(dChain.Refs))
	}
	if got := pathsSimple(chains); got != "" {
		t.Errorf("chain to %s repeats a node — paths must be simple", got)
	}
	// The chain's edges are the WALK's edges, in order: each edge's far
	// end is the next edge's host.
	for i, r := range dChain.Refs[:len(dChain.Refs)-1] {
		if len(r.refFar) != 1 || r.refFar[0] != dChain.Refs[i+1].parent {
			t.Errorf("chain edge %d's far end is not the next edge's host", i)
		}
	}
	// Bounded: {1,2} caps D's route at two edges, and D is not in the
	// bounded match set at all.
	chains, rows := runPathWalk(t, files, "#A::out.call{1,2}:path")
	var dRow bool
	for _, n := range rows {
		if n.leaf == "D" {
			dRow = true
		}
	}
	if dRow {
		t.Error("D is 3 hops out; a {1,2} walk must not reach it")
	}
	for r, p := range chains {
		if len(p.Refs) > 2 {
			t.Errorf("chain to %s has %d edges; the bound is 2", r.leaf, len(p.Refs))
		}
	}
	if got := pathsSimple(chains); got != "" {
		t.Errorf("bounded chain to %s repeats a node", got)
	}
}

func TestEvalPathCapture_CycleSafe(t *testing.T) {
	// E↔F is a 2-cycle: an unbounded walk must TERMINATE and every
	// recorded chain must be simple (E→F and F→E, never E→F→E).
	files := map[string]string{
		"app.go": "package p\n\nfunc E() { F() }\nfunc F() { E() }\n",
	}
	chains, _ := runPathWalk(t, files, "func::out.call{1,}:path")
	if chains == nil {
		t.Fatal("a :path walk must record chains")
	}
	if got := pathsSimple(chains); got != "" {
		t.Fatalf("cycle leaked into a chain at %s — the walk is not cycle-safe", got)
	}
	// Both directions exist, each exactly one edge long: the walk followed
	// the cycle to the repeated node and stopped.
	for _, leaf := range []string{"E", "F"} {
		var found bool
		for r, p := range chains {
			if r.leaf == leaf && len(p.Refs) == 1 {
				found = true
			}
		}
		if !found {
			t.Errorf("no one-edge chain to %s (want both cycle directions)", leaf)
		}
	}
}

func TestEvalPathCapture_AbsentWithoutSuffix(t *testing.T) {
	// A {1,} walk WITHOUT :path is the F1 edge walk: no chains are
	// recorded, so the renderer cannot invent a path for it.
	files := map[string]string{
		"app.go": "package p\n\nfunc A() { B() }\nfunc B() {}\n",
	}
	chains, _ := runPathWalk(t, files, "func::out.call{1,}")
	if chains != nil {
		t.Errorf("a walk without :path must not record chains; got %d", len(chains))
	}
}
