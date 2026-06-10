package bindings

import (
	"regexp"
	"strings"

	"github.com/iodesystems/poly-lsp-mcp/internal/migrations"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// ApplyDerivedSQL is the SQL half of @derived consumption, symmetric to ApplyDerived
// (gat). The sqlc fork tags each generated Go field `derived:"table.column"` — the
// declared edge to its schema source. This reads those tags, folds the migrations to
// find where each referenced column is actually defined, and registers that migration
// site as the DECLARED (authoritative) source for the column name. A rename/refactor
// of the column then reaches its migration source of truth (the `underlying` target)
// instead of guessing, with the query/db-tag references surfaced as lexical candidates.
//
// Columns are keyed by bare name (the form queries and db: tags use); same-named
// columns across tables share a key — a known limitation of the name-keyed index.
func (r *Resolver) ApplyDerivedSQL(idx *symbols.Index) []DerivRoot {
	// 1. table.column edges the sqlc generator declared.
	want := map[string]bool{}
	walkFiles(r.root, func(path string, data []byte) {
		if !hasSuffix(path, ".go") {
			return
		}
		for _, m := range derivedTagRe.FindAllSubmatch(data, -1) {
			want[string(m[1])] = true
		}
	})
	if len(want) == 0 {
		return nil
	}

	// 2. fold the migrations → each column's current defining site.
	sc := migrations.Fold(r.root)

	// 3. register the migration site of every derived-from column as declared.
	var roots []DerivRoot
	for tc := range want {
		tbl, col, ok := strings.Cut(tc, ".")
		if !ok {
			continue
		}
		t := sc.Tables[tbl]
		if t == nil {
			continue
		}
		for _, c := range t.Columns {
			if c.Name == col && c.DefinedAt.File != "" {
				idx.InsertDeclared(c.Name, c.DefinedAt.File, "sql", c.DefinedAt.Line, c.DefinedAt.Col)
				roots = append(roots, DerivRoot{Name: c.Name, Kind: "sqlc-column", Source: symbols.Site{
					File: c.DefinedAt.File, Line: c.DefinedAt.Line, Col: c.DefinedAt.Col,
					Language: "sql", Confidence: symbols.ConfidenceDeclared,
				}})
				break
			}
		}
	}
	return roots
}

var derivedTagRe = regexp.MustCompile(`derived:"([^"]+)"`)
