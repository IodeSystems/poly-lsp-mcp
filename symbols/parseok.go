package symbols

import (
	"context"

	sitter "github.com/smacker/go-tree-sitter"
)

// ParsesCleanly reports whether content parses with no ERROR or MISSING node
// anywhere in the tree.
//
// FileSymbols cannot answer this: tree-sitter is error-TOLERANT by design, so
// it recovers and returns a symbol list for input that is not valid source at
// all — which is exactly how a merge-conflicted file yields a symbol table
// containing declarations from both sides at once. Callers that need to know
// whether a reconstruction is trustworthy need the tree's own verdict, not
// the fact that symbols came back.
//
// false for a language with no grammar: nothing was verified, so nothing may
// be claimed.
func ParsesCleanly(language string, content []byte) bool {
	lang := LanguageByName(language)
	if lang == nil {
		return false
	}
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil || tree == nil {
		return false
	}
	defer tree.Close()
	return !tree.RootNode().HasError()
}
