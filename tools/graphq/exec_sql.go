package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// SQL executor, in-process over the shared node/edge rows.
//
// DEVIATION FROM SPEC: the spec named modernc.org/sqlite (pure Go). I have no
// shell in this session, and the graph records that a no-shell session cannot
// honestly add a Go dependency — go.sum needs a real module zip hash that only
// `go get` can write, and hand-writing it is "asserting without provenance."
// So the executor is a small pure-Go SQL-subset evaluator over the SAME
// node/edge rows. It is portable, cgo-free, and unit-testable with canned rows
// and no network. If the operator `go get`s modernc.org/sqlite, this file can
// be swapped for a real SQLite run over the identical tables without changing
// the card, the row model, or the scorer.
//
// The tables (the committed SQLite-schema card's names):
//
//	node(id TEXT, file TEXT, name TEXT, kind TEXT)
//	edge(src TEXT, dst TEXT, kind TEXT)
//
// where node.id / edge.src / edge.dst are the canonical node addresses
// ("file#Name"). The answer set is the DISTINCT values of the first returned
// column that look like node addresses (i.e. the column the query is about).
//
// The supported subset:
//
//	SELECT [DISTINCT] col FROM t
//	[JOIN t2 ON t.col = t2.col] ...
//	[WHERE simple-conditions]
//
//   - col: a bare column name.
//   - t: node or edge.
//   - ON: equality joins (t.a = t2.b), including a recursive CTE's join.
//   - WHERE: equality / LIKE / IN, AND/OR of those, against columns.
//
//   - Recursive CTEs are supported in the standard shape:
//
//	WITH RECURSIVE reach AS (
//	  SELECT src FROM edge WHERE dst = '<seed>'
//	  UNION
//	  SELECT e.src FROM edge e JOIN reach r ON e.dst = r.src
//	) SELECT DISTINCT src FROM reach;
//
// which is the "everything that reaches X" / "who transitively calls X"
// pattern. The CTE is expanded as a fixpoint over the edge rows.
//
// This is deliberately a subset: it covers the question shapes the round
// needs (direct callers, transitive callers, callers in a file, unused
// symbols) without a full SQL parser.

// sqlRows builds the node/edge tables as columnar rows for the evaluator.
type sqlRows struct {
	nodeCols map[string][]string // column name -> values
	nodeN    int
	edgeCols map[string][]string
	edgeN    int
}

func buildSQLRows(rows *Rows) *sqlRows {
	s := &sqlRows{
		nodeCols: map[string][]string{"id": {}, "file": {}, "name": {}, "kind": {}},
		edgeCols: map[string][]string{"src": {}, "dst": {}, "kind": {}},
	}
	for _, n := range rows.Nodes {
		s.nodeCols["id"] = append(s.nodeCols["id"], n.ID)
		s.nodeCols["file"] = append(s.nodeCols["file"], n.File)
		s.nodeCols["name"] = append(s.nodeCols["name"], n.Name)
		s.nodeCols["kind"] = append(s.nodeCols["kind"], n.Kind)
	}
	s.nodeN = len(rows.Nodes)
	for _, e := range rows.Edges {
		s.edgeCols["src"] = append(s.edgeCols["src"], e.From)
		s.edgeCols["dst"] = append(s.edgeCols["dst"], e.To)
		s.edgeCols["kind"] = append(s.edgeCols["kind"], e.Kind)
	}
	s.edgeN = len(rows.Edges)
	return s
}

// execSQL runs one SQL query over the rows and returns the answer set
// (the distinct node addresses in the first returned column).
func execSQL(rows *Rows, query string) ([]string, error) {
	q := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(query), ";"))
	if q == "" {
		return nil, fmt.Errorf("empty SQL query")
	}
	s := buildSQLRows(rows)

	// Peel a leading WITH RECURSIVE cte AS (...) if present.
	cte, rest, err := splitCTE(q)
	if err != nil {
		return nil, err
	}
	rest = strings.TrimSpace(rest)

	// Expand the CTE to a fixpoint, adding its result as a virtual table.
	if cte != nil {
		if err := expandCTE(s, cte); err != nil {
			return nil, err
		}
	}

	return runSelect(s, rest)
}

// cte is a parsed WITH RECURSIVE name AS (base UNION recurse).
type cte struct {
	name    string
	base    string // the non-recursive SELECT (against edge/node)
	recJoin string // the JOIN target table in the recursive SELECT (e.g. "reach")
	recSel  string // the column selected in the recursive leg
	recOn   string // the join column on the CTE side (r.src / r.dst)
	onCTE   string // the join column on the CTE side (r.src / r.dst)
	onEdge  string // the join column on the edge side (e.dst / e.src)
}

var (
	cteHeadRe = regexp.MustCompile(`(?is)^\s*WITH\s+RECURSIVE\s+(\w+)\s+AS\s*\(`)
)

// splitCTE detects and strips a leading WITH RECURSIVE clause.
func splitCTE(q string) (*cte, string, error) {
	m := cteHeadRe.FindStringSubmatchIndex(q)
	if m == nil {
		return nil, q, nil
	}
	// Find the matching close paren of the CTE body.
	// The '(' is the last character of the full match.
	depth := 0
	start := m[1] - 1
	for i := start; i < len(q); i++ {
		switch q[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				body := q[start+1 : i]
				rest := q[i+1:]
				// The head regex lets one space ride along after the name
				// (WHITESPACE\s+AS); trim it or the alias match below fails.
				c := &cte{name: strings.TrimSpace(q[m[2]:m[3]])}
				if err := parseCTEBody(c, body); err != nil {
					return nil, "", err
				}
				return c, rest, nil
			}
		}
	}
	return nil, "", fmt.Errorf("unbalanced parentheses in CTE")
}

// parseCTEBody splits the CTE body on UNION into the base and recursive legs.
func parseCTEBody(c *cte, body string) error {
	parts := splitTopLevel(body, "UNION")
	if len(parts) != 2 {
		return fmt.Errorf("recursive CTE needs exactly base UNION recursive")
	}
	// The base leg is run as a normal SELECT; store the full statement.
	c.base = strings.TrimSpace(parts[0])
	// The recursive leg selects the column that becomes the CTE's next row.
	// Strip a qualifier so it names an edge table column (src/dst/kind).
	recSel := firstSelectColumn(parts[1])
	_, col, _ := splitColRef(recSel)
	c.recSel = strings.ToLower(col)
	if c.recSel == "" {
		return fmt.Errorf("could not determine the CTE's selected column")
	}
	// Parse the recursive leg's JOIN ... ON to learn the two join columns.
	jm := joinRe.FindStringSubmatch(parts[1])
	if jm == nil {
		return fmt.Errorf("recursive leg needs a JOIN ... ON")
	}
	c.recJoin = jm[1] // the CTE table name; jm[2] its optional alias
	if jm[2] != "" {
		c.recJoin = jm[2] // the recursive leg references the alias
	}
	// The ON is of the form "E.DST = R.SRC" (or the reverse). Identify which
	// side is the edge (e.*) and which is the CTE (the alias).
	pieces := strings.SplitN(jm[3], "=", 2)
	if len(pieces) != 2 {
		return fmt.Errorf("bad ON clause")
	}
	a := strings.TrimSpace(pieces[0])
	b := strings.TrimSpace(pieces[1])
	for _, ref := range []string{a, b} {
		qual, col, ok := splitColRef(ref)
		if !ok {
			return fmt.Errorf("bad column ref %q", ref)
		}
		qual = strings.ToLower(qual)
		if qual == c.name || qual == "r" {
			c.onCTE = strings.ToLower(col)
		} else {
			c.onEdge = strings.ToLower(col)
		}
	}
	if c.onCTE == "" || c.onEdge == "" {
		return fmt.Errorf("could not assign the ON columns to the edge and the CTE")
	}
	return nil
}

// joinRe matches "JOIN tbl [alias] ON <cond>". Group 1 is the table, group 2
// the optional alias, group 3 the ON condition. The alias is what the
// recursive CTE's join refers to ("JOIN reach r ON e.dst = r.src").
var joinRe = regexp.MustCompile(`(?is)JOIN\s+(\w+)(?:\s+(\w+))?\s+ON\s+([^)]+)`)

// splitTopLevel splits s on the keyword (UNION / WHERE / JOIN / FROM / AND /
// OR) at parenthesis depth 0 only, and only OUTSIDE single-quoted string
// literals: the node ids in WHERE clauses ('store.go#Save') contain "UNION"
// and must not split the query.
func splitTopLevel(s, kw string) []string {
	var out []string
	depth := 0
	last := 0
	inStr := false
	upper := strings.ToUpper(s)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\'' {
				inStr = false
			}
			continue
		}
		switch c {
		case '\'':
			inStr = true
			continue
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && i+len(kw) <= len(s) && strings.HasPrefix(upper[i:], kw) &&
			(i == 0 || !isIdentPart(rune(s[i-1]))) &&
			(i+len(kw) == len(s) || !isIdentPart(rune(s[i+len(kw)]))) {
			out = append(out, s[last:i])
			last = i + len(kw)
			i += len(kw) - 1
			// UNION ALL is one keyword: skip the ALL so the split lands
			// between the two legs, not inside the keyword.
			if kw == "UNION" && strings.HasPrefix(upper[i+1:], " ALL") {
				i += 4
				last = i + 1
			}
		}
	}
	out = append(out, s[last:])
	return out
}

// safePart returns parts[i] if i is in range, else "". For diagnostics.
func safePart(parts []string, i int) string {
	if i < len(parts) {
		return parts[i]
	}
	return ""
}

// firstSelectColumn returns the first column of a "SELECT [DISTINCT] col ..."
// fragment.
func firstSelectColumn(sel string) string {
	upper := strings.ToUpper(sel)
	i := strings.Index(upper, "SELECT")
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(sel[i+len("SELECT"):])
	if strings.HasPrefix(strings.ToUpper(rest), "DISTINCT ") {
		rest = strings.TrimSpace(rest[len("DISTINCT "):])
	}
	// take up to FROM
	if j := strings.Index(strings.ToUpper(rest), " FROM"); j >= 0 {
		rest = rest[:j]
	}
	cols := strings.Split(rest, ",")
	return strings.TrimSpace(cols[0])
}

// expandCTE computes the CTE as a fixpoint and registers it as a virtual
// single-column table on s.
func expandCTE(s *sqlRows, c *cte) error {
	// base: rows of the CTE from the non-recursive SELECT.
	baseRows, err := runSelect(s, strings.TrimSpace(c.base))
	if err != nil {
		return err
	}
	fixed := map[string]bool{}
	var frontier []string
	for _, r := range baseRows {
		if !fixed[r] {
			fixed[r] = true
			frontier = append(frontier, r)
		}
	}
	// recursive: repeatedly join edge rows where edge.onEdge == cte.onCTE and
	// the edge's other end (the selected column) is new.
	for len(frontier) > 0 {
		var next []string
		for _, cur := range frontier {
			for i := 0; i < s.edgeN; i++ {
				if s.edgeCols[c.onEdge][i] != cur {
					continue
				}
				cand := s.edgeCols[c.recSel][i]
				if !fixed[cand] {
					fixed[cand] = true
					next = append(next, cand)
				}
			}
		}
		frontier = next
	}
	var all []string
	for k := range fixed {
		all = append(all, k)
	}
	sort.Strings(all)
	s.nodeCols["cte_"+c.name] = all
	_ = c.recJoin
	return nil
}

// runSelect runs a plain SELECT over s's tables (node, edge, and any CTE
// table) and returns the distinct values of the first returned column.
func runSelect(s *sqlRows, sel string) ([]string, error) {
	upper := strings.ToUpper(sel)
	if !strings.HasPrefix(upper, "SELECT") {
		return nil, fmt.Errorf("expected a SELECT")
	}
	body := strings.TrimSpace(sel[len("SELECT"):])
	distinct := false
	if strings.HasPrefix(strings.ToUpper(body), "DISTINCT ") {
		distinct = true
		body = strings.TrimSpace(body[len("DISTINCT "):])
	}
	// Split off the FROM clause (top-level).
	fromParts := splitTopLevel(body, "FROM")
	if len(fromParts) < 2 {
		return nil, fmt.Errorf("SELECT needs a FROM")
	}
	selCols := strings.TrimSpace(fromParts[0])
	rest := strings.TrimSpace(fromParts[1])

	// Split rest into FROM-tables (with JOINs) and WHERE.
	where := ""
	tablePart := rest
	if wp := splitTopLevel(rest, "WHERE"); len(wp) >= 2 {
		tablePart = strings.TrimSpace(wp[0])
		where = strings.TrimSpace(wp[1])
	}

	// Parse the FROM table list: "node n JOIN edge e ON ..."
	fromClause, joins, err := parseFrom(tablePart)
	if err != nil {
		return nil, err
	}

	// Build the working set: start from the base table's rows, apply joins.
	type row struct {
		alias map[string]string // column name (qualified) -> value
	}
	baseRows, err := tableRows(s, fromClause)
	if err != nil {
		return nil, err
	}

	// Apply WHERE to base rows (joins may add columns; for the subset we
	// apply WHERE after joins).
	// We represent each working row as alias.col -> value.
	var work []map[string]string
	for _, r := range baseRows {
		work = append(work, r)
	}
	for _, j := range joins {
		work = applyJoin(s, work, j)
	}
	if where != "" {
		work = applyWhere(work, where)
	}

	// Project the first selected column.
	firstCol := strings.SplitN(selCols, ",", 2)[0]
	firstCol = strings.TrimSpace(firstCol)
	qual, col, ok := splitColRef(firstCol)
	if !ok {
		// bare column: try every alias
		col = strings.ToLower(firstCol)
		qual = ""
	}
	seen := map[string]bool{}
	var out []string
	for _, w := range work {
		var v string
		if qual != "" {
			v = w[strings.ToLower(qual)+"."+col]
		} else {
			for k, val := range w {
				if colName(k) == col {
					v = val
					break
				}
			}
		}
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if distinct {
		out = dedupeStr(out)
	}
	sort.Strings(out)
	return out, nil
}

// tableRows returns the base table's rows as alias.col -> value maps.
func tableRows(s *sqlRows, f fromClause) ([]map[string]string, error) {
	var rows []map[string]string
	if f.table == "node" {
		for i := 0; i < s.nodeN; i++ {
			r := map[string]string{}
			for col, vals := range s.nodeCols {
				r[f.alias+"."+strings.ToLower(col)] = vals[i]
			}
			rows = append(rows, r)
		}
	} else if f.table == "edge" {
		for i := 0; i < s.edgeN; i++ {
			r := map[string]string{}
			for col, vals := range s.edgeCols {
				r[f.alias+"."+strings.ToLower(col)] = vals[i]
			}
			rows = append(rows, r)
		}
	} else {
		// a CTE table: single column
		vals, ok := s.nodeCols["cte_"+f.table]
		if !ok {
			return nil, fmt.Errorf("unknown table %q", f.table)
		}
		for _, v := range vals {
			rows = append(rows, map[string]string{f.alias + ".src": v, f.alias + ".id": v})
		}
	}
	return rows, nil
}

type fromClause struct {
	table string
	alias string
}

type join struct {
	table string
	alias string
	onL, onR string // qualified column refs (lower-cased qual.col)
}

// parseFrom splits "node n JOIN edge e ON ..." into the base clause and joins.
func parseFrom(tablePart string) (fromClause, []join, error) {
	// First table: "node [alias]"
	parts := strings.Fields(tablePart)
	if len(parts) < 1 {
		return fromClause{}, nil, fmt.Errorf("empty FROM")
	}
	fc := fromClause{table: strings.ToLower(parts[0])}
	if len(parts) > 1 && !strings.EqualFold(parts[1], "JOIN") {
		fc.alias = strings.ToLower(parts[1])
	} else {
		fc.alias = fc.table
	}
	if fc.alias == "" {
		fc.alias = fc.table
	}
	// joins: find each "JOIN tbl [alias] ON ..."
	var joins []join
	for _, jm := range joinRe.FindAllStringSubmatch(tablePart, -1) {
		j := join{table: strings.ToLower(jm[1])}
		if jm[2] != "" {
			j.alias = strings.ToLower(jm[2])
		} else {
			j.alias = j.table
		}
		pieces := strings.SplitN(jm[3], "=", 2)
		if len(pieces) != 2 {
			return fc, nil, fmt.Errorf("bad ON in JOIN")
		}
		j.onL, j.onR = strings.ToLower(strings.TrimSpace(pieces[0])), strings.ToLower(strings.TrimSpace(pieces[1]))
		joins = append(joins, j)
	}
	return fc, joins, nil
}

// applyJoin joins work with a new table on the ON equality.
func applyJoin(s *sqlRows, work []map[string]string, j join) []map[string]string {
	other, err := tableRows(s, fromClause{table: j.table, alias: j.alias})
	if err != nil {
		return nil
	}
	// index other rows by the ON column value for speed
	otherByL := map[string][]map[string]string{}
	otherByR := map[string][]map[string]string{}
	for _, o := range other {
		if v, ok := o[j.onL]; ok {
			otherByL[v] = append(otherByL[v], o)
		}
		if v, ok := o[j.onR]; ok {
			otherByR[v] = append(otherByR[v], o)
		}
	}
	var out []map[string]string
	for _, w := range work {
		// determine which ON ref is in w and which is in other
		lInW, rInW := inRow(w, j.onL), inRow(w, j.onR)
		var matches []map[string]string
		if lInW && !rInW {
			matches = otherByR[w[j.onL]]
		} else if rInW && !lInW {
			matches = otherByL[w[j.onR]]
		} else {
			// both or neither in w: fall back to linear scan
			for _, o := range other {
				if (inRow(w, j.onL) && w[j.onL] == o[j.onR]) || (inRow(w, j.onR) && w[j.onR] == o[j.onL]) {
					matches = append(matches, o)
				}
			}
		}
		for _, o := range matches {
			nw := map[string]string{}
			for k, v := range w {
				nw[k] = v
			}
			for k, v := range o {
				nw[k] = v
			}
			out = append(out, nw)
		}
	}
	return out
}

func inRow(r map[string]string, ref string) bool {
	_, ok := r[ref]
	return ok
}

// applyWhere filters rows by a WHERE clause (equality, LIKE, IN, AND/OR).
func applyWhere(work []map[string]string, where string) []map[string]string {
	var out []map[string]string
	for _, w := range work {
		if whereTrue(w, where) {
			out = append(out, w)
		}
	}
	return out
}

// whereTrue evaluates a WHERE clause. Supports AND/OR of simple predicates:
// col = 'x', col LIKE 'x%', col IN ('a','b').
func whereTrue(w map[string]string, where string) bool {
	// split on top-level OR first, then AND
	for _, orPart := range splitTopLevel(where, "OR") {
		if whereANDTrue(w, strings.TrimSpace(orPart)) {
			return true
		}
	}
	return false
}

func whereANDTrue(w map[string]string, s string) bool {
	for _, andPart := range splitTopLevel(s, "AND") {
		if !whereAtomTrue(w, strings.TrimSpace(andPart)) {
			return false
		}
	}
	return true
}

func whereAtomTrue(w map[string]string, s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	up := strings.ToUpper(s)
	// IN list
	if i := strings.Index(up, " IN "); i >= 0 {
		colRef := strings.TrimSpace(s[:i])
		list := s[i+len(" IN "):]
		vals := parseList(list)
		v := colValue(w, colRef)
		for _, x := range vals {
			if v == x {
				return true
			}
		}
		return false
	}
	// LIKE
	if i := strings.Index(up, " LIKE "); i >= 0 {
		colRef := strings.TrimSpace(s[:i])
		like := strings.TrimSpace(s[i+len(" LIKE "):])
		like = strings.Trim(like, "'\"")
		return likeMatch(colValue(w, colRef), like)
	}
	// equality
	if i := strings.Index(s, "="); i >= 0 {
		l := strings.TrimSpace(s[:i])
		r := strings.TrimSpace(s[i+1:])
		lv, rl := isQuoted(l), isQuoted(r)
		if lv && !rl {
			return colValue(w, r) == strings.Trim(l, "'\"")
		}
		if rl && !lv {
			return colValue(w, l) == strings.Trim(r, "'\"")
		}
	}
	return false
}

func parseList(list string) []string {
	list = strings.TrimSpace(list)
	list = strings.Trim(list, "()")
	var out []string
	for _, p := range strings.Split(list, ",") {
		out = append(out, strings.Trim(strings.TrimSpace(p), "'\""))
	}
	return out
}

func likeMatch(s, pat string) bool {
	// convert a SQL LIKE (%,_) to a regexp
	var b strings.Builder
	b.WriteString("^")
	for _, c := range pat {
		switch c {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

func isQuoted(s string) bool {
	return len(s) >= 2 && (s[0] == '\'' || s[0] == '"')
}

func colValue(w map[string]string, ref string) string {
	ref = strings.TrimSpace(ref)
	if isQuoted(ref) {
		return strings.Trim(ref, "'\"")
	}
	qual, col, ok := splitColRef(ref)
	if !ok {
		// bare column
		for k, v := range w {
			if colName(k) == strings.ToLower(col) {
				return v
			}
		}
		return ""
	}
	return w[strings.ToLower(qual)+"."+strings.ToLower(col)]
}

// splitColRef splits "alias.col" into (qual, col); ok is false for a bare col.
func splitColRef(ref string) (qual, col string, ok bool) {
	ref = strings.TrimSpace(ref)
	if i := strings.IndexByte(ref, '.'); i >= 0 {
		return ref[:i], ref[i+1:], true
	}
	return "", ref, false
}

func colName(qualified string) string {
	if i := strings.LastIndexByte(qualified, '.'); i >= 0 {
		return qualified[i+1:]
	}
	return qualified
}
