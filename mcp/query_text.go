package mcp

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
)

// QueryText compiles a selector, evaluates it against the workspace
// tree, and renders the matches for a human reader — grouped by the
// file each match lives in.
//
// This is the CLI's view of exactly the path node_query serves to a
// model (parseModernSelector -> buildTree -> evaluate). Only the
// rendering differs: node_query emits compact JSON to save the model
// tokens, which is the wrong shape for a terminal. Keep the compile /
// evaluate / paginate contract here identical to
// handleModernNodeQuery — a selector that behaves one way for the CLI
// and another for the model makes this tool a liar.
//
// limit <= 0 means "no limit": a human at a terminal wants the whole
// answer, where the model's tight default window exists to push it
// toward a narrower selector.
func (s *Server) QueryText(selector string, limit, offset int, w io.Writer) error {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return errors.New("selector is required (e.g. \":root > *\" for the top-level tour)")
	}
	// "?" answers with the grammar — same contract as the MCP tool.
	if selector == "?" {
		_, err := fmt.Fprintln(w, selectorGrammarHelp)
		return err
	}
	if offset < 0 {
		return errors.New("offset must be >= 0")
	}

	selector, explain := splitExplain(selector)
	list, err := parseModernSelector(selector)
	if err != nil {
		return err
	}
	// Reference edges resolve through the symbol index, which the MCP
	// server builds during `initialize`. A one-shot CLI run has no
	// initialize, so build it here or every ::in/::out selector would
	// quietly answer "no matches" instead of the truth.
	if s.getIndex() == nil {
		if err := s.BuildIndex(); err != nil {
			return err
		}
	}
	e, err := s.buildTree()
	if err != nil {
		return err
	}
	rows := e.evaluate(list)

	total := len(rows)
	if limit <= 0 {
		limit = total
	}
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	paged := rows[offset:end]

	if explain {
		return renderExplain(w, e.explainRows(list), e.workExceeded)
	}

	var trace []string
	if e.workExceeded {
		trace = e.costTrace(list)
	}
	return renderQueryTree(w, paged, total, offset, end, e.workExceeded, trace)
}

// maxFarEnds caps how many far ends one ref row spells out before it
// starts counting them instead.
const maxFarEnds = 3

// queryGroup is one file's worth of matches. self is the group's own
// node when the container itself matched (a `file.go` selector matches
// the file node) — the header already states it, so it never also
// appears as a child row.
type queryGroup struct {
	key  string
	self *treeNode
	rows []*treeNode
}

// groupKey buckets a match by the file it lives in. Symbols, refs and
// fragments all carry their host file; a matched file or dir node is
// its own bucket; the project node has no file at all.
func groupKey(n *treeNode) string {
	if n.class == "project" {
		return n.full
	}
	return n.file
}

func renderQueryTree(w io.Writer, paged []*treeNode, total, offset, end int, workExceeded bool, trace []string) error {
	if total == 0 {
		// A bare "no matches" would claim the selector's answer IS none.
		// When the budget killed the walk, that is not what happened and
		// not what we know — say which one this is.
		if workExceeded {
			fmt.Fprintln(w, "no matches — but evaluation stopped at the work budget FIRST,")
			fmt.Fprintln(w, "so this is NOT an answer: the walk never finished.")
			return writeBudgetBlow(w, trace)
		}
		_, err := fmt.Fprintln(w, "no matches")
		return err
	}

	// Rows arrive in deterministic pre-order; first-seen group order
	// preserves it.
	var groups []*queryGroup
	byKey := map[string]*queryGroup{}
	for _, n := range paged {
		k := groupKey(n)
		g := byKey[k]
		if g == nil {
			g = &queryGroup{key: k}
			byKey[k] = g
			groups = append(groups, g)
		}
		if isContainerOf(n, k) {
			g.self = n
			continue
		}
		g.rows = append(g.rows, n)
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for i, g := range groups {
		// Blank-line separation earns its keep only around a group
		// that actually has children. A result that is all headers
		// (`:root > *`) is a list, and a list double-spaced is just
		// harder to read.
		if i > 0 && (len(g.rows) > 0 || len(groups[i-1].rows) > 0) {
			fmt.Fprintln(tw)
		}
		fmt.Fprintln(tw, groupHeader(g))
		for j, n := range g.rows {
			glyph := "├─"
			if j == len(g.rows)-1 {
				glyph = "└─"
			}
			fmt.Fprintf(tw, "%s %s\t%s\n", glyph, describeNode(n), nodeSpan(n))
		}
	}
	fmt.Fprintln(tw)
	fmt.Fprintln(tw, summarize(groups, total, offset, end))
	if err := tw.Flush(); err != nil {
		return err
	}
	if workExceeded {
		// Same contract as node_query: a budget-trimmed result says so
		// and names the fix. Never trim quietly.
		fmt.Fprintln(w, "warning: evaluation stopped at the work budget — results are INCOMPLETE.")
		return writeBudgetBlow(w, trace)
	}
	return nil
}

// renderExplain prints the :explain cost tree — each element's a-priori
// est beside the measured work it actually cost, with the ">x" floor on
// the element the budget tripped in.
func renderExplain(w io.Writer, rows []explainRow, workExceeded bool) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "element\test\tmeasured")
	for _, r := range rows {
		mark := ""
		if r.Blown {
			mark = "\t← budget ran out here"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s%s\n", r.Element, r.Est, r.Measured, mark)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "est = a-priori (free, from index tallies); measured = work actually spent.")
	if workExceeded {
		fmt.Fprintln(w, ">x = lower bound (budget tripped mid-element); — = never reached.")
	}
	return nil
}

// writeBudgetBlow renders the per-element cost trace (pointing at the
// element that ate the budget) then the narrow-it advice. The trace is
// what turns the generic warning into something legible.
func writeBudgetBlow(w io.Writer, trace []string) error {
	if len(trace) > 0 {
		fmt.Fprintln(w, "cost by element (units spent):")
		for _, l := range trace {
			fmt.Fprintln(w, l)
		}
	}
	_, err := fmt.Fprintln(w, "  Narrow the traversal: a kind class (::in.call), a filtered inner\n"+
		"  (:parents(func)), bounded hops ({1,3}), or a tighter scope.")
	return err
}

// isContainerOf reports whether n is the node its own group is named
// after, rather than something living inside it.
func isContainerOf(n *treeNode, key string) bool {
	switch n.class {
	case "project", "dir", "file":
		return n.addr() == key || (n.class == "project" && n.full == key)
	}
	return false
}

func groupHeader(g *queryGroup) string {
	if g.key == "" {
		return "(project)"
	}
	if g.self != nil && g.self.class == "dir" {
		return g.key + "/"
	}
	return g.key
}

func describeNode(n *treeNode) string {
	switch n.class {
	case "ref":
		t := "::" + n.refDir
		if n.refPos != "" {
			t += "." + n.refPos
		}
		if n.refKind != "" {
			t += "." + n.refKind
		}
		arrow := "←"
		if n.refDir == "out" {
			arrow = "→"
		}
		far := make([]string, 0, len(n.refFar))
		for _, f := range n.refFar {
			far = append(far, f.addr())
		}
		// Edges are name-keyed via the lexical index, so a common name
		// can carry dozens of far ends — enough to bury the rest of the
		// output. Show a few and SAY how many were held back; a silent
		// "…" would read as if that were the whole edge.
		if len(far) > maxFarEnds {
			held := len(far) - maxFarEnds
			far = append(far[:maxFarEnds:maxFarEnds], fmt.Sprintf("(+%d more)", held))
		}
		return fmt.Sprintf("%s %s %s", t, arrow, strings.Join(far, ", "))
	case "fragment":
		return fmt.Sprintf("::grep %q", n.frag.Text)
	case "project", "dir", "file":
		return n.class + " " + n.leaf
	}
	name := n.sym
	if name == "" {
		name = n.leaf
	}
	return n.class + " " + name
}

// nodeSpan renders the source line range. project/dir nodes have no
// span; a one-line node states the single line rather than "7-7".
func nodeSpan(n *treeNode) string {
	if n.class == "project" || n.class == "dir" {
		return ""
	}
	if n.at[0] == n.at[1] {
		return strconv.Itoa(n.at[0])
	}
	return fmt.Sprintf("%d-%d", n.at[0], n.at[1])
}

func summarize(groups []*queryGroup, total, offset, end int) string {
	// "files" is only honest when every group is one; a dir or the
	// project node in the results makes them locations.
	noun := "files"
	for _, g := range groups {
		if g.self != nil && g.self.class != "file" {
			noun = "locations"
			break
		}
	}
	unit := noun
	if len(groups) == 1 {
		unit = strings.TrimSuffix(noun, "s")
	}
	if end-offset < total {
		return fmt.Sprintf("%d %s · showing %d–%d of %d matches — raise --limit or use --offset",
			len(groups), unit, offset+1, end, total)
	}
	return fmt.Sprintf("%d %s · %d matches", len(groups), unit, total)
}
