package symbols

import (
	"context"
	"fmt"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/sql"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
)

// LanguageByName returns the tree-sitter language object for one of our
// supported code languages, or nil for languages we only handle via the
// lexical extractor (markdown, yaml, json). Used by document_structure
// and any future caller that needs to parse a file's syntax tree.
func LanguageByName(name string) *sitter.Language {
	switch name {
	case "go":
		return golang.GetLanguage()
	case "typescript":
		return tsx.GetLanguage()
	case "python":
		return python.GetLanguage()
	case "sql":
		return sql.GetLanguage()
	}
	return nil
}

// StructureNode describes one top-level construct in a file. Two
// ranges live on the same node by design:
//
//   - Range  — the whole declaration. Pass to node_read / node_edit /
//     node_refactor (when the refactor wants to replace the whole
//     thing).
//   - NameRange — just the identifier within the declaration, if the
//     grammar exposes a name. Pass to node_references and to
//     node_refactor with kind="rename" so the operation pins on the
//     name token, not the surrounding declaration.
//
// Both are 1-based, end-exclusive (a single character at line 1 col 1
// occupies startLine=1 startCol=1 endLine=1 endCol=2).
type StructureNode struct {
	Type           string `json:"type"`
	Name           string `json:"name,omitempty"`
	StartLine      int    `json:"startLine"`
	StartCol       int    `json:"startCol"`
	EndLine        int    `json:"endLine"`
	EndCol         int    `json:"endCol"`
	NameStartLine  int    `json:"nameStartLine,omitempty"`
	NameStartCol   int    `json:"nameStartCol,omitempty"`
	NameEndLine    int    `json:"nameEndLine,omitempty"`
	NameEndCol     int    `json:"nameEndCol,omitempty"`
}

// StructureNodes parses content with the given language's grammar and
// returns the named children of the root node — typically one entry
// per top-level declaration (functions, types, classes, imports).
// Errors when the language has no tree-sitter grammar wired up
// (markdown, yaml, json — those use the lexical extractor and have no
// useful syntactic structure to surface).
func StructureNodes(language string, content []byte) ([]StructureNode, error) {
	lang := LanguageByName(language)
	if lang == nil {
		return nil, fmt.Errorf("no tree-sitter grammar for language %q", language)
	}
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if tree == nil {
		return nil, fmt.Errorf("parse returned nil tree")
	}
	defer tree.Close()

	root := tree.RootNode()
	count := int(root.NamedChildCount())
	out := make([]StructureNode, 0, count)
	for i := range count {
		child := root.NamedChild(i)
		sp := child.StartPoint()
		ep := child.EndPoint()
		entry := StructureNode{
			Type:      child.Type(),
			StartLine: int(sp.Row) + 1,
			StartCol:  int(sp.Column) + 1,
			EndLine:   int(ep.Row) + 1,
			EndCol:    int(ep.Column) + 1,
		}
		if nameNode := findStructureNameNode(child); nameNode != nil {
			entry.Name = nameNode.Content(content)
			nsp := nameNode.StartPoint()
			nep := nameNode.EndPoint()
			entry.NameStartLine = int(nsp.Row) + 1
			entry.NameStartCol = int(nsp.Column) + 1
			entry.NameEndLine = int(nep.Row) + 1
			entry.NameEndCol = int(nep.Column) + 1
		}
		out = append(out, entry)
	}
	return out, nil
}

// findStructureNameNode returns the identifier node that names a
// top-level declaration, or nil if the grammar doesn't expose one.
// Preferences in order:
//
//  1. A direct `name` field (most function_declaration etc. have one).
//  2. A direct *_identifier child.
//  3. The same lookups one level deeper, for declarations that wrap
//     the name an extra layer — type_declaration → type_spec →
//     type_identifier in Go, or export_statement →
//     {function,class,type_alias}_declaration →
//     {identifier,type_identifier} in TypeScript.
//
// Depth is bounded so we don't descend into function bodies and pick
// up some random local identifier.
func findStructureNameNode(node *sitter.Node) *sitter.Node {
	return findNameNodeDepth(node, 3)
}

func findNameNodeDepth(node *sitter.Node, depth int) *sitter.Node {
	if depth <= 0 || node == nil {
		return nil
	}
	if name := node.ChildByFieldName("name"); name != nil {
		return name
	}
	count := int(node.NamedChildCount())
	for i := range count {
		c := node.NamedChild(i)
		switch c.Type() {
		case "identifier", "type_identifier", "field_identifier",
			"package_identifier", "property_identifier":
			return c
		}
	}
	for i := range count {
		c := node.NamedChild(i)
		if found := findNameNodeDepth(c, depth-1); found != nil {
			return found
		}
	}
	return nil
}
