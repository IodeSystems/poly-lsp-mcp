package symbols

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sitter "github.com/smacker/go-tree-sitter"
)

// Benchmarks for the layer a CPU profile keeps landing in: a cold node_query
// spends ~83% of its time in runtime.cgocall, split ~64% tree-sitter ParseCtx
// and ~26% walking nodes. Those two have very different fixes — parsing is
// memoizable, walking is not — so they are measured apart rather than
// together.
//
// Fixtures are GENERATED, not read from the repo, so a number means the same
// thing next month. Sizes are powers of ten apart to make the scaling visible.

// benchSource builds a Go file with n top-level funcs, each with a doc
// comment, params and a body — the shape the extractor actually walks.
func benchSource(n int) []byte {
	var b strings.Builder
	b.WriteString("package bench\n\nimport \"fmt\"\n\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `
// Fn%d does a thing worth documenting, at a length real doc comments reach.
type T%d struct {
	Name string
	Size int
}

func (t T%d) Method%d(ctx string, retries int) (string, error) {
	if retries > 0 {
		fmt.Println(ctx, t.Name)
	}
	return t.Name, nil
}

func Fn%d(a, b int) int {
	total := a + b
	for i := 0; i < b; i++ {
		total += i
	}
	return total
}
`, i, i, i, i, i)
	}
	return []byte(b.String())
}

// FileSymbols is parse + walk together — what every tree build pays per file.
func BenchmarkFileSymbols(b *testing.B) {
	for _, n := range []int{10, 100} {
		src := benchSource(n)
		b.Run(fmt.Sprintf("decls=%d", n*3), func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := FileSymbols("go", src); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// ParseCtx alone: the tree-sitter parse with NO walking. FileSymbols minus
// this is the walk — the ~26% whose only real fix is batching the traversal
// in C, which is a large change and wants evidence first.
func BenchmarkParseOnly(b *testing.B) {
	lang := LanguageByName("go")
	for _, n := range []int{10, 100} {
		src := benchSource(n)
		b.Run(fmt.Sprintf("decls=%d", n*3), func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				p := sitter.NewParser()
				p.SetLanguage(lang)
				tree, err := p.ParseCtx(context.Background(), nil, src)
				if err != nil {
					b.Fatal(err)
				}
				tree.Close()
			}
		})
	}
}

// The parser is constructed per call throughout the codebase. This is what
// pooling one would save — measured so the question is answerable rather than
// assumed.
func BenchmarkParserConstruction(b *testing.B) {
	lang := LanguageByName("go")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p := sitter.NewParser()
		p.SetLanguage(lang)
	}
}

// The memo that makes a warm query cheap: a hit must be orders of magnitude
// under a parse, or it is not worth the memory.
func BenchmarkSymbolCache(b *testing.B) {
	src := benchSource(100)
	syms, err := FileSymbols("go", src)
	if err != nil {
		b.Fatal(err)
	}
	c := NewSymbolCache()
	c.Put("go", src, syms)

	b.Run("hit", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, ok := c.Get("go", src); !ok {
				b.Fatal("expected a hit")
			}
		}
	})
	// A miss costs the hash and nothing else — the parse is the caller's.
	other := benchSource(99)
	b.Run("miss", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, ok := c.Get("go", other); ok {
				b.Fatal("expected a miss")
			}
		}
	})
}
