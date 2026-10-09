// Package main is the operator-driven harness for the
// "standard graph query language vs the CSS-style selector" round.
//
// It reads a question bank and one language card, asks the local model for
// one query per question, EXECUTES the answer against the poly-lsp-mcp graph,
// scores correctness against the bank's ground truth, and records retries,
// prompt tokens and wall time per question.
//
// The three candidate languages (GSL, a Cypher subset, a SQL subset) are
// compared on IDENTICAL data: the node/edge rows in this file. GSL runs
// in-process through the mcp package; Cypher and SQL run in-process over the
// same rows. The rows are DERIVED from mcp.QueryJSON — the mcp tree and
// engine are unexported, and the symbols.Index public API carries names and
// sites but not edge KINDS (call/type/import), which live only in the tree.
// So QueryJSON is the single source of truth for both the GSL answers and
// the Cypher/SQL rows, which is exactly the "same node/edge rows" guarantee
// the round needs: a correct query in any language sees the same graph.
package main

// Node is one node of the shared row model. ID is the canonical address
// (see score.go: a symbol is "file#Name", a file/dir its workspace-relative
// path, the project root its base name). Name is the leaf symbol name (""
// for file/dir/project). Kind is the GSL tag (func, method, type, file, ...).
type Node struct {
	ID   string
	File string
	Name string
	Kind string
}

// Edge is one directed reference edge between two nodes. From and To are
// node IDs (the "file#Name" addresses). Kind is the reference kind
// (call, type, import) — the axis the GSL .call/.type/.import selectors
// expose.
type Edge struct {
	From string
	To   string
	Kind string
}

// Rows is the shared node/edge set the three languages run over.
type Rows struct {
	Nodes []Node
	Edges []Edge
}

// nodeByID indexes Rows.Nodes by ID for the executors.
func (r *Rows) nodeByID() map[string]Node {
	m := make(map[string]Node, len(r.Nodes))
	for _, n := range r.Nodes {
		m[n.ID] = n
	}
	return m
}

// edgeKindSet returns the distinct edge kinds present, sorted. Used by the
// Cypher/SQL cards' documentation of what relationship types are available.
func (r *Rows) edgeKindSet() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range r.Edges {
		if !seen[e.Kind] {
			seen[e.Kind] = true
			out = append(out, e.Kind)
		}
	}
	return out
}
