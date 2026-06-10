package bindings

import "github.com/iodesystems/poly-lsp-mcp/symbols"

// DerivRoot is an authoritative derivation source a @derived resolver registered:
// the symbol Name, the generator Kind that declared the edge, and the Source site —
// the underlying definition a rename's `underlying` mode targets. node_refactor uses
// the set of roots to decide whether a rename touches a derivation graph (and so must
// resolve a mode) vs a plain symbol.
type DerivRoot struct {
	Name   string
	Kind   string // "gat-operation" | "sqlc-column"
	Source symbols.Site
}
