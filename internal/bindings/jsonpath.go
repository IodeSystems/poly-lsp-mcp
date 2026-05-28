package bindings

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// JSONPath subset supported by v0.2.1:
//
//   $.foo            — root object key
//   $.foo.bar        — nested object key
//   $.foo[0]         — array index (0-based)
//   $.foo[*].bar     — wildcard over array OR over map values
//
// Leading "$" is optional ("foo.bar" works as shorthand for "$.foo.bar").
// Recursive descent ("..foo"), filters ("[?(@.x==1)]"), slices ("[1:3]"),
// and quoted keys ("['x.y']") are not supported; the parser rejects them.
//
// Why this subset: it covers the common cases for "this YAML/JSON value
// is part of binding X" without pulling in a full JSONPath dependency.
// String-literal cross-language references — the value-add of Tier 2 —
// only need to be locatable by simple paths in practice.

type segment interface{ jsonpathSegment() }

type keySegment struct{ Key string }
type indexSegment struct{ Index int }
type wildcardSegment struct{}

func (keySegment) jsonpathSegment()      {}
func (indexSegment) jsonpathSegment()    {}
func (wildcardSegment) jsonpathSegment() {}

// parsePath turns a JSONPath expression into a flat list of segments.
// An empty path is an error; the empty list of segments means "root",
// which is not currently a meaningful site (use a key segment).
func parsePath(p string) ([]segment, error) {
	if p == "" {
		return nil, fmt.Errorf("empty jsonpath")
	}
	p = strings.TrimPrefix(p, "$")
	// Normalize so the cursor logic only handles '.' and '[' as starts.
	if p != "" && p[0] != '.' && p[0] != '[' {
		p = "." + p
	}
	if p == "" {
		return nil, fmt.Errorf("jsonpath has no segments")
	}

	var out []segment
	for i := 0; i < len(p); {
		switch p[i] {
		case '.':
			if i+1 < len(p) && p[i+1] == '.' {
				return nil, fmt.Errorf("recursive descent (..) not supported")
			}
			i++
			if i >= len(p) {
				return nil, fmt.Errorf("trailing '.' in jsonpath")
			}
			j := i
			for j < len(p) && p[j] != '.' && p[j] != '[' {
				j++
			}
			if j == i {
				return nil, fmt.Errorf("empty key in jsonpath at offset %d", i)
			}
			out = append(out, keySegment{Key: p[i:j]})
			i = j
		case '[':
			j := i + 1
			for j < len(p) && p[j] != ']' {
				j++
			}
			if j >= len(p) {
				return nil, fmt.Errorf("unclosed '[' in jsonpath")
			}
			tok := strings.TrimSpace(p[i+1 : j])
			switch {
			case tok == "*":
				out = append(out, wildcardSegment{})
			case strings.HasPrefix(tok, "'") || strings.HasPrefix(tok, `"`):
				return nil, fmt.Errorf("quoted keys not supported in jsonpath")
			case strings.Contains(tok, ":"):
				return nil, fmt.Errorf("array slices not supported in jsonpath")
			default:
				n, err := strconv.Atoi(tok)
				if err != nil {
					return nil, fmt.Errorf("bad array index %q", tok)
				}
				if n < 0 {
					return nil, fmt.Errorf("negative array index %d", n)
				}
				out = append(out, indexSegment{Index: n})
			}
			i = j + 1
		default:
			return nil, fmt.Errorf("unexpected %q at offset %d", p[i], i)
		}
	}
	return out, nil
}

// position is a 1-based (line, column) pair pointing at a node in the
// source document. Matches yaml.Node.Line/Column conventions and lines
// up with symbols.Site.
type position struct {
	Line int
	Col  int
}

// evalYAMLJSON parses content as YAML — which is a superset of JSON, so
// a single code path handles both — and returns the positions of every
// node matching path. Empty result means the path matched nothing.
func evalYAMLJSON(content []byte, path []segment) ([]position, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, err
	}
	var root *yaml.Node
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil, nil
		}
		root = doc.Content[0]
	} else {
		root = &doc
	}
	return walk([]*yaml.Node{root}, path), nil
}

func walk(nodes []*yaml.Node, path []segment) []position {
	if len(path) == 0 {
		out := make([]position, 0, len(nodes))
		for _, n := range nodes {
			out = append(out, position{Line: n.Line, Col: n.Column})
		}
		return out
	}
	seg := path[0]
	rest := path[1:]
	var next []*yaml.Node
	for _, n := range nodes {
		switch s := seg.(type) {
		case keySegment:
			if n.Kind != yaml.MappingNode {
				continue
			}
			// MappingNode content alternates key, value, key, value …
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i]
				if k.Kind == yaml.ScalarNode && k.Value == s.Key {
					next = append(next, n.Content[i+1])
				}
			}
		case indexSegment:
			if n.Kind != yaml.SequenceNode {
				continue
			}
			if s.Index >= 0 && s.Index < len(n.Content) {
				next = append(next, n.Content[s.Index])
			}
		case wildcardSegment:
			switch n.Kind {
			case yaml.SequenceNode:
				next = append(next, n.Content...)
			case yaml.MappingNode:
				for i := 1; i < len(n.Content); i += 2 {
					next = append(next, n.Content[i])
				}
			}
		}
	}
	return walk(next, rest)
}
