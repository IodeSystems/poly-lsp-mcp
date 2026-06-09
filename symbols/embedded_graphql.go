package symbols

import (
	"context"
	"regexp"
	"sync"

	sitter "github.com/smacker/go-tree-sitter"
)

// embeddedGraphQLExtractor wraps a base TS/TSX extractor and ALSO surfaces the
// identifier tokens inside GraphQL documents embedded in template literals —
// graphql(`...`) and gql`...` (the graphql-codegen / graphql-request pattern). The
// TS grammar treats a template body as opaque string content, so a GraphQL field
// reference such as `eventCreate` inside graphql(`{ ... eventCreate ... }`) is
// invisible to identifier-node extraction — which made cross-language rename of a
// gat/codegen op (Go OperationID → SDL field → TS query) miss its most important
// site. We scan the tagged template body the same way a .graphql file is scanned
// (lexical, high-recall) and map each token to its byte-accurate position in the
// .ts/.tsx file, so references/rename reach it.
type embeddedGraphQLExtractor struct {
	base  Extractor
	query *sitter.Query
	pool  sync.Pool
}

var (
	gqlTemplateTags = map[string]struct{}{"graphql": {}, "gql": {}}
	gqlEmbedIdentRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
)

func newEmbeddedGraphQLExtractor(base Extractor, lang *sitter.Language) *embeddedGraphQLExtractor {
	q, err := sitter.NewQuery([]byte(`(template_string) @doc`), lang)
	if err != nil {
		panic("symbols: bad embedded-graphql query: " + err.Error())
	}
	e := &embeddedGraphQLExtractor{base: base, query: q}
	e.pool.New = func() any {
		p := sitter.NewParser()
		p.SetLanguage(lang)
		return p
	}
	return e
}

func (e *embeddedGraphQLExtractor) Extract(content []byte) []Hit {
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

	var lineStarts []int // computed lazily — most files have no gql templates
	for {
		m, ok := cur.NextMatch()
		if !ok {
			break
		}
		for _, c := range m.Captures {
			doc := c.Node
			if !isGraphQLTagged(doc, content) {
				continue
			}
			if lineStarts == nil {
				lineStarts = computeLineStarts(content)
			}
			start := int(doc.StartByte())
			// Includes the backticks; the token regex never matches them, and any
			// ${FragmentDoc} interpolation yields a real reference we want.
			body := content[doc.StartByte():doc.EndByte()]
			for _, loc := range gqlEmbedIdentRe.FindAllIndex(body, -1) {
				line, col := byteToLineCol(lineStarts, start+loc[0])
				hits = append(hits, Hit{Name: string(body[loc[0]:loc[1]]), Line: line, Col: col})
			}
		}
	}
	return hits
}

// isGraphQLTagged reports whether a template_string is the body of a graphql(`...`)
// call or a gql`...` tagged template. graphql(`...`) wraps the string in an
// `arguments` node; gql`...` makes it the call's direct argument.
func isGraphQLTagged(doc *sitter.Node, content []byte) bool {
	call := doc.Parent()
	if call != nil && call.Type() == "arguments" {
		call = call.Parent()
	}
	if call == nil || call.Type() != "call_expression" {
		return false
	}
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return false
	}
	_, ok := gqlTemplateTags[fn.Content(content)]
	return ok
}

// computeLineStarts returns the byte offset at which each line begins (line 1 at
// index 0), for mapping an absolute byte offset to a 1-based (line, col).
func computeLineStarts(content []byte) []int {
	starts := []int{0}
	for i, b := range content {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// byteToLineCol maps an absolute byte offset to a 1-based line and 1-based byte
// column within that line (matching tree-sitter StartPoint, which the other
// extractors use).
func byteToLineCol(lineStarts []int, off int) (int, int) {
	lo, hi := 0, len(lineStarts)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if lineStarts[mid] <= off {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo + 1, off - lineStarts[lo] + 1
}
