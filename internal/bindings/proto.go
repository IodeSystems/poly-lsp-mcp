package bindings

import "regexp"

// Proto schema extraction. Tier 3 reads protobuf files declared in
// tslsmcp.yaml under `schemas:` and treats each named entity as a
// binding. The parser is intentionally regex-based — it covers the
// declarations real codebases actually use without pulling a full
// proto compiler into our dep graph.
//
// Supported declarations:
//   - message <Name> {
//   - enum <Name> {
//   - service <Name> {
//   - rpc <Name>(
//
// Anchors `^\s*` and a trailing `{` or `(` make the patterns specific
// enough to skip mentions in comments, in strings, or as field types.
// Edge cases the parser will MISS:
//   - inline `message Foo { message Bar { ... } }` nested forms where
//     the inner declaration shares a line with the outer brace (rare
//     in practice; canonical proto style puts each on its own line).
//   - oneof / map fields (these are field declarations, not types).
//
// Future swap: tree-sitter-protobuf via smacker; current parser is the
// no-dep MVP.

var (
	protoMessageRe = regexp.MustCompile(`(?m)^\s*message\s+(\w+)\s*\{`)
	protoEnumRe    = regexp.MustCompile(`(?m)^\s*enum\s+(\w+)\s*\{`)
	protoServiceRe = regexp.MustCompile(`(?m)^\s*service\s+(\w+)\s*\{`)
	protoRpcRe     = regexp.MustCompile(`(?m)^\s*rpc\s+(\w+)\s*\(`)
)

// parseProto extracts every named entity from a proto file. Positions
// point at the entity's name token (not the keyword), 1-based, byte
// offset within line — matches symbols.Site conventions.
func parseProto(content []byte) []SchemaEntity {
	nl := newlineOffsets(content)
	var out []SchemaEntity
	for _, re := range []*regexp.Regexp{protoMessageRe, protoEnumRe, protoServiceRe, protoRpcRe} {
		for _, m := range re.FindAllSubmatchIndex(content, -1) {
			start, end := m[2], m[3]
			line, col := offsetToLineCol(start, nl)
			out = append(out, SchemaEntity{
				Name: string(content[start:end]),
				Line: line,
				Col:  col,
			})
		}
	}
	return out
}
