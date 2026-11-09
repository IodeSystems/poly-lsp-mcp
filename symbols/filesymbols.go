package symbols

import "github.com/iodesystems/poly-lsp-mcp/segment"

// Symbol is one named construct in a file. It is segment's walk result; see
// segment.Symbol for the field contract.
type Symbol = segment.Symbol

// FileSymbols parses content with the language's tree-sitter grammar and
// returns a FLAT, source-ordered list of every symbol (top-level and
// nested). The walk lives in package segment, so segment.Symbols and this
// cannot diverge.
//
// XML has no vendored grammar (and the html grammar mis-parses Android XML:
// dotted tag names split, entities in strings.xml error out), so it gets a
// purpose-built encoding/xml walk instead. Any other language without a
// grammar returns an error; callers handle those with a single whole-file
// entry.
func FileSymbols(language string, content []byte) ([]Symbol, error) {
	if language == "xml" {
		return XMLFileSymbols(content), nil
	}
	return segment.Walk(language, content)
}
