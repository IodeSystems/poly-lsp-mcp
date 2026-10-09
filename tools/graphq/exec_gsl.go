package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
)

// newGSLServer builds the in-process mcp server the GSL executor and the row
// builder share. No daemon, no shell, no stdio: New + BuildIndex, exactly the
// one-shot query CLI's path (mcp/server.go: New; mcp/query_text.go: QueryJSON).
//
// root is the poly-lsp-mcp checkout being queried. The registry is the
// default one (config.Default().Build()) — go plus the languages present —
// the same registry main.go's query subcommand uses, so the harness sees the
// graph the dev toolset sees.
func newGSLServer(root string) (*mcp.Server, error) {
	reg, err := config.Default().Build()
	if err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	s := mcp.New(reg, root, nil, nil)
	if err := s.BuildIndex(); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	return s, nil
}

// match is one row of a mcp.QueryJSON document.
type match struct {
	Node string   `json:"node"`
	File string   `json:"file"`
	Type string   `json:"type"`
	From []string `json:"from"`
	To   []string `json:"to"`
}

// queryDoc is one mcp.QueryJSON document.
type queryDoc struct {
	Matches []match `json:"matches"`
	Total   int     `json:"total"`
}

// runQuery executes one GSL selector in-process and returns the parsed rows.
// A *mcp.SelectorError (the model's query failed to parse/execute) is returned
// via the selectorErr out-param so the caller can feed its message back as the
// single retry; an operational error (index/tree build) is returned as err, a
// hard failure for the question.
func (h *harness) runQuery(selector string) (matches []match, selectorErr *mcp.SelectorError, err error) {
	var out bytes.Buffer
	if e := h.server.QueryJSON(selector, h.limit, 0, h.budget, &out); e != nil {
		if se, ok := unwrapSelector(e); ok {
			return nil, se, nil
		}
		return nil, nil, e
	}
	var doc queryDoc
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("parse QueryJSON output: %w", err)
	}
	return doc.Matches, nil, nil
}

// unwrapSelector reports whether err is (or wraps) a *mcp.SelectorError.
func unwrapSelector(err error) (*mcp.SelectorError, bool) {
	for err != nil {
		if se, ok := err.(*mcp.SelectorError); ok {
			return se, true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return nil, false
		}
		err = u.Unwrap()
	}
	return nil, false
}

// BuildRows derives the shared node/edge set the three languages run over,
// from the SAME in-process mcp server (hence the "same node/edge rows"
// guarantee — QueryJSON is the one source of truth).
//
//   - Nodes: the universal selector `*` returns every symbol and file node in
//     one walk (no limit), normalized to the canonical address form.
//   - Edges: reference edges (with their KIND) are reachable only through
//     `::in`/`::out` per symbol — the tree and symbols.Index public API do not
//     expose edge kinds in bulk. A per-symbol sweep would be O(symbols) tree
//     builds, so we do a bounded BFS from the SEEDS: the symbols the round is
//     actually about. For each seed we read `#'sym'::in` and `#'sym'::out`
//     (all kinds) and one hop out to cover transitive questions. edgeQueryCap
//     bounds the total edge queries so a large bank can't stall the run.
//
// Seeds come from the bank (truth-derived symbol names + any explicit seeds),
// so the derived graph contains exactly the edges the questions can ask about.
func (h *harness) BuildRows(seeds []string) (*Rows, error) {
	rows := &Rows{}

	// Nodes: every symbol and file, one query, no limit.
	matches, se, err := h.runQuery("*")
	if se != nil {
		return nil, fmt.Errorf("node sweep `*`: %w", se.Err)
	}
	if err != nil {
		return nil, fmt.Errorf("node sweep `*`: %w", err)
	}
	seenNode := map[string]bool{}
	for _, m := range matches {
		n := normalizeNode(m)
		if n.ID == "" || seenNode[n.ID] {
			continue
		}
		seenNode[n.ID] = true
		rows.Nodes = append(rows.Nodes, n)
	}

	// Edges: bounded BFS from the seeds. The seeds are already bare symbol
	// names (the caller derives them via symbolLeaf), so they are used as-is.
	byID := rows.nodeByID()
	var frontier []string
	queued := map[string]bool{}
	for _, s := range seeds {
		s = strings.TrimSpace(s)
		if s != "" && !queued[s] {
			queued[s] = true
			frontier = append(frontier, s)
		}
	}

	seenEdge := map[string]bool{}
	edgeQueries := 0
	// depth 0 = the seeds' own edges; depth 1 = one hop out (transitive).
	for depth := 0; depth < 2 && len(frontier) > 0; depth++ {
		var next []string
		for _, name := range frontier {
			if edgeQueries >= h.edgeQueryCap {
				break
			}
			for _, dir := range []string{"in", "out"} {
				sel := "#'" + name + "'::" + dir
				em, se, err := h.runQuery(sel)
				if se != nil || err != nil {
					// A seed the graph doesn't have (renamed, external) is
					// skipped: its questions will simply miss, the honest
					// outcome. Not a build failure.
					continue
				}
				edgeQueries++
				for _, m := range em {
					e, ok := normalizeEdge(m, name, dir, byID)
					if !ok {
						continue
					}
					k := e.From + "\x00" + e.To + "\x00" + e.Kind
					if seenEdge[k] {
						continue
					}
					seenEdge[k] = true
					rows.Edges = append(rows.Edges, e)
					for _, far := range []string{e.From, e.To} {
						if !seenNode[far] {
							seenNode[far] = true
							rows.Nodes = append(rows.Nodes, Node{ID: far, File: fileOf(far), Name: symbolLeaf(far)})
						}
					}
					if depth == 0 {
						next = append(next, e.From, e.To)
					}
				}
			}
			if edgeQueries >= h.edgeQueryCap {
				break
			}
		}
		frontier = dedupe(next, queued)
	}

	sort.Slice(rows.Nodes, func(i, j int) bool { return rows.Nodes[i].ID < rows.Nodes[j].ID })
	sort.Slice(rows.Edges, func(i, j int) bool {
		if rows.Edges[i].From != rows.Edges[j].From {
			return rows.Edges[i].From < rows.Edges[j].From
		}
		if rows.Edges[i].To != rows.Edges[j].To {
			return rows.Edges[i].To < rows.Edges[j].To
		}
		return rows.Edges[i].Kind < rows.Edges[j].Kind
	})
	return rows, nil
}

// normalizeNode maps one QueryJSON match to a Node in the canonical address
// form (score.go documents it): a symbol is "file#Name", a file/dir its
// workspace-relative path.
func normalizeNode(m match) Node {
	id := m.Node
	if id == "" {
		return Node{}
	}
	return Node{ID: id, File: fileOf(id), Name: symbolLeaf(id)}
}

// normalizeEdge maps one `::in`/`::out` match to a directed Edge. The anchor
// is the seed symbol the selector was written against; the far end is the
// row's from: (for ::in) or to: (for ::out).
func normalizeEdge(m match, anchor, dir string, byID map[string]Node) (Edge, bool) {
	kind := edgeKindFromType(m.Type)
	var from, to string
	if dir == "in" {
		if len(m.From) == 0 {
			return Edge{}, false
		}
		from, to = m.From[0], anchorID(anchor, byID)
	} else {
		if len(m.To) == 0 {
			return Edge{}, false
		}
		from, to = anchorID(anchor, byID), m.To[0]
	}
	if from == "" || to == "" {
		return Edge{}, false
	}
	return Edge{From: from, To: to, Kind: kind}, true
}

// anchorID returns the canonical node ID for a seed symbol name, preferring
// the node already in the set (so a same-named symbol resolves to the one `*`
// found) and falling back to the bare name.
func anchorID(name string, byID map[string]Node) string {
	for _, n := range byID {
		if n.Name == name {
			return n.ID
		}
	}
	return name
}

// edgeKindFromType parses the reference kind out of a row's type field
// ("::in.call" → "call", "::out.type.return" → "type", "::in" → "any").
func edgeKindFromType(typ string) string {
	for _, p := range strings.Split(typ, ".")[1:] {
		if p == "call" || p == "type" || p == "import" {
			return p
		}
	}
	return "any"
}

// symbolLeaf returns the leaf symbol name from a canonical address
// ("a.go#Save" → "Save"; "a.go@6" → ""; "a.go" → "").
func symbolLeaf(addr string) string {
	if i := strings.LastIndex(addr, "#"); i >= 0 {
		return addr[i+1:]
	}
	return ""
}

// fileOf returns the file part of a canonical address ("a.go#Save" → "a.go",
// "a.go@6" → "a.go", "a.go" → "a.go").
func fileOf(addr string) string {
	if i := strings.IndexAny(addr, "#@"); i >= 0 {
		return addr[:i]
	}
	return addr
}

// dedupe returns the names in xs not already in seen, in order.
func dedupe(xs []string, seen map[string]bool) []string {
	var out []string
	for _, x := range xs {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
