package symbols

import (
	"context"
	"testing"

	sitter "github.com/smacker/go-tree-sitter"
)

// nodeType must agree with Type() exactly — it is a performance substitution,
// so any disagreement is a silent behaviour change in the classifier.
func TestNodeTypeMatchesType(t *testing.T) {
	src := []byte("package p\n\nimport \"fmt\"\n\ntype T struct{ X int }\n\nfunc (t T) M(a int) (string, error) {\n\tfmt.Println(a)\n\treturn \"\", nil\n}\n")
	p := sitter.NewParser()
	p.SetLanguage(LanguageByName("go"))
	tree, err := p.ParseCtx(context.Background(), nil, src)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()

	seen := 0
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if got, want := nodeType(typeTable("go"), n), n.Type(); got != want {
			t.Fatalf("nodeType = %q, Type() = %q", got, want)
		}
		seen++
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}
	walk(tree.RootNode())
	if seen < 20 {
		t.Fatalf("fixture drift: only %d nodes walked", seen)
	}
	// An unknown language yields no table and must not panic.
	if typeTable("nosuchlang") != nil {
		t.Error("an unknown language should have no table")
	}
}

func BenchmarkNodeTypeVsType(b *testing.B) {
	src := benchSource(20)
	p := sitter.NewParser()
	p.SetLanguage(LanguageByName("go"))
	tree, _ := p.ParseCtx(context.Background(), nil, src)
	defer tree.Close()
	var nodes []*sitter.Node
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		nodes = append(nodes, n)
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}
	walk(tree.RootNode())

	b.Run("Type", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, n := range nodes {
				_ = n.Type()
			}
		}
	})
	b.Run("nodeType", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, n := range nodes {
				_ = nodeType(typeTable("go"), n)
			}
		}
	})
	tbl := typeTable("go")
	b.Run("nodeType_tableHoisted", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, n := range nodes {
				if s := int(n.Symbol()); s < len(tbl) {
					_ = tbl[s]
				}
			}
		}
	})
}
