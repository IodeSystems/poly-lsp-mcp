package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Cypher-subset executor, in-process over the shared node/edge rows.
//
// The supported subset (documented so it matches the committed Cypher card):
//
//	MATCH (a)-[:TYPE*1..n]->(b)   // one pattern; TYPE is a relationship type
//	MATCH (a:Label)-[:TYPE]->(b)  // optional node label
//	MATCH (#'a.go#Save')-[:CALLS]->(b)  // a quoted node id anchors an endpoint
//	RETURN [DISTINCT] b           // one or more variables
//
//   - Relationship TYPE: the model writes CALLS / TYPES / IMPORTS (the card's
//     names); they map to the row kinds call / type / import.
//   - Node Label: Func / Method / Type / Struct / Interface / Class / Const /
//     Var / Field / Enum / Ctor / Module / Import / Argument / Annotation /
//     Return. A labelled node is filtered to rows whose kind matches.
//   - A quoted id ('a.go#Save') anchors that endpoint to one specific node.
//   - Variable length: *1..n bounds the hops (n >= 1); a bare :TYPE is one hop.
//   - RETURN: the variable(s) to output; DISTINCT de-duplicates. The answer
//     set is the returned variable's node ID(s) (canonical "file#Name").
//
// This is deliberately small: a hand-written evaluator over the rows, NO
// external Cypher engine (the spec forbids one). It is a pure function of the
// rows, so it is unit-testable with canned rows and no model.
func execCypher(rows *Rows, query string) ([]string, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("empty Cypher query")
	}
	matchPat, retPat, err := splitClauses(q)
	if err != nil {
		return nil, err
	}
	pats, err := parsePatterns(matchPat)
	if err != nil {
		return nil, err
	}
	retVars, distinct, err := parseReturn(retPat)
	if err != nil {
		return nil, err
	}
	if len(retVars) == 0 {
		return nil, fmt.Errorf("RETURN needs at least one variable")
	}

	byID := rows.nodeByID()
	labelFilter := func(label string) map[string]bool {
		if label == "" {
			return nil
		}
		return map[string]bool{cypherLabelToKind(label): true}
	}

	seen := map[string]bool{}
	var out []string
	emit := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}

	for _, p := range pats {
		bindings := evalPattern(rows, p, byID, labelFilter)
		for _, v := range retVars {
			for _, id := range bindings[v] {
				emit(id)
			}
		}
	}
	if distinct {
		out = dedupeStr(out)
	}
	sort.Strings(out)
	return out, nil
}

// pattern is one parsed MATCH element: (a)-[:TYPE*1..n]->(b).
type pattern struct {
	from, to     string
	fromLabel    string
	toLabel      string
	fromID, toID string
	relType      string // "call"|"type"|"import"
	minHop, maxHop int
}

// evalPattern returns a map from variable name to the set of node IDs that
// variable is bound to for every row path matching the pattern.
// Single-hop walks one edge; variable-length walks 1..maxHop edges of the kind.
func evalPattern(rows *Rows, p pattern, byID map[string]Node, labelFilter func(string) map[string]bool) map[string][]string {
	bindings := map[string][]string{}
	addTo := func(v, id string) {
		if id == "" {
			return
		}
		for _, x := range bindings[v] {
			if x == id {
				return
			}
		}
		bindings[v] = append(bindings[v], id)
	}
	fk := labelFilter(p.fromLabel)
	tk := labelFilter(p.toLabel)
	matchesFrom := func(id string) bool {
		if p.fromID != "" {
			return id == p.fromID
		}
		return fk == nil || (byID[id].Kind != "" && fk[byID[id].Kind])
	}
	matchesTo := func(id string) bool {
		if p.toID != "" {
			return id == p.toID
		}
		return tk == nil || (byID[id].Kind != "" && tk[byID[id].Kind])
	}

	if p.maxHop <= 1 {
		for _, e := range rows.Edges {
			if e.Kind != p.relType {
				continue
			}
			if !matchesFrom(e.From) || !matchesTo(e.To) {
				continue
			}
			if p.from != "" {
				addTo(p.from, e.From)
			}
			if p.to != "" {
				addTo(p.to, e.To)
			}
		}
		return bindings
	}
	// Variable-length: BFS up to maxHop edges of the given kind. An
	// Variable-length (a)-[:TYPE*1..n]->(b): walk BACKWARD from the
	// endpoints that match the `to` constraint. An unanchored `a` is bound
	// only to nodes that reach a matching end in 1..n hops of the kind —
	// (a)-[:CALLS*1..3]->(b:#'Save') yields the nodes that transitively
	// reach Save, not every node.
	var ends []string
	if p.toID != "" {
		ends = []string{p.toID}
	} else {
		for id := range byID {
			if matchesTo(id) {
				ends = append(ends, id)
			}
		}
	}
	for _, id := range ends {
		if p.to != "" {
			addTo(p.to, id)
		}
	}
	if len(ends) == 0 {
		return bindings
	}
	reach := map[string]bool{}
	frontier := ends
	for hop := 1; hop <= p.maxHop; hop++ {
		var next []string
		for _, cur := range frontier {
			for _, e := range rows.Edges {
				if e.To != cur || e.Kind != p.relType {
					continue
				}
				if reach[e.From] {
					continue
				}
				reach[e.From] = true
				next = append(next, e.From)
			}
		}
		frontier = next
		if len(frontier) == 0 {
			break
		}
	}
	for id := range reach {
		if p.from == "" || matchesFrom(id) {
			if p.from != "" {
				addTo(p.from, id)
			}
		}
	}
	return bindings
}

// cypherPatRe matches one pattern: (a)-[:TYPE*1..n]->(b). Each side is a
// plain variable (a), a labelled variable (a:Label), or an anchored id
// (#'a.go#Save'). The anchored form is how a question pins one endpoint to a
// specific node ("the callers of Save" pins b to #'store.go#Save').
// cypherPatRe matches one pattern: (a)-[:TYPE*1..n]->(b). Each side is one
// of: (a) a bare variable, (a:Label) a labelled variable, (#'a.go#Save') an
// anchored id, or (b:#'a.go#Save') a variable bound to a specific node.
// Group layout: 1 fromVar, 2 fromAnchor/Label, 3 fromLabel, 4 relType,
// 5 varLen, 6 minHop, 7 maxHop, 8 toVar, 9 toAnchor/Label, 10 toLabel.
var cypherPatRe = regexp.MustCompile(`\((\w+)?(?::(#'[^']*'|(\w+)))?\)-\[:([A-Za-z_]+)(\*(\d+)\.\.(\d+))?\]->\((\w+)?(?::(#'[^']*'|(\w+)))?\)`)

// splitClauses splits a query into its MATCH and RETURN clauses.
func splitClauses(q string) (match, ret string, err error) {
	upper := strings.ToUpper(q)
	mi := strings.Index(upper, "MATCH")
	ri := strings.Index(upper, "RETURN")
	if mi < 0 || ri < 0 || ri < mi {
		return "", "", fmt.Errorf("query needs a MATCH clause and a RETURN clause")
	}
	return strings.TrimSpace(q[mi+len("MATCH"):ri]), strings.TrimSpace(q[ri+len("RETURN"):]), nil
}

// parsePatterns finds every pattern in the MATCH clause.
func parsePatterns(match string) ([]pattern, error) {
	locs := cypherPatRe.FindAllStringSubmatchIndex(match, -1)
	if len(locs) == 0 {
		return nil, fmt.Errorf("no pattern like (a)-[:TYPE]->(b) in MATCH clause")
	}
	var out []pattern
	for _, l := range locs {
		g := func(i int) string {
			if l[2*i] < 0 {
				return ""
			}
			return match[l[2*i]:l[2*i+1]]
		}
		// Group layout: 1 from, 2 fromID, 3 fromLabel, 4 relType,
		// 5 varLen (the *n..n), 6 min, 7 max, 8 to, 9 toID, 10 toLabel.
		p := pattern{
			from: g(1),
			relType: relTypeToKind(g(4)),
			to: g(8),
			minHop: 1, maxHop: 1,
		}
		// The colon value (g2/g9) is an anchored id (#'file#Name') or a label.
		if v := g(2); v != "" {
			if strings.HasPrefix(v, "#'") {
				p.fromID = strings.TrimSuffix(strings.TrimPrefix(v, "#'"), "'")
			} else {
				p.fromLabel = v
			}
		}
		if v := g(9); v != "" {
			if strings.HasPrefix(v, "#'") {
				p.toID = strings.TrimSuffix(strings.TrimPrefix(v, "#'"), "'")
			} else {
				p.toLabel = v
			}
		}
		if l[2*5] >= 0 { // variable length present: g(6)=min, g(7)=max
			p.minHop = atoiSafe(g(6), 1)
			p.maxHop = atoiSafe(g(7), 3)
			if p.maxHop < p.minHop {
				p.maxHop = p.minHop
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// parseReturn parses the RETURN clause into variables and a DISTINCT flag.
func parseReturn(ret string) (vars []string, distinct bool, err error) {
	upper := strings.ToUpper(ret)
	if strings.HasPrefix(upper, "DISTINCT ") {
		distinct = true
		ret = strings.TrimSpace(ret[len("DISTINCT "):])
	}
	for _, v := range strings.Split(ret, ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if !isIdent(v) {
			return nil, false, fmt.Errorf("RETURN supports bare variables only, got %q", v)
		}
		vars = append(vars, v)
	}
	return vars, distinct, nil
}

// relTypeToKind maps the card's relationship names to the row kinds.
func relTypeToKind(t string) string {
	switch strings.ToUpper(t) {
	case "CALLS", "CALL":
		return "call"
	case "TYPES", "TYPE":
		return "type"
	case "IMPORTS", "IMPORT":
		return "import"
	default:
		return strings.ToLower(t)
	}
}

// cypherLabelToKind maps a node label to the row kind it filters on.
func cypherLabelToKind(label string) string {
	switch strings.ToLower(label) {
	case "func", "function":
		return "func"
	default:
		return strings.ToLower(label)
	}
}

func atoiSafe(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return def
	}
	return n
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if i == 0 {
			if !isIdentStart(c) {
				return false
			}
			continue
		}
		if !isIdentPart(c) {
			return false
		}
	}
	return true
}

func isIdentStart(c rune) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c rune) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func dedupeStr(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
