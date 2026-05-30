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

// StructureNode describes one top-level construct in a file: its
// tree-sitter node type, an extracted name if the grammar exposes one
// via a "name" field or an *_identifier descendant, and the 1-based
// end-exclusive range it occupies.
//
// Range convention matches the rest of internal/mcp's range-based
// tools: line and column are 1-based, end is exclusive (so a single
// character at line 1 col 1 has range startLine=1 startCol=1
// endLine=1 endCol=2).
type StructureNode struct {
	Type      string `json:"type"`
	Name      string `json:"name,omitempty"`
	StartLine int    `json:"startLine"`
	StartCol  int    `json:"startCol"`
	EndLine   int    `json:"endLine"`
	EndCol    int    `json:"endCol"`
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
		out = append(out, StructureNode{
			Type:      child.Type(),
			Name:      extractStructureName(child, content),
			StartLine: int(sp.Row) + 1,
			StartCol:  int(sp.Column) + 1,
			EndLine:   int(ep.Row) + 1,
			EndCol:    int(ep.Column) + 1,
		})
	}
	return out, nil
}

// extractStructureName tries to find a name token for a top-level
// declaration. Preferences in order:
//
//  1. A direct `name` field (most languages' function_declaration etc.)
//  2. The first *_identifier descendant within a small depth budget,
//     for declarations that wrap the name a level deeper —
//     type_declaration -> type_spec -> type_identifier in Go, or
//     export_statement -> {function,class,type_alias}_declaration ->
//     {identifier,type_identifier} in TypeScript.
//
// Depth is bounded so we don't descend into function bodies and pick
// up some random local identifier.
func extractStructureName(node *sitter.Node, content []byte) string {
	if name := node.ChildByFieldName("name"); name != nil {
		return name.Content(content)
	}
	return findIdentDescendant(node, content, 3)
}

func findIdentDescendant(node *sitter.Node, content []byte, depth int) string {
	if depth <= 0 || node == nil {
		return ""
	}
	count := int(node.NamedChildCount())
	for i := range count {
		c := node.NamedChild(i)
		switch c.Type() {
		case "identifier", "type_identifier", "field_identifier",
			"package_identifier", "property_identifier":
			return c.Content(content)
		}
	}
	// Second pass: descend into wrapper nodes (export_statement,
	// type_spec, etc.) one level at a time. ChildByFieldName("name")
	// on the inner node often resolves before we recurse, hence the
	// pref-name short-circuit.
	for i := range count {
		c := node.NamedChild(i)
		if name := c.ChildByFieldName("name"); name != nil {
			return name.Content(content)
		}
		if name := findIdentDescendant(c, content, depth-1); name != "" {
			return name
		}
	}
	return ""
}
