package symbols

import (
	"sync"

	sitter "github.com/smacker/go-tree-sitter"
)

// nodeType returns a node's grammar type WITHOUT allocating.
//
// sitter.Node.Type() is C.GoString(ts_node_type(...)) — a fresh Go string on
// every call. The symbol walk calls it several times per node across dozens of
// sites, and it measured 65% of the walk's allocations (404,139 objects for
// one 35KB file), which is more than everything this package does put
// together.
//
// The names are not dynamic: a grammar has a fixed symbol table, a few hundred
// entries, and ts_node_symbol returns an index into it for free. So the table
// is materialized ONCE per language and indexed thereafter. Same strings, no
// garbage.
//
// Falls back to Type() for a symbol outside the table — a grammar version
// mismatch would be the only way there, and being slow is better than being
// wrong.
// Takes the TABLE, not the language name. Resolving the table per call cost
// more than the allocation it saved — measured 367ns/node against Type()'s
// 337ns, because a sync.Map lookup is not free. Hoisted to one lookup per
// function it is 301ns AND allocation-free, which is the whole point.
func nodeType(tbl []string, n *sitter.Node) string {
	if s := int(n.Symbol()); tbl != nil && s < len(tbl) {
		if name := tbl[s]; name != "" {
			return name
		}
	}
	return n.Type()
}

var typeTables sync.Map // language name -> []string

// typeTable builds (once) the symbol-id → name table for a grammar.
func typeTable(language string) []string {
	if v, ok := typeTables.Load(language); ok {
		tbl, _ := v.([]string)
		return tbl
	}
	lang := LanguageByName(language)
	if lang == nil {
		typeTables.Store(language, []string(nil))
		return nil
	}
	n := int(lang.SymbolCount())
	tbl := make([]string, n)
	for i := 0; i < n; i++ {
		tbl[i] = lang.SymbolName(sitter.Symbol(i))
	}
	typeTables.Store(language, tbl)
	return tbl
}
