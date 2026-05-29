package bindings

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// JSONSchema extraction. Three places named schemas live in real-world
// JSONSchema documents:
//
//   1. `$defs.<Name>` — Draft 2019-09 and later (the modern form).
//   2. `definitions.<Name>` — Draft 4 through 7 (still common in older
//      tooling output: jsonschema-pydantic, justjson, etc.).
//   3. Top-level `title` — when a document is a single schema, its
//      title often matches the generated type name in the target
//      language SDK.
//
// We deliberately don't pull in:
//   - `properties.<Name>` keys — those are property names, not schema
//     names; including them would explode the binding namespace.
//   - Nested `$defs` inside another `$defs` value — keeps the parser
//     simple and matches how real codegen tools treat the file.
//   - `$id` URL parsing — overengineering for the value it adds.

// parseJSONSchema walks the document and returns one SchemaEntity per
// named schema, with line/col pointing at the name token in the source.
// Works on both JSON and YAML JSONSchema documents since JSON is a
// strict subset of YAML.
func parseJSONSchema(content []byte) ([]SchemaEntity, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("parse jsonschema: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("jsonschema: root is not a mapping")
	}

	var out []SchemaEntity

	if defs := findMappingValue(root, "$defs"); defs != nil && defs.Kind == yaml.MappingNode {
		out = append(out, mappingKeys(defs)...)
	}
	if defs := findMappingValue(root, "definitions"); defs != nil && defs.Kind == yaml.MappingNode {
		out = append(out, mappingKeys(defs)...)
	}
	if title := findMappingValue(root, "title"); title != nil && title.Kind == yaml.ScalarNode && title.Value != "" {
		out = append(out, SchemaEntity{Name: title.Value, Line: title.Line, Col: title.Column})
	}

	return out, nil
}

// mappingKeys returns a SchemaEntity for every key in a mapping node.
// Used by both openapi and jsonschema parsers — same pattern, same code.
func mappingKeys(m *yaml.Node) []SchemaEntity {
	out := make([]SchemaEntity, 0, len(m.Content)/2)
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i]
		if k.Kind == yaml.ScalarNode {
			out = append(out, SchemaEntity{Name: k.Value, Line: k.Line, Col: k.Column})
		}
	}
	return out
}
