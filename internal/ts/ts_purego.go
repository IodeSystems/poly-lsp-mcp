//go:build !cgo

package ts

import (
	"context"
	"fmt"
	"sync"

	gts "github.com/odvcencio/gotreesitter"

	"github.com/iodesystems/poly-lsp-mcp/internal/ts/grammars"
)

// Point and Symbol have the same shape in both runtimes, so they alias.
type (
	Point  = gts.Point
	Symbol = gts.Symbol
)

// Runtime names the tree-sitter runtime compiled into this binary.
func Runtime() string { return "pure-go" }

// Language is one grammar. The grammars are smacker's own, converted for
// gotreesitter (see package grammars), so a tree here has the shape the cgo
// runtime gives it; only the parser that builds it differs.
type Language struct {
	l *gts.Language
}

// lang wraps a grammar constructor: lazy, like the grammar decode under it,
// and one *Language per grammar for the process.
func lang(load func() *gts.Language) func() *Language {
	return sync.OnceValue(func() *Language { return &Language{l: load()} })
}

var (
	Go       = lang(grammars.GoLanguage)
	TSX      = lang(grammars.TsxLanguage)
	Python   = lang(grammars.PythonLanguage)
	SQL      = lang(grammars.SqlLanguage)
	Java     = lang(grammars.JavaLanguage)
	C        = lang(grammars.CLanguage)
	Cpp      = lang(grammars.CppLanguage)
	Kotlin   = lang(grammars.KotlinLanguage)
	Groovy   = lang(grammars.GroovyLanguage)
	Markdown = lang(grammars.MarkdownLanguage)
	Protobuf = lang(grammars.ProtoLanguage)
	Swift    = lang(grammars.SwiftLanguage)
)

// SymbolCount is the size of the grammar's symbol table.
func (l *Language) SymbolCount() uint32 { return uint32(len(l.l.SymbolNames)) }

// SymbolName is the type name Node.Type() reports for a symbol. It goes
// through a leaf node's Type so the table matches Type() exactly, including
// gotreesitter's display-name unescaping and the ERROR symbol.
func (l *Language) SymbolName(s Symbol) string {
	return gts.NewLeafNode(s, false, 0, 0, Point{}, Point{}).Type(l.l)
}

// Parser mirrors smacker's: language set after construction, reusable.
type Parser struct {
	lang *Language
	p    *gts.Parser
}

func NewParser() *Parser { return &Parser{} }

func (p *Parser) SetLanguage(lang *Language) {
	if p.lang == lang {
		return
	}
	p.lang = lang
	p.p = gts.NewParser(lang.l)
}

// ParseCtx parses content. Incremental reparsing (oldTree) is not used by this
// module and is not supported here. The context is checked once up front:
// every caller passes context.Background().
func (p *Parser) ParseCtx(ctx context.Context, oldTree *Tree, content []byte) (*Tree, error) {
	if oldTree != nil {
		return nil, fmt.Errorf("ts: incremental parse not supported by the pure-Go runtime")
	}
	if p.p == nil {
		return nil, fmt.Errorf("ts: parser has no language")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t, err := p.p.Parse(content)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("ts: parse returned no tree")
	}
	return &Tree{t: t, lang: p.lang, src: content, cache: map[*gts.Node]*Node{}}, nil
}

func (p *Parser) Close() {}

// Tree owns the node wrappers. Like smacker, it hands out one *Node per
// underlying node, so callers may compare nodes with == as well as Equal.
type Tree struct {
	t     *gts.Tree
	lang  *Language
	src   []byte
	cache map[*gts.Node]*Node
}

func (t *Tree) wrap(n *gts.Node) *Node {
	if n == nil {
		return nil
	}
	if w, ok := t.cache[n]; ok {
		return w
	}
	w := &Node{n: n, t: t}
	t.cache[n] = w
	return w
}

// RootNode is never nil, as with smacker: for input where gotreesitter
// returns no root (empty input, for some grammars) it yields an empty
// ERROR-free leaf so callers can walk it without a nil check.
func (t *Tree) RootNode() *Node {
	if r := t.t.RootNode(); r != nil {
		return t.wrap(r)
	}
	return t.wrap(gts.NewLeafNode(0, true, 0, 0, Point{}, Point{}))
}

// Close releases the tree's arenas. Nodes from it are invalid afterwards,
// exactly as after smacker's ts_tree_delete.
func (t *Tree) Close() {
	t.t.Release()
	t.cache = nil
}

// Node is a gotreesitter node bound to the tree (and so the language) it
// came from, which gotreesitter's Type/ChildByFieldName otherwise need passed.
type Node struct {
	n *gts.Node
	t *Tree
}

func (n *Node) Type() string                   { return n.n.Type(n.t.lang.l) }
func (n *Node) Symbol() Symbol                 { return n.n.Symbol() }
func (n *Node) StartByte() uint32              { return n.n.StartByte() }
func (n *Node) EndByte() uint32                { return n.n.EndByte() }
func (n *Node) StartPoint() Point              { return n.n.StartPoint() }
func (n *Node) EndPoint() Point                { return n.n.EndPoint() }
func (n *Node) IsNamed() bool                  { return n.n.IsNamed() }
func (n *Node) IsMissing() bool                { return n.n.IsMissing() }
func (n *Node) IsError() bool                  { return n.n.IsError() }
func (n *Node) ChildCount() uint32             { return uint32(n.n.ChildCount()) }
func (n *Node) NamedChildCount() uint32        { return uint32(n.n.NamedChildCount()) }
func (n *Node) Child(i int) *Node              { return n.t.wrap(n.n.Child(i)) }
func (n *Node) NamedChild(i int) *Node         { return n.t.wrap(n.n.NamedChild(i)) }
func (n *Node) Parent() *Node                  { return n.t.wrap(n.n.Parent()) }
func (n *Node) NextSibling() *Node             { return n.t.wrap(n.n.NextSibling()) }
func (n *Node) PrevSibling() *Node             { return n.t.wrap(n.n.PrevSibling()) }
func (n *Node) FieldNameForChild(i int) string { return n.n.FieldNameForChild(i, n.t.lang.l) }
func (n *Node) String() string                 { return n.n.SExpr(n.t.lang.l) }

func (n *Node) ChildByFieldName(name string) *Node {
	return n.t.wrap(n.n.ChildByFieldName(name, n.t.lang.l))
}

// HasError matches ts_node_has_error, which counts MISSING nodes as errors;
// gotreesitter's HasError alone does not.
func (n *Node) HasError() bool { return n.n.HasErrorOrMissing() }

// Content slices like smacker's (and panics like it on a foreign buffer),
// where gotreesitter's Text would quietly return "".
func (n *Node) Content(input []byte) string { return string(input[n.StartByte():n.EndByte()]) }

func (n *Node) Equal(other *Node) bool { return other != nil && n.n == other.n }

func (n *Node) NextNamedSibling() *Node {
	s := n.NextSibling()
	for s != nil && !s.IsNamed() {
		s = s.NextSibling()
	}
	return s
}

func (n *Node) PrevNamedSibling() *Node {
	s := n.PrevSibling()
	for s != nil && !s.IsNamed() {
		s = s.PrevSibling()
	}
	return s
}

func (n *Node) NamedDescendantForPointRange(start, end Point) *Node {
	return n.t.wrap(n.n.NamedDescendantForPointRange(start, end))
}

// Query is a compiled pattern. Captures carry an index, as in smacker, into
// the query's capture-name table.
type Query struct {
	q     *gts.Query
	lang  *Language
	index map[string]uint32
}

func NewQuery(pattern []byte, lang *Language) (*Query, error) {
	q, err := gts.NewQuery(string(pattern), lang.l)
	if err != nil {
		return nil, err
	}
	index := map[string]uint32{}
	for i, name := range q.CaptureNames() {
		index[name] = uint32(i)
	}
	return &Query{q: q, lang: lang, index: index}, nil
}

func (q *Query) CaptureNameForId(id uint32) string {
	name, _ := q.q.CaptureNameForID(id)
	return name
}

type QueryCapture struct {
	Index uint32
	Node  *Node
}

type QueryMatch struct {
	PatternIndex uint16
	Captures     []QueryCapture
}

type QueryCursor struct {
	q *Query
	t *Tree
	c *gts.QueryCursor
}

func NewQueryCursor() *QueryCursor { return &QueryCursor{} }

func (qc *QueryCursor) Exec(q *Query, n *Node) {
	qc.q, qc.t = q, n.t
	qc.c = q.q.Exec(n.n, n.t.lang.l, n.t.src)
}

func (qc *QueryCursor) NextMatch() (*QueryMatch, bool) {
	if qc.c == nil {
		return nil, false
	}
	m, ok := qc.c.NextMatch()
	if !ok {
		return nil, false
	}
	out := &QueryMatch{PatternIndex: uint16(m.PatternIndex), Captures: make([]QueryCapture, len(m.Captures))}
	for i, c := range m.Captures {
		out.Captures[i] = QueryCapture{Index: qc.q.index[c.Name], Node: qc.t.wrap(c.Node)}
	}
	return out, true
}

func (qc *QueryCursor) Close() {}
