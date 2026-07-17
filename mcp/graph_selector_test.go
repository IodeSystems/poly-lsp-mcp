package mcp

import (
	"fmt"
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

// --------------------------------------------------------- import edges

const humaMainSrc = `package main

import (
	"github.com/danielgtaylor/huma/v2"
)

func main() {
	var api huma.API
	huma.Register(api, huma.Operation{OperationID: "get-user"}, GetUser)
	huma.Get(api, "/health", HealthCheck)
}

func GetUser(x int) {}

func HealthCheck(x int) {}
`

const humaAdminSrc = `package main

import (
	h "github.com/danielgtaylor/huma/v2"
)

func registerAdmin() {
	h.Post(nil, "/admin/reset", ResetAll)
}

func ResetAll(x int) {}
`

// The acceptance exercise this slice was built against: find every
// endpoint of a generic huma app. External packages have no decl in the
// workspace, so qualified references resolve to the file's IMPORT node
// — which must therefore carry the PACKAGE name (vN skipped, alias
// honored), and must only ever be the far end of its OWN file's sites.
func TestImportEdgesFindHumaEndpoints(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module app\ngo 1.26\n")
	write("main.go", humaMainSrc)
	write("admin.go", humaAdminSrc)
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// The import node is named for the PACKAGE: huma, not v2 — and the
	// alias wins when one is written.
	q := query(t, s, map[string]any{"selector": `import`, "limit": 10})
	wantNodes(t, q, "main.go#huma", "admin.go#h")

	// THE endpoint query: every huma registration site, workspace-wide.
	q = query(t, s, map[string]any{"selector": `import[name*=hum], import#h`, "limit": 10})
	if q.TotalMatches != 2 {
		t.Fatalf("expected both imports; got %v", nodes(q))
	}
	q = query(t, s, map[string]any{"selector": `#'main.go#huma'::in.call`, "limit": 10})
	if q.TotalMatches != 2 {
		t.Errorf("main.go registers 2 endpoints via huma; got %v", nodes(q))
	}
	// The alias resolves the same way, and edges are FILE-scoped: h's
	// import sees only admin.go's site, huma's only main.go's.
	q = query(t, s, map[string]any{"selector": `#'admin.go#h'::in.call`, "limit": 10})
	if q.TotalMatches != 1 {
		t.Errorf("admin.go registers 1 endpoint via h; got %v", nodes(q))
	}
	for _, m := range q.Matches {
		if !strings.HasPrefix(m.Node, "admin.go@") {
			t.Errorf("an import's edges must come from its own file; got %v", m.Node)
		}
	}

	// ::grep over the edge SITES narrows to the registration verbs and
	// carries the routes — the whole exercise in one selector.
	r := s.callTool("node_query", map[string]any{
		"selector": `import::in.call::grep('-E (Register|Get|Post)\(')`,
		"limit":    10,
	})
	if r.IsError {
		t.Fatalf("errored: %s", r.Content[0].Text)
	}
	text := r.Content[0].Text
	for _, want := range []string{"get-user", "/health", "/admin/reset"} {
		if !strings.Contains(text, want) {
			t.Errorf("endpoint sweep missing %q in: %s", want, text)
		}
	}

	// And the handlers, graph-native: funcs whose incoming reference
	// SITE is a huma line (:contains on an edge tests the site text).
	q = query(t, s, map[string]any{"selector": `func:where(::in:contains('-E (huma|h)\.'))`, "limit": 10})
	wantNodes(t, q, "main.go#GetUser", "main.go#HealthCheck", "admin.go#ResetAll")
}

// ---------------------------------------------------- :not / :is

func TestNotAndIsAreSelfTests(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// :not tests the node ITSELF (CSS-true): a leading #id is the node,
	// never a descendant.
	q := query(t, s, map[string]any{"selector": `#'main.go' > func:not(#C):not(#X)`, "limit": 50})
	wantNodes(t, q, "main.go#B", "main.go#A", "main.go#Y", "main.go#UsesT")

	// bonsai's dead-func detector, spelled: ∄ callers, minus the roots
	// (an attr exclusion and an id exclusion, chained).
	q = query(t, s, map[string]any{"selector": `func:not([name^=Use]):not(#useHelper):where(::in:empty)`, "limit": 50})
	wantNodes(t, q, "main.go#A")

	// :not of a pseudo-carrying compound ≡ the :empty claim.
	a := query(t, s, map[string]any{"selector": `func:not(:any(::out.call))`, "limit": 50})
	b := query(t, s, map[string]any{"selector": `func:where(::out.call:empty)`, "limit": 50})
	if strings.Join(nodes(a), "|") != strings.Join(nodes(b), "|") || len(nodes(a)) == 0 {
		t.Errorf(":not(:any(X)) must equal :where(X:empty); got %v vs %v", nodes(a), nodes(b))
	}

	// :is unions at the compound level, mid-chain.
	q = query(t, s, map[string]any{"selector": `#'main.go' > :is(struct, var)`})
	wantNodes(t, q, "main.go#T", "main.go#h")

	// A chained inner falls back to global membership: funcs NOT under web/.
	q = query(t, s, map[string]any{"selector": `func:not(#web *)`, "limit": 50})
	for _, n := range nodes(q) {
		if strings.HasPrefix(n, "web/") {
			t.Errorf("web funcs should be excluded; got %v", nodes(q))
		}
	}
	if !hasNode(q, "main.go#C") {
		t.Errorf("go funcs should remain; got %v", nodes(q))
	}
}

// ---------------------------------------------- colon auto-repair

func TestMissingColonsAreRepaired(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// bonsai's raw spelling shape, verbatim — has() maps to :any,
	// out.call gets its two colons, not() its one. Funcs that make no
	// calls, C excluded by id.
	q := query(t, s, map[string]any{"selector": `func:not(has(out.call)):not(#C)`, "limit": 50})
	wantNodes(t, q, "main.go#UsesT", "web/util.ts#tsHelper")

	// Each repair also works alone, and equals the canonical spelling.
	// NB: repairs are syntax-level only — CSS attachment semantics
	// survive them, so the tight forms are the ones that mean "C's own
	// edges" (X::in vs X ::in, as #a::before vs #a ::before).
	for sel, same := range map[string]string{
		`file:has(func)`:             `file:any(func)`,
		`#'main.go#C'in.call`:        `#'main.go#C'::in.call`,
		`#'main.go#C':in.call`:       `#'main.go#C'::in.call`,
		`func:where(out.call:empty)`: `func:where(::out.call:empty)`,
		`#'main.go#C':parent(func)`:  `#'main.go#C':parents(func)`,
	} {
		a := query(t, s, map[string]any{"selector": sel, "limit": 50})
		b := query(t, s, map[string]any{"selector": same, "limit": 50})
		if strings.Join(nodes(a), "|") != strings.Join(nodes(b), "|") || a.TotalMatches == 0 {
			t.Errorf("%s should repair to %s; got %v vs %v", sel, same, nodes(a), nodes(b))
		}
	}
}

// --------------------------------------------------------- work budget

// Unbounded traversals ({1,}, :parents) are termination-safe (visited
// sets), so the guard is a query-wide WORK budget, not a hop cap — a
// hop cap bounds the wrong axis (breadth is the cost) and silently
// under-reports. Tripping the budget must be LOUD: partial results,
// truncated flag, repair recipe in the note.
func TestWorkBudgetTripsLoudly(t *testing.T) {
	s := startGraph(t)
	defer s.close()
	s.srv.SetQueryWorkBudget(25)

	r := s.callTool("node_query", map[string]any{"selector": `func:where(::in:empty)`, "limit": 50})
	if r.IsError {
		t.Fatalf("a tripped budget is a partial RESULT, not an error: %s", r.Content[0].Text)
	}
	text := r.Content[0].Text
	if !strings.Contains(text, `"truncated":true`) || !strings.Contains(text, "work budget") {
		t.Errorf("tripped budget must be loudly flagged; got: %s", text)
	}

	// The default budget doesn't trip on a normal workspace.
	s.srv.SetQueryWorkBudget(0)
	q := query(t, s, map[string]any{"selector": `func:where(::in:empty)`, "limit": 50})
	if q.Truncated {
		t.Errorf("default budget should not trip here; got note %q", q.Note)
	}
}

// A tripped budget must return the SAME partial result every run.
//
// The traversal carries node sets as map[*treeNode]bool, and ranging a
// Go map is randomized per run. That is invisible while a query runs to
// completion — the result set is the same however you reach it, and
// evaluate sorts before returning. The moment the budget trips it stops
// being invisible: the cutoff lands wherever the random walk happened to
// be, so the same selector answers differently every run. Truncated is
// allowed to be partial; it is not allowed to be a coin flip.
// The budgets here are tuned to trip PART WAY THROUGH a set of tips —
// that is the only window where order is observable. Too low and the
// walk dies before it branches (stable by luck); too high and it
// finishes (stable by completion). Both directions pass vacuously, so
// the test asserts Truncated to prove it is still in the window.
func TestTrippedBudgetIsReproducible(t *testing.T) {
	for _, tc := range []struct {
		sel    string
		budget int
	}{
		{`func::out`, 75},        // per-host walk in refMatches
		{`func:parents(*)`, 100}, // the :parents frontier
		{`*:where(::out)`, 100},  // pseudo pipeline per subject
	} {
		t.Run(tc.sel, func(t *testing.T) {
			s := startGraph(t)
			defer s.close()
			s.srv.SetQueryWorkBudget(tc.budget)

			var first string
			for i := 0; i < 25; i++ {
				q := query(t, s, map[string]any{"selector": tc.sel, "limit": 50})
				if !q.Truncated {
					t.Fatalf("budget %d no longer trips for %s — retune it, "+
						"or this test proves nothing", tc.budget, tc.sel)
				}
				got := fmt.Sprint(q.Matches)
				if i == 0 {
					first = got
					continue
				}
				if got != first {
					t.Fatalf("a tripped budget returned DIFFERENT results across runs — "+
						"truncation is a coin flip, not an answer.\nrun 0:  %s\nrun %d: %s",
						first, i, got)
				}
			}
		})
	}
}

// A LOCAL is only visible inside its own function. Edges are name-keyed
// off the lexical index, so without a scope rule every function's `t`
// became an edge to every OTHER function's `t`.
//
// The existing fixtures could not catch this: they are small enough that
// no two functions share a local name. On a real workspace it dominated
// the graph — 1,250,227 of func::out's 1,263,196 far ends (99.0%) pointed
// at a local of another function, 96% of them parameters, and one edge
// claimed 492 far ends because 492 tests take a `t`. Hence a fixture that
// deliberately reuses a parameter name across functions and files.
func TestLocalsDoNotEdgeToOtherFunctionsLocals(t *testing.T) {
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
	write("go.mod", "module scoped\ngo 1.26\n")
	// `v` is a parameter of all three, in two files. Three unrelated
	// bindings that merely share a spelling.
	write("a.go", `package scoped

func Alpha(v string) string {
	return v + "a"
}

func Beta(v string) string {
	return v + "b"
}
`)
	write("b.go", `package scoped

func Gamma(v string) string {
	return v + "g"
}
`)
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// Alpha's use of `v` must resolve to Alpha's own `v` — never Beta's
	// (same file) and never Gamma's (another file).
	q := query(t, s, map[string]any{"selector": `#'a.go#Alpha'::out`, "limit": 50})
	for _, m := range q.Matches {
		for _, to := range m.To {
			if strings.HasPrefix(to, "a.go#Beta") || strings.HasPrefix(to, "b.go#Gamma") {
				t.Errorf("Alpha's local `v` edged to another function's local: %s\n"+
					"a local is not visible outside its own body", to)
			}
		}
	}

	// The mirror: Beta's `v` is referenced only from inside Beta.
	in := query(t, s, map[string]any{"selector": `#'a.go#Beta.v'::in`, "limit": 50})
	for _, m := range in.Matches {
		for _, from := range m.From {
			if !strings.HasPrefix(from, "a.go#Beta") {
				t.Errorf("Beta's local `v` claimed an incoming edge from %s — "+
					"only Beta's own body can reference it", from)
			}
		}
	}
}

// Asking for ONE direction must not build the other.
//
// buildRefs used to materialize both halves and let the caller filter,
// so ::out silently paid the ::in bill — and the ::in half sweeps every
// occurrence of the name workspace-wide. Measured on a real workspace:
// #New::out cost 1.78M work units for the same 49 matches that 140k
// buys, against a 200k default budget. That single waste is what made
// the whole edge half of the language unusable by default.
//
// This asserts the CONTRACT, not the numbers: after an ::out query, no
// node has an incoming half built.
func TestOutQueryNeverBuildsIncomingRefs(t *testing.T) {
	s := startGraph(t)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	list, err := parseModernSelector(`func::out`)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.srv.buildTree()
	if err != nil {
		t.Fatal(err)
	}
	if rows := e.evaluate(list); len(rows) == 0 {
		t.Fatal("fixture should have outgoing edges for this to prove anything")
	}

	var walk func(n *treeNode)
	walk = func(n *treeNode) {
		if n.refsInLoaded {
			t.Fatalf("::out built the INCOMING half for %s — the expensive "+
				"direction nobody asked for", n.addr())
		}
		for _, c := range n.children {
			walk(c)
		}
	}
	walk(e.project)
}

// --------------------------------------------------------- name vs path

// [name] and [path] are disjoint axes: a node is CALLED something and
// it LIVES somewhere. They were one axis until [name] was found
// answering path questions — on a real workspace func[name*=test]
// returned 508 funcs (every func in a _test.go file, matched through
// the "<file>#<sym>" address id) where 1 was actually named *test*.
// That left no spelling for "named test", which is the whole point of
// splitting them.
func TestNameAndPathAreDifferentAxes(t *testing.T) {
	s := startGraph(t)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// The fixture's funcs live in main.go / web/*.ts and none is NAMED
	// for its file, so the two axes must disagree.
	byPath := query(t, s, map[string]any{"selector": `func[path$=.ts]`, "limit": 50})
	wantNodes(t, byPath, "web/app.ts#useHelper", "web/util.ts#tsHelper")

	// [name] must not see the path at all: no func is CALLED "app.ts".
	byName := query(t, s, map[string]any{"selector": `func[name*=app.ts]`, "limit": 50})
	if byName.TotalMatches != 0 {
		t.Errorf("[name] leaked the path — func[name*=app.ts] matched %v; "+
			"paths belong to [path]", nodes(byName))
	}
	// ...and a dir/file is CALLED its basename, not its path.
	fileByName := query(t, s, map[string]any{"selector": `file[name*=web/]`, "limit": 50})
	if fileByName.TotalMatches != 0 {
		t.Errorf("[name] leaked the path on a file: %v", nodes(fileByName))
	}
	fileByPath := query(t, s, map[string]any{"selector": `file[path*=web/]`, "limit": 50})
	if fileByPath.TotalMatches == 0 {
		t.Error("[path*=web/] should match the files under web/")
	}

	// #id keeps spanning BOTH, addresses included — pinning one symbol
	// by its "<file>#<sym>" address is the language's anchor move and
	// must survive the split.
	pinned := query(t, s, map[string]any{"selector": `#'web/app.ts#useHelper'`, "limit": 50})
	wantNodes(t, pinned, "web/app.ts#useHelper")
}

// The motivating query, end to end: "the funcs that are not test funcs"
// must be expressible, and must not be answered by name.
func TestNonTestFilterUsesPathAxis(t *testing.T) {
	s := startGraph(t)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	all := query(t, s, map[string]any{"selector": `func`, "limit": 50})
	ts := query(t, s, map[string]any{"selector": `func[path$=.ts]`, "limit": 50})
	notTS := query(t, s, map[string]any{"selector": `func:not([path$=.ts])`, "limit": 50})

	if all.TotalMatches != ts.TotalMatches+notTS.TotalMatches {
		t.Errorf("a path filter and its negation must partition the set: "+
			"all=%d ts=%d not-ts=%d", all.TotalMatches, ts.TotalMatches, notTS.TotalMatches)
	}
	for _, n := range nodes(notTS) {
		if strings.HasSuffix(n, ".ts") || strings.Contains(n, "web/") {
			t.Errorf(":not([path$=.ts]) returned a .ts node: %s", n)
		}
	}
}

// `~=` is how this language spells OR, and the two boolean axes must
// hold together: OR is the regex's own `|`, AND is a compound of attrs
// (CSS conjoins them already — no operator invented for it).
func TestAttrRegexIsTheOrOperator(t *testing.T) {
	s := startGraph(t)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// OR: the union of two literal filters, in one test.
	ts := query(t, s, map[string]any{"selector": `func[path*=app]`, "limit": 50})
	util := query(t, s, map[string]any{"selector": `func[path*=util]`, "limit": 50})
	either := query(t, s, map[string]any{"selector": `func[path~=app|util]`, "limit": 50})
	if either.TotalMatches != ts.TotalMatches+util.TotalMatches {
		t.Errorf("[path~=a|b] must be the union: app=%d util=%d either=%d",
			ts.TotalMatches, util.TotalMatches, either.TotalMatches)
	}

	// Anchors subsume ^= and $= exactly.
	if re, lit := query(t, s, map[string]any{"selector": `file[path~=\.ts$]`, "limit": 50}),
		query(t, s, map[string]any{"selector": `file[path$=.ts]`, "limit": 50}); re.TotalMatches != lit.TotalMatches {
		t.Errorf("[path~=\\.ts$] and [path$=.ts] must agree: %d vs %d", re.TotalMatches, lit.TotalMatches)
	}

	// AND needs no operator: a compound conjoins.
	both := query(t, s, map[string]any{"selector": `func[path*=web][path*=.ts]`, "limit": 50})
	for _, n := range nodes(both) {
		if !strings.Contains(n, "web") || !strings.Contains(n, ".ts") {
			t.Errorf("compound attrs must AND; got %s", n)
		}
	}

	// A bad pattern is a selector ERROR at parse time, never a silent
	// zero-match that reads like "nothing matched".
	if msg := queryErr(t, s, map[string]any{"selector": `func[path~=a(b]`}); !strings.Contains(msg, "bad regex") {
		t.Errorf("a bad regex must say so; got %s", msg)
	}
}

// A literal op with a `|` in it is an alternation attempt: it looks for
// the literal "a|b", finds nothing, and a wrapping :not() then excludes
// nothing and hands back the WHOLE set — measured at 820/820 funcs
// before ~= existed. A filter may match nothing; it may not look like
// it filtered. Quoting is the escape for a real '|'.
func TestLiteralOpRefusesAlternationAndNamesRegex(t *testing.T) {
	s, _ := startModern(t)
	defer s.close()

	msg := queryErr(t, s, map[string]any{"selector": `func:not([path*=test|smoke])`})
	for _, want := range []string{
		"silently no-ops",
		"[path~=test|smoke]", // the repair, spelled out
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("literal-op alternation must name the regex fix; missing %q in: %s", want, msg)
		}
	}
	// Quoted is the literal escape and must NOT trip the guard — it has
	// to parse and simply match nothing, which is the honest answer for
	// a path that really contains "a|b".
	if r := s.callTool("node_query", map[string]any{"selector": `func[path*='a|b']`}); r.IsError {
		t.Errorf("a quoted '|' is a literal, not an alternation attempt; got error: %s",
			r.Content[0].Text)
	}
}

// --------------------------------------------------------- guided errors

func TestRetiredSpellingsNameTheirReplacement(t *testing.T) {
	s := startGraph(t)
	defer s.close()

	// NB: [name~=X] used to live here — CSS's word-list match is useless
	// on names, so it was an error pointing at ^= *= $=. It is now the
	// REGEX op (the language's OR: [path~=a|b]), so it is a real
	// spelling and no longer retired. See TestAttrRegexIsTheOrOperator.
	for sel, want := range map[string]string{
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
