//go:build cgo

package ts

import (
	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/c"
	"github.com/smacker/go-tree-sitter/cpp"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/groovy"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/kotlin"
	markdown "github.com/smacker/go-tree-sitter/markdown/tree-sitter-markdown"
	"github.com/smacker/go-tree-sitter/protobuf"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/sql"
	"github.com/smacker/go-tree-sitter/swift"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
)

// Aliases, not wrappers: with cgo the adapter must add nothing, so the C path
// stays exactly what it was before the adapter existed.
type (
	Node         = sitter.Node
	Tree         = sitter.Tree
	Parser       = sitter.Parser
	Language     = sitter.Language
	Query        = sitter.Query
	QueryCursor  = sitter.QueryCursor
	QueryMatch   = sitter.QueryMatch
	QueryCapture = sitter.QueryCapture
	Point        = sitter.Point
	Symbol       = sitter.Symbol
)

// Runtime names the tree-sitter runtime compiled into this binary.
func Runtime() string { return "cgo" }

func NewParser() *Parser                                      { return sitter.NewParser() }
func NewQuery(pattern []byte, lang *Language) (*Query, error) { return sitter.NewQuery(pattern, lang) }
func NewQueryCursor() *QueryCursor                            { return sitter.NewQueryCursor() }

func Go() *Language       { return golang.GetLanguage() }
func TSX() *Language      { return tsx.GetLanguage() }
func Python() *Language   { return python.GetLanguage() }
func SQL() *Language      { return sql.GetLanguage() }
func Java() *Language     { return java.GetLanguage() }
func C() *Language        { return c.GetLanguage() }
func Cpp() *Language      { return cpp.GetLanguage() }
func Kotlin() *Language   { return kotlin.GetLanguage() }
func Groovy() *Language   { return groovy.GetLanguage() }
func Markdown() *Language { return markdown.GetLanguage() }
func Protobuf() *Language { return protobuf.GetLanguage() }
func Swift() *Language    { return swift.GetLanguage() }
