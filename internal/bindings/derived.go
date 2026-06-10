package bindings

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// ApplyDerived reads gat-emitted `@derived(operationId: "X")` directives from the
// workspace's GraphQL SDL and binds each generated field to its Go source — the
// `OperationID: "X"` string literal gat derived it from. These are DECLARED edges
// (the generator stated them), the authoritative replacement for guessing the
// field↔OperationID mapping by replicating gat's naming rule. Returns the count of
// declared sites added.
func (r *Resolver) ApplyDerived(idx *symbols.Index) int {
	// 1. operationIds the SDL declares as derivation sources.
	want := map[string]bool{}
	walkFiles(r.root, func(path string, data []byte) {
		if !hasSuffix(path, ".graphql", ".gql") {
			return
		}
		for _, m := range derivedRe.FindAllSubmatch(data, -1) {
			want[string(m[1])] = true
		}
	})
	if len(want) == 0 {
		return 0
	}

	// 2. find the Go OperationID literal for each + register it as a declared site.
	added := 0
	walkFiles(r.root, func(path string, data []byte) {
		if !hasSuffix(path, ".go") {
			return
		}
		for _, h := range goOperationIDSites(data) {
			if !want[h.value] {
				continue
			}
			idx.InsertDeclared(h.value, path, "go", h.line, h.col)
			added++
		}
	})
	return added
}

var derivedRe = regexp.MustCompile(`@derived\(operationId:\s*"([^"]+)"\)`)

func walkFiles(root string, fn func(path string, data []byte)) {
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if n := d.Name(); n == "node_modules" || (strings.HasPrefix(n, ".") && n != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if data, rerr := os.ReadFile(path); rerr == nil {
			fn(path, data)
		}
		return nil
	})
}

func hasSuffix(path string, suffixes ...string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(path, s) {
			return true
		}
	}
	return false
}

type opIDHit struct {
	value     string
	line, col int
}

var goOpIDQuery = mustGoQuery(`(keyed_element
    (literal_element (identifier) @key)
    (literal_element (interpreted_string_literal) @val))`)

// goOperationIDSites returns each `OperationID: "X"` struct-field literal, pointing
// at the content position (past the opening quote) so a rename targets the value.
func goOperationIDSites(content []byte) []opIDHit {
	p := sitter.NewParser()
	p.SetLanguage(golang.GetLanguage())
	tree, err := p.ParseCtx(context.Background(), nil, content)
	if err != nil || tree == nil {
		return nil
	}
	defer tree.Close()
	cur := sitter.NewQueryCursor()
	defer cur.Close()
	cur.Exec(goOpIDQuery, tree.RootNode())

	var out []opIDHit
	for {
		m, ok := cur.NextMatch()
		if !ok {
			break
		}
		var key, val *sitter.Node
		for _, c := range m.Captures {
			switch goOpIDQuery.CaptureNameForId(c.Index) {
			case "key":
				key = c.Node
			case "val":
				val = c.Node
			}
		}
		if key == nil || val == nil || key.Content(content) != "OperationID" {
			continue
		}
		name := strings.Trim(val.Content(content), "\"`")
		if name == "" {
			continue
		}
		pt := val.StartPoint()
		out = append(out, opIDHit{value: name, line: int(pt.Row) + 1, col: int(pt.Column) + 2})
	}
	return out
}

func mustGoQuery(q string) *sitter.Query {
	query, err := sitter.NewQuery([]byte(q), golang.GetLanguage())
	if err != nil {
		panic("bindings: bad go query: " + err.Error())
	}
	return query
}
