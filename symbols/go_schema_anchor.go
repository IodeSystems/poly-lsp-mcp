package symbols

import (
	"context"
	"strings"
	"sync"

	sitter "github.com/smacker/go-tree-sitter"
)

// goSchemaAnchorExtractor wraps the Go identifier extractor and ALSO indexes the
// string VALUE of schema-anchor struct fields — chiefly huma/gat `OperationID: "x"`.
// That string is the name the operation surfaces as across the generated API surface
// (the gat GraphQL field, the OpenAPI operationId, the gRPC method). Without it the
// codegen SOURCE of a field is a bare Go string literal — not an identifier node — so
// a cross-language rename of the field can't reach the registration that REGENERATES
// it, and the rename silently reverts on the next codegen run.
type goSchemaAnchorExtractor struct {
	base  Extractor
	query *sitter.Query
	pool  sync.Pool
}

// goSchemaAnchorKeys: struct-field names whose string value names a schema entity.
// OperationID is huma/gat's operation id (becomes the gat GraphQL field name).
var goSchemaAnchorKeys = map[string]struct{}{"OperationID": {}}

func newGoSchemaAnchorExtractor(base Extractor, lang *sitter.Language) *goSchemaAnchorExtractor {
	const q = `(keyed_element
        (literal_element (identifier) @key)
        (literal_element (interpreted_string_literal) @val))`
	query, err := sitter.NewQuery([]byte(q), lang)
	if err != nil {
		panic("symbols: bad go-schema-anchor query: " + err.Error())
	}
	e := &goSchemaAnchorExtractor{base: base, query: query}
	e.pool.New = func() any {
		p := sitter.NewParser()
		p.SetLanguage(lang)
		return p
	}
	return e
}

func (e *goSchemaAnchorExtractor) Extract(content []byte) []Hit {
	hits := e.base.Extract(content)

	parser := e.pool.Get().(*sitter.Parser)
	defer e.pool.Put(parser)
	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil || tree == nil {
		return hits
	}
	defer tree.Close()

	cur := sitter.NewQueryCursor()
	defer cur.Close()
	cur.Exec(e.query, tree.RootNode())

	for {
		m, ok := cur.NextMatch()
		if !ok {
			break
		}
		var key, val *sitter.Node
		for _, c := range m.Captures {
			switch e.query.CaptureNameForId(c.Index) {
			case "key":
				key = c.Node
			case "val":
				val = c.Node
			}
		}
		if key == nil || val == nil {
			continue
		}
		if _, ok := goSchemaAnchorKeys[key.Content(content)]; !ok {
			continue
		}
		name := strings.Trim(val.Content(content), "\"`")
		if name == "" {
			continue
		}
		pt := val.StartPoint()
		hits = append(hits, Hit{
			Name: name,
			Line: int(pt.Row) + 1,
			Col:  int(pt.Column) + 2, // +1 for 1-based, +1 to skip the opening quote → points at the content
		})
	}
	return hits
}
