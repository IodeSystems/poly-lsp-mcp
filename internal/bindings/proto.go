package bindings

import (
	"context"
	"sync"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/protobuf"
)

// Proto schema extraction. Tier 3 reads protobuf files declared in
// poly-lsp-mcp.yaml under `schemas:` and treats each named entity as a
// binding.
//
// Implementation: tree-sitter-protobuf via smacker/go-tree-sitter. The
// grammar exposes one node kind per declaration kind:
//
//   (message  (message_name (identifier)))
//   (enum     (enum_name    (identifier)))
//   (service  (service_name (identifier)))
//   (rpc      (rpc_name     (identifier)))
//
// One query captures every name token at once. Nested declarations
// (e.g. `message Outer { message Inner { … } }`) work the same way
// the outer ones do — the grammar surfaces them recursively, which the
// previous regex MVP missed.

// protoQuery captures every named declaration's identifier token. The
// query is compile-time constant so we panic on a bad expression at
// package init via initProtoQuery rather than per file.
const protoQuery = `[
  (message  (message_name  (identifier) @name))
  (enum     (enum_name     (identifier) @name))
  (service  (service_name  (identifier) @name))
  (rpc      (rpc_name      (identifier) @name))
]`

var (
	protoCompiled    *sitter.Query
	protoCompileOnce sync.Once
	protoParserPool  sync.Pool
)

func initProtoQuery() {
	q, err := sitter.NewQuery([]byte(protoQuery), protobuf.GetLanguage())
	if err != nil {
		panic("bindings: bad proto tree-sitter query: " + err.Error())
	}
	protoCompiled = q
	protoParserPool.New = func() any {
		p := sitter.NewParser()
		p.SetLanguage(protobuf.GetLanguage())
		return p
	}
}

// parseProto extracts every named entity from a proto file. Positions
// point at the entity's name token (not the keyword), 1-based, byte
// offset within line — matches symbols.Site conventions and the shape
// the rest of internal/bindings expects.
func parseProto(content []byte) []SchemaEntity {
	protoCompileOnce.Do(initProtoQuery)

	parser := protoParserPool.Get().(*sitter.Parser)
	defer protoParserPool.Put(parser)

	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil || tree == nil {
		return nil
	}
	defer tree.Close()

	cur := sitter.NewQueryCursor()
	defer cur.Close()
	cur.Exec(protoCompiled, tree.RootNode())

	var out []SchemaEntity
	for {
		m, ok := cur.NextMatch()
		if !ok {
			break
		}
		for _, c := range m.Captures {
			pt := c.Node.StartPoint()
			out = append(out, SchemaEntity{
				Name: c.Node.Content(content),
				Line: int(pt.Row) + 1,
				Col:  int(pt.Column) + 1,
			})
		}
	}
	return out
}
