package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

// F5: the `&` intersection operator between two ref clauses. The canonical
// form for "A AND B" — two edge steps from the same node, each landing on
// its far nodes; the answer is the nodes in BOTH.

// Fixture graph (main.go):
//
//	Store  ◀──type──  UseStore   (UseStore declares a var of type Store)
//	run ────call────▶ UseStore   (run calls UseStore)
//	run ────call────▶ Other      (run calls Other; Other does NOT use Store)
//
// So UseStore uses Store AND is called by run; Other is only called by run.
const intersectGoSrc = `package lib

type Store struct{}

func UseStore() { var s Store; _ = s }

func Other() {}

func run() { UseStore(); Other() }
`

func startIntersect(t *testing.T) *mcpSession {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module lib\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte(intersectGoSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	s := startSessionFull(t, dir, nil, nil)
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	return s
}

// The two sides, alone, each return their far nodes. UseStore is in both;
// Other is only in the call side. This is the premise the intersection
// composes, and it pins the edge directions the operator depends on.
func TestIntersectionSides(t *testing.T) {
	s := startIntersect(t)
	defer s.close()

	// Store used AS A TYPE by exactly UseStore.
	q := query(t, s, map[string]any{"selector": `#'main.go#Store'::in.type`})
	wantNodes(t, q, "main.go#UseStore")

	// run's outgoing calls land on UseStore and Other.
	q = query(t, s, map[string]any{"selector": `#run::out.call`})
	wantNodes(t, q, "main.go#UseStore", "main.go#Other")
}

// The canonical intersection form: a node that has BOTH edge types in the
// required directions appears exactly once; a node with only one of the two
// is absent.
func TestIntersectionReturnsBothSides(t *testing.T) {
	s := startIntersect(t)
	defer s.close()

	q := query(t, s, map[string]any{
		"selector": `#'main.go#Store'::in.type & #run::out.call`,
	})
	// UseStore is the one node in both sets; Other (call-only) is not.
	wantNodes(t, q, "main.go#UseStore")

	// Each answer is one plain symbol (far node), not an edge row, and the
	// node appears once — not duplicated by the two sides.
	if q.TotalMatches != 1 {
		t.Errorf("intersection = 1 node (UseStore); got %d (%v)", q.TotalMatches, nodes(q))
	}
	for _, m := range q.Matches {
		if m.Class != "func" {
			t.Errorf("far-node type = %q, want func (not an edge row)", m.Class)
		}
	}
}

// A node with only ONE of the two edge types must not appear: Other is
// called by run but never uses Store, so it is excluded by the type side.
func TestIntersectionExcludesOneSide(t *testing.T) {
	s := startIntersect(t)
	defer s.close()

	q := query(t, s, map[string]any{
		"selector": `#'main.go#Store'::in.type & #run::out.call`,
	})
	if hasNode(q, "main.go#Other") {
		t.Errorf("Other (call-only) must not be in the intersection; got %v", nodes(q))
	}
}

// Chaining: A & B & C reads right-associated, so the middle and last sides
// compose; a node must survive every side. Here C reuses the type side, so
// the answer is unchanged — the parser must accept the chain and evaluate
// it, not reject a second `&`.
func TestIntersectionChains(t *testing.T) {
	s := startIntersect(t)
	defer s.close()

	q := query(t, s, map[string]any{
		"selector": `#'main.go#Store'::in.type & #run::out.call & #'main.go#Store'::in.type`,
	})
	wantNodes(t, q, "main.go#UseStore")
}
