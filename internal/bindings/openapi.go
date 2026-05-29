package bindings

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// OpenAPI schema extraction. We pull two kinds of named entities:
//
//   1. Type names declared under `components.schemas.<Name>` (OpenAPI
//      3.x) or `definitions.<Name>` (older Swagger 2.0). These map to
//      generated types in language SDKs.
//   2. `operationId` values inside path operations. These map to
//      generated method names.
//
// We deliberately don't extract:
//   - Path parameter names (highly variable; many spurious matches).
//   - Schema `properties` keys (would explode the binding namespace).
//   - `$ref` targets (those are downstream uses of declared names, and
//     the lexical extractor over the YAML file already indexes the
//     literal text — ApplySchemas promotes those to declared via the
//     workspace-hit pass).
//
// File can be YAML or JSON; gopkg.in/yaml.v3 handles both since JSON is
// a strict subset of YAML.

// parseOpenAPI walks the document and returns one SchemaEntity per
// extracted name with the line/col of the name token in the source.
func parseOpenAPI(content []byte) ([]SchemaEntity, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("parse openapi: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("openapi: root is not a mapping")
	}

	var out []SchemaEntity

	// components.schemas — modern OpenAPI 3.x.
	if schemas := findMappingValue(root, "components", "schemas"); schemas != nil && schemas.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(schemas.Content); i += 2 {
			k := schemas.Content[i]
			if k.Kind == yaml.ScalarNode {
				out = append(out, SchemaEntity{Name: k.Value, Line: k.Line, Col: k.Column})
			}
		}
	}

	// definitions — Swagger 2.0 fallback.
	if defs := findMappingValue(root, "definitions"); defs != nil && defs.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(defs.Content); i += 2 {
			k := defs.Content[i]
			if k.Kind == yaml.ScalarNode {
				out = append(out, SchemaEntity{Name: k.Value, Line: k.Line, Col: k.Column})
			}
		}
	}

	// paths.<path>.<method>.operationId — every operation's id.
	if paths := findMappingValue(root, "paths"); paths != nil && paths.Kind == yaml.MappingNode {
		// Iterate path values; for each, iterate method values; pull
		// operationId scalar.
		for i := 1; i < len(paths.Content); i += 2 {
			pathItem := paths.Content[i]
			if pathItem.Kind != yaml.MappingNode {
				continue
			}
			for j := 1; j < len(pathItem.Content); j += 2 {
				op := pathItem.Content[j]
				if op.Kind != yaml.MappingNode {
					continue
				}
				if id := findMappingValue(op, "operationId"); id != nil && id.Kind == yaml.ScalarNode {
					out = append(out, SchemaEntity{Name: id.Value, Line: id.Line, Col: id.Column})
				}
			}
		}
	}

	return out, nil
}

// findMappingValue walks a mapping node by string keys and returns the
// value node at the end, or nil if any hop misses or hits the wrong
// node kind. Variadic so callers read top-down: `findMappingValue(root,
// "components", "schemas")` mirrors the dotted YAML path.
func findMappingValue(node *yaml.Node, keys ...string) *yaml.Node {
	cur := node
	for _, key := range keys {
		if cur == nil || cur.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(cur.Content); i += 2 {
			k := cur.Content[i]
			if k.Kind == yaml.ScalarNode && k.Value == key {
				next = cur.Content[i+1]
				break
			}
		}
		if next == nil {
			return nil
		}
		cur = next
	}
	return cur
}
