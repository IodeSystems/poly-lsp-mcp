package symbols

import (
	"context"
	"sync"

	sitter "github.com/smacker/go-tree-sitter"
)

// TreeSitterExtractor runs a single compiled tree-sitter query against
// each file's syntax tree and returns the captured tokens as Hits.
//
// Compared to LexicalExtractor, this drops identifier-shaped tokens
// that live inside string literals, comments, and keywords — the grammar
// classifies those as non-identifier nodes so the query never matches
// them. The Keywords map is still consulted because some grammars
// surface builtin types (Go's `int64`, `string`) as type-identifier
// nodes; filtering those is a per-language choice.
type TreeSitterExtractor struct {
	lang     *sitter.Language
	query    *sitter.Query
	keywords map[string]struct{}

	pool sync.Pool // *sitter.Parser
}

// NewTreeSitterExtractor compiles the query against the language. The
// query string typically unions every node type the language uses for
// identifier-like names (Go uses identifier / field_identifier /
// type_identifier / package_identifier).
func NewTreeSitterExtractor(lang *sitter.Language, query string, keywords map[string]struct{}) (*TreeSitterExtractor, error) {
	q, err := sitter.NewQuery([]byte(query), lang)
	if err != nil {
		return nil, err
	}
	e := &TreeSitterExtractor{
		lang:     lang,
		query:    q,
		keywords: keywords,
	}
	e.pool.New = func() any {
		p := sitter.NewParser()
		p.SetLanguage(lang)
		return p
	}
	return e, nil
}

func (e *TreeSitterExtractor) Extract(content []byte) []Hit {
	parser := e.pool.Get().(*sitter.Parser)
	defer e.pool.Put(parser)

	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil || tree == nil {
		return nil
	}
	defer tree.Close()

	cur := sitter.NewQueryCursor()
	defer cur.Close()
	cur.Exec(e.query, tree.RootNode())

	var hits []Hit
	for {
		m, ok := cur.NextMatch()
		if !ok {
			break
		}
		for _, c := range m.Captures {
			name := c.Node.Content(content)
			if _, kw := e.keywords[name]; kw {
				continue
			}
			pt := c.Node.StartPoint()
			hits = append(hits, Hit{
				Name: name,
				Line: int(pt.Row) + 1,
				Col:  int(pt.Column) + 1,
			})
		}
	}
	return hits
}

// mustTreeSitterExtractor is used during package init for queries that
// are compile-time constants — a panic here would mean a typo in our
// own source, not user input.
func mustTreeSitterExtractor(lang *sitter.Language, query string, keywords map[string]struct{}) Extractor {
	e, err := NewTreeSitterExtractor(lang, query, keywords)
	if err != nil {
		panic("symbols: bad tree-sitter query: " + err.Error())
	}
	return e
}
