package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// APIRow is one declaration in the API summary view.
type APIRow struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	Head      string `json:"head"`
	Doc       string `json:"doc"`
	FieldType string `json:"fieldType,omitempty"`
}

// QueryAPI evaluates a selector and renders one row per exported declaration:
// kind, name, owner type, head (declaration without body), doc text, and for
// struct fields the field type text. Multi-name fields produce one row per
// name. Interface methods appear with their signatures.
//
// The selector determines which nodes to include. For "file.go > *", all
// top-level symbols are included, and their children (struct fields, interface
// methods) are included as well.
func (s *Server) QueryAPI(selector string, limit, offset int, budget string, asJSON bool, w io.Writer, include ...string) error {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return fmt.Errorf("selector is required")
	}
	list, err := parseModernSelector(selector)
	if err != nil {
		return err
	}
	if s.getIndex() == nil {
		if err := s.BuildIndex(); err != nil {
			return err
		}
	}
	e, err := s.buildTreeFor(selector, include...)
	if err != nil {
		return err
	}
	if v, unit, ok := parseBudget(budget); ok {
		e.setBudget(v, unit)
	}
	nodes := e.evaluate(list)

	// Collect symbol nodes: for each matched node, if it's a file, take its
	// symbol children; if it's a symbol, take it and all its symbol
	// descendants (so struct fields and interface methods are included).
	var syms []*treeNode
	var collectSym func(n *treeNode)
	collectSym = func(n *treeNode) {
		if isSymbolClass(n.class) {
			syms = append(syms, n)
		}
		for _, c := range n.children {
			collectSym(c)
		}
	}
	for _, n := range nodes {
		collectSym(n)
	}

	// Build rows, filtering to exported names.
	var rows []APIRow
	seen := map[string]bool{}
	for _, n := range syms {
		if n.leaf == "" {
			continue
		}
		// Read file content for head and doc extraction.
		content, err := os.ReadFile(n.abs)
		if err != nil {
			continue
		}
		lines := strings.Split(string(content), "\n")

		// Doc text: prefer commentAt span; fall back to scanning up from
		// at[0] for consecutive comment lines (the walk doesn't always set
		// commentAt, e.g. for interface types).
		doc := ""
		if n.commentAt[0] > 0 && n.commentAt[1] >= n.commentAt[0] {
			var docLines []string
			for i := n.commentAt[0] - 1; i <= n.commentAt[1]-1 && i < len(lines); i++ {
				docLines = append(docLines, strings.TrimSpace(stripCommentMarker(lines[i])))
			}
			doc = strings.Join(docLines, " ")
		} else {
			// Walk up from at[0] collecting comment lines.
			i := n.at[0] - 1 // 0-based index of the line before the decl
			var docLines []string
			for i >= 0 && i < len(lines) {
				line := strings.TrimSpace(lines[i])
				if strings.HasPrefix(line, "//") {
					docLines = append([]string{strings.TrimSpace(line[2:])}, docLines...)
					i--
				} else if strings.HasPrefix(line, "/*") {
					// Block comment: collect until */
					j := i
					for j < len(lines) {
						docLines = append([]string{strings.TrimSpace(stripCommentMarker(lines[j]))}, docLines...)
						if strings.Contains(lines[j], "*/") {
							break
						}
						j++
					}
					break
				} else {
					break
				}
			}
			doc = strings.Join(docLines, " ")
		}

		// Head: declaration text without body.
		head := declHead(n, lines)

		// For fields: split multi-name and extract field type.
		if n.class == "field" {
			names, fieldType := splitFieldNames(head)
			if len(names) == 0 {
				names = []string{n.leaf}
			}
			for _, name := range names {
				if !isExported(name) {
					continue
				}
				key := n.sym + "/" + name
				if seen[key] {
					continue
				}
				seen[key] = true
				rows = append(rows, APIRow{
					Kind:      n.class,
					Name:      name,
					Owner:     ownerName(n),
					Head:      head,
					Doc:       doc,
					FieldType: fieldType,
				})
			}
		} else {
			if !isExported(n.leaf) {
				continue
			}
			key := n.sym
			if seen[key] {
				continue
			}
			seen[key] = true
			rows = append(rows, APIRow{
				Kind:  n.class,
				Name:  n.leaf,
				Owner: ownerName(n),
				Head:  head,
				Doc:   doc,
			})
		}
	}

	// Pagination.
	total := len(rows)
	if offset > 0 {
		if offset >= total {
			rows = nil
		} else {
			rows = rows[offset:]
		}
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}

	if asJSON {
		doc := map[string]any{
			"total": total,
			"rows":  rows,
		}
		if rows == nil {
			doc["rows"] = []any{}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(doc)
	}

	// Text render: one row per line.
	for _, r := range rows {
		line := fmt.Sprintf("%-8s %-20s %s", r.Kind, r.Name, r.Head)
		if r.Owner != "" {
			line = fmt.Sprintf("%-8s %-20s %s.%s", r.Kind, r.Owner, r.Name, r.Head)
		}
		if r.FieldType != "" {
			line += " : " + r.FieldType
		}
		if r.Doc != "" {
			line += "  // " + r.Doc
		}
		fmt.Fprintln(w, line)
	}
	return nil
}

// isSymbolClass reports whether a tree node class is a declaration we render
// in the API view.
func isSymbolClass(c string) bool {
	switch c {
	case "func", "method", "field", "type", "struct", "interface",
		"const", "var", "class", "enum":
		return true
	}
	return false
}

// isExported reports whether a name is exported (starts with uppercase).
func isExported(name string) bool {
	if name == "" {
		return false
	}
	return name[0] >= 'A' && name[0] <= 'Z'
}

// ownerName returns the owner type name for a node, or "" for top-level
// declarations.
func ownerName(n *treeNode) string {
	if n.parent == nil {
		return ""
	}
	// Walk up to find the nearest type/struct/interface owner.
	for p := n.parent; p != nil; p = p.parent {
		switch p.class {
		case "struct", "interface", "class", "enum", "type":
			return p.leaf
		case "file":
			return ""
		}
	}
	return ""
}

// declHead returns the declaration text without the body. Comment lines are
// skipped (the doc text is carried separately in APIRow.Doc). For a function
// or method, the head ends at the body's opening brace. For a field, type,
// or other declaration, it is the non-comment declaration lines up to (and
// including) the line with the opening brace, or the full decl span if there
// is no body.
func declHead(n *treeNode, lines []string) string {
	start := n.at[0] // 1-based line
	if start < 1 {
		start = 1
	}
	end := n.at[1]
	if end > len(lines) {
		end = len(lines)
	}
	// If bodyAt is set and is within the decl span, the body starts there.
	// For a one-line function, bodyAt == at[0] (same line); we cut at the
	// opening brace on that line.
	if n.bodyAt > 0 && n.bodyAt <= end {
		if n.bodyAt == start {
			// One-line: cut at the first depth-0 brace on the line.
			line := lines[start-1]
			cut := findBodyBrace(line)
			if cut > 0 {
				return strings.TrimSpace(line[:cut])
			}
			// No brace found: use the whole line.
			return strings.TrimSpace(line)
		}
		// Multi-line: include non-comment lines up to bodyAt-1, then the
		// opening brace on bodyAt (if any) to close the signature.
		var parts []string
		for i := start; i < n.bodyAt && i <= end; i++ {
			line := strings.TrimSpace(lines[i-1])
			if isCommentLine(line) {
				continue
			}
			parts = append(parts, line)
		}
		// Include the body-start line up to its opening brace.
		if n.bodyAt <= end {
			bl := strings.TrimSpace(lines[n.bodyAt-1])
			if !isCommentLine(bl) {
				cut := findBodyBrace(bl)
				if cut > 0 {
					parts = append(parts, bl[:cut])
				} else {
					parts = append(parts, bl)
				}
			}
		}
		return strings.Join(parts, " ")
	}
	// No body: the full decl span, skipping comment lines.
	var parts []string
	for i := start; i <= end; i++ {
		line := strings.TrimSpace(lines[i-1])
		if isCommentLine(line) {
			continue
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, " ")
}

// isCommentLine reports whether a trimmed line is a pure comment line.
func isCommentLine(s string) bool {
	return strings.HasPrefix(s, "//") || strings.HasPrefix(s, "/*") || strings.HasPrefix(s, "*")
}

// findBodyBrace returns the byte offset of the first depth-0 '{' in s,
// tracking paren/bracket depth so a '{' inside () or [] is not the body.
// Returns -1 if no depth-0 brace is found.
func findBodyBrace(s string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '[':
			depth++
		case ')', ']':
			if depth > 0 {
				depth--
			}
		case '{':
			if depth == 0 {
				return i
			}
			depth++
		}
	}
	return -1
}

// stripCommentMarker removes a leading // or /* or * comment marker from a
// line, returning the remainder.
func stripCommentMarker(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "//") {
		return strings.TrimSpace(s[2:])
	}
	if strings.HasPrefix(s, "/*") {
		s = s[2:]
		if idx := strings.Index(s, "*/"); idx >= 0 {
			s = s[:idx]
		}
		return strings.TrimSpace(s)
	}
	if strings.HasPrefix(s, "*") {
		return strings.TrimSpace(s[1:])
	}
	return s
}

// splitFieldNames parses a field head like "W, H float64" into the names
// [W H] and the type text "float64". For a single-name field "Name string",
// it returns [Name] and "string".
func splitFieldNames(head string) ([]string, string) {
	head = strings.TrimSpace(head)
	// Find the last space that separates names from type. The type is
	// everything after the last space that is not inside a composite type.
	// Simple heuristic: the type starts at the last space before a word
	// that is not a comma-separated name.
	space := strings.LastIndex(head, " ")
	if space < 0 {
		return []string{head}, ""
	}
	namePart := strings.TrimSpace(head[:space])
	typePart := strings.TrimSpace(head[space+1:])
	// If the "type" part contains a comma, it's not a type — the whole
	// thing is names.
	if strings.Contains(typePart, ",") {
		names := strings.Split(namePart+", "+typePart, ",")
		for i := range names {
			names[i] = strings.TrimSpace(names[i])
		}
		return names, ""
	}
	names := strings.Split(namePart, ",")
	for i := range names {
		names[i] = strings.TrimSpace(names[i])
	}
	return names, typePart
}
