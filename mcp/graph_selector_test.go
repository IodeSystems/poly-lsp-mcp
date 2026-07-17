package mcp

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The graph half of the selector language: reference EDGES reified as
// ::in/::out pseudo-element nodes, kind classification, gated crossing,
// {m,n} repetition, :parents as the upstream inverse, position claims,
// language classes, and :first/:last. The pure-containment half stays
// CSS and is covered by modern_test.go.
//
// The polyglot call graph under test:
//
//	go:  A ──▶ B ──▶ C ◀── h (a var: an UNCLASSIFIED ref, not a call)
//	     Y ◀──▶ X ──▶ C          (X/Y are a real cycle)
//	     UsesT(t T)              (T used AS A TYPE)
//	ts:  useHelper ──▶ tsHelper  (plus an IMPORT of tsHelper)
//
const graphGoSrc = `package lib

type T struct{}

func C() {}

func B(bArg int) { C() }

func A() { B(1) }

func X(xArg string) { Y(); C() }

func Y() { X() }

func UsesT(t T) {}

var h = C
`

const graphUtilTS = `export function tsHelper(a: string) { return a; }
`

const graphAppTS = `import {tsHelper} from './util';
export function useHelper() { return tsHelper('x'); }
`

func startGraph(t *testing.T) *mcpSession {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module lib\ngo 1.26\n")
	write("main.go", graphGoSrc)
	write("web/util.ts", graphUtilTS)
	write("web/app.ts", graphAppTS)
	s := startSessionFull(t, dir, nil, nil)
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	return s
}

func wantNodes(t *testing.T, q queryResult, want ...string) {
	t.Helper()
	got := map[string]bool{}
	for _, n := range nodes(q) {
		got[n] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %q; got %v", w, nodes(q))
		}
	}
	if len(got) != len(want) {
		t.Errorf("want exactly %v; got %v", want, nodes(q))
	}
}

// ---------------------------------------------------------- edge nodes

func TestEdgeNodesAndKinds(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// C is pointed at by B (call), X (call) and the var h — an
	// unclassified reference, NOT a call. The kind class separates them.
	q := query(t, s, map[string]any{"selector": `#'main.go#C'::in`})
	if q.TotalMatches != 3 {
		t.Errorf("C has 3 incoming refs; got %v", nodes(q))
	}
	q = query(t, s, map[string]any{"selector": `#'main.go#C'::in.call`})
	if q.TotalMatches != 2 {
		t.Errorf("only B and X CALL C — h is a plain ref; got %v", nodes(q))
	}
	// The edge rows speak the grammar: type is the selector spelling.
	for _, m := range q.Matches {
		if m.Class != "::in.call" {
			t.Errorf("edge row type = %q, want ::in.call", m.Class)
		}
	}

	// T is used AS A TYPE; tsHelper is imported AND called.
	if q := query(t, s, map[string]any{"selector": `#'main.go#T'::in.type`}); q.TotalMatches != 1 {
		t.Errorf("T has one type-use; got %v", nodes(q))
	}
	if q := query(t, s, map[string]any{"selector": `#'web/util.ts#tsHelper'::in.import`}); q.TotalMatches != 1 {
		t.Errorf("tsHelper is imported once; got %v", nodes(q))
	}
	if q := query(t, s, map[string]any{"selector": `#'web/util.ts#tsHelper'::in.call`}); q.TotalMatches != 1 {
		t.Errorf("tsHelper is called once; got %v", nodes(q))
	}
}

func TestStarNeverMatchesEdges(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// B's only child is its argument — the pseudo-element contract keeps
	// edges out of `*` and containment queries file-local.
	q := query(t, s, map[string]any{"selector": `#'main.go#B' > *`})
	wantNodes(t, q, "main.go#B.bArg")
	q = query(t, s, map[string]any{"selector": `#'main.go' func`, "limit": 50})
	for _, n := range nodes(q) {
		if !strings.HasPrefix(n, "main.go#") {
			t.Errorf("containment leaked through an edge: %q", n)
		}
	}
}

func TestEdgeCrossing(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// The far end is the edge's child: cross with '>'.
	q := query(t, s, map[string]any{"selector": `#'main.go#A'::out.call > *`})
	wantNodes(t, q, "main.go#B")
	q = query(t, s, map[string]any{"selector": `#'main.go#X'::out.call > *`})
	wantNodes(t, q, "main.go#Y", "main.go#C")
	// The var h points at C with an unclassified edge.
	q = query(t, s, map[string]any{"selector": `#'main.go#h'::out > *`})
	wantNodes(t, q, "main.go#C")
	q = query(t, s, map[string]any{"selector": `#'main.go#h'::out.call > *`})
	wantNodes(t, q)
}

func TestEdgeHopsAreTransitive(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// {1,} crosses call edges to a fixpoint — the X↔Y cycle terminates.
	// The far ends of ALL crossed edges are C's transitive callers.
	q := query(t, s, map[string]any{"selector": `#'main.go#C'::in.call{1,} > *`, "limit": 50})
	wantNodes(t, q, "main.go#B", "main.go#X", "main.go#A", "main.go#Y")

	// An exact window: callers-of-callers only.
	q = query(t, s, map[string]any{"selector": `#'main.go#C'::in.call{2,2} > *`})
	wantNodes(t, q, "main.go#A", "main.go#Y")

	// A node can reach itself through a cycle.
	q = query(t, s, map[string]any{"selector": `#'main.go#X'::in.call{1,} > *`})
	wantNodes(t, q, "main.go#Y", "main.go#X")
}

func TestPositionClaims(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// Dead code: nothing points here. (useHelper and UsesT are also
	// uncalled — the graph is polyglot.)
	q := query(t, s, map[string]any{"selector": `func:where(::in:empty)`, "limit": 50})
	wantNodes(t, q, "main.go#A", "main.go#UsesT", "web/app.ts#useHelper")

	// Leaves: funcs that call nothing.
	q = query(t, s, map[string]any{"selector": `func:where(::out.call:empty)`, "limit": 50})
	wantNodes(t, q, "main.go#C", "main.go#UsesT", "web/util.ts#tsHelper")

	// :any is the complement, and the explicit & spelling is identical.
	for _, sel := range []string{`func:where(::in.call:any)`, `func:where(&::in.call:any)`} {
		q = query(t, s, map[string]any{"selector": sel, "limit": 50})
		wantNodes(t, q, "main.go#C", "main.go#B", "main.go#X", "main.go#Y", "web/util.ts#tsHelper")
	}

	// ∀ at a position: C's incoming edges are NOT all calls (h), B's are.
	q = query(t, s, map[string]any{"selector": `func:where(::in.call:all)`, "limit": 50})
	if hasNode(q, "main.go#C") {
		t.Errorf("h's ref to C is not a call — ∀ must fail for C; got %v", nodes(q))
	}
	if !hasNode(q, "main.go#B") {
		t.Errorf("B's every incoming ref is a call — ∀ must hold; got %v", nodes(q))
	}
}

// ---------------------------------------------------------- :parents

func TestParentsIsUpstream(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// Only the workspace root has NOTHING upstream.
	q := query(t, s, map[string]any{"selector": `*:parents:empty`})
	if q.TotalMatches != 1 || q.Matches[0].Class != "project" {
		t.Errorf("*:parents:empty must be exactly the root; got %v", nodes(q))
	}

	// Upstream funcs of C = its transitive callers (the var h and the
	// containing file are upstream too, but they are not funcs).
	q = query(t, s, map[string]any{"selector": `#'main.go#C':parents(func)`, "limit": 50})
	wantNodes(t, q, "main.go#B", "main.go#X", "main.go#A", "main.go#Y")

	// Containment is upstream as well.
	q = query(t, s, map[string]any{"selector": `#'main.go#C':parents(file)`})
	wantNodes(t, q, "main.go")

	// Multi-element inner: the result is the ROOT of the matched path —
	// the dir whose func is upstream of tsHelper (useHelper, in web/).
	q = query(t, s, map[string]any{"selector": `#'web/util.ts#tsHelper':parents(dir func)`})
	wantNodes(t, q, "web")
}

// ------------------------------------------------- language + ordering

func TestLanguageClasses(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	q := query(t, s, map[string]any{"selector": `func.ts`, "limit": 50})
	wantNodes(t, q, "web/util.ts#tsHelper", "web/app.ts#useHelper")
	q = query(t, s, map[string]any{"selector": `file.go`})
	wantNodes(t, q, "main.go")
}

func TestFirstLastArePerAnchor(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// Per anchor: each file's FIRST func.
	q := query(t, s, map[string]any{"selector": `file > func:first`, "limit": 50})
	wantNodes(t, q, "main.go#C", "web/util.ts#tsHelper", "web/app.ts#useHelper")

	// One anchor (the root): jQuery behavior — the last func overall.
	q = query(t, s, map[string]any{"selector": `func:last`})
	wantNodes(t, q, "web/util.ts#tsHelper")
}

// ------------------------------------------------- repetition + groups

func TestRepetitionIsChildJoined(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// *{2} = exactly two child steps from the root.
	q := query(t, s, map[string]any{"selector": `:root > *{2}`, "limit": 50})
	if !hasNode(q, "main.go#C") || !hasNode(q, "web/util.ts") {
		t.Errorf("depth-2 nodes missing; got %v", nodes(q))
	}
	if hasNode(q, "main.go") {
		t.Errorf("main.go is depth 1, not 2; got %v", nodes(q))
	}

	// {0,1}: the skip path keeps the previous tip — the element vanishes.
	q = query(t, s, map[string]any{"selector": `#'main.go' > *{0,1}`, "limit": 50})
	if !hasNode(q, "main.go") || !hasNode(q, "main.go#C") {
		t.Errorf("{0,1} must include the anchor (skip) and its children; got %v", nodes(q))
	}

	// A group repeats as a unit.
	q = query(t, s, map[string]any{"selector": `(dir file){1}`, "limit": 50})
	wantNodes(t, q, "web/util.ts", "web/app.ts")
}

// --------------------------------------------------- site addressing

func TestEdgeAddressesAreSites(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	q := query(t, s, map[string]any{"selector": `#'main.go#A'::out.call`})
	if q.TotalMatches != 1 {
		t.Fatalf("A makes one call; got %v", nodes(q))
	}
	addr := q.Matches[0].Node
	if !regexp.MustCompile(`^main\.go@\d+$`).MatchString(addr) {
		t.Fatalf("edge address should be file@line, got %q", addr)
	}
	// Reading the edge reads the call site.
	r := s.callTool("node_read", map[string]any{"node": addr})
	if r.IsError {
		t.Fatalf("node_read %s errored: %s", addr, r.Content[0].Text)
	}
	if !strings.Contains(r.Content[0].Text, "B(1)") {
		t.Errorf("reading %s should show the call site, got: %s", addr, r.Content[0].Text)
	}
}

// --------------------------------------------------------- guided errors

func TestRetiredSpellingsNameTheirReplacement(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	for sel, want := range map[string]string{
		`file:has(func)`:            ":any",
		`func:has_parent(#'a.go')`:  `#'a.ts' func`,
		`*:references(#'C')`:        "::out",
		`*:depth(0,0)`:              "{m,n}",
		`func::ref.out`:             "::in",
		`func::before`:              "::out",
		`func:empty`:                ":where",
		`&:parents:empty`:           ":where",
		`func:where(> &)`:           "contradicts",
		`func:where(::in:empty *)`:  "follow",
		`::in{0,}`:                  "hops start at 1",
		`func::out.ptr`:             ".call/.type/.import",
		`file.rust`:                 "language",
	} {
		got := queryErr(t, s, map[string]any{"selector": sel})
		if !strings.Contains(got, want) {
			t.Errorf("%s: error should name %q, got: %s", sel, want, got)
		}
	}
}
