package ts

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/*.tree from the cgo runtime")

var samples = map[string]func() *Language{
	"sample.go":     Go,
	"sample.tsx":    TSX,
	"sample.py":     Python,
	"sample.sql":    SQL,
	"sample.java":   Java,
	"sample.c":      C,
	"sample.cpp":    Cpp,
	"sample.kt":     Kotlin,
	"sample.groovy": Groovy,
	"sample.md":     Markdown,
	"sample.proto":  Protobuf,
}

func parse(t *testing.T, lang *Language, src []byte) *Tree {
	t.Helper()
	p := NewParser()
	p.SetLanguage(lang)
	tree, err := p.ParseCtx(context.Background(), nil, src)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// dump renders a tree through the adapter surface the call sites use: type,
// field, namedness, byte range, and leaf text.
func dump(n *Node, src []byte, depth int, sb *strings.Builder) {
	field := ""
	if p := n.Parent(); p != nil {
		for i := range int(p.ChildCount()) {
			if p.Child(i) == n {
				if f := p.FieldNameForChild(i); f != "" {
					field = f + ": "
				}
			}
		}
	}
	text := ""
	if n.ChildCount() == 0 {
		text = fmt.Sprintf(" %q", n.Content(src))
	}
	fmt.Fprintf(sb, "%s%s%s named=%v [%d-%d]%s\n", strings.Repeat("  ", depth), field, n.Type(), n.IsNamed(), n.StartByte(), n.EndByte(), text)
	for i := range int(n.ChildCount()) {
		dump(n.Child(i), src, depth+1, sb)
	}
}

// The pure-Go runtime parses with smacker's own grammars (package grammars),
// so on valid source it must build the C runtime's tree exactly. The golden
// trees are the cgo runtime's; the samples exercise every external scanner
// (sql dollar quotes, kotlin strings and semicolons, tsx templates and
// ternaries, python indentation, cpp raw strings, markdown blocks).
func TestTreesMatchTheCRuntime(t *testing.T) {
	for name, lang := range samples {
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			tree := parse(t, lang(), src)
			defer tree.Close()
			if tree.RootNode().HasError() {
				t.Errorf("%s does not parse cleanly", name)
			}
			var sb strings.Builder
			dump(tree.RootNode(), src, 0, &sb)
			golden := filepath.Join("testdata", name+".tree")
			if *update {
				if Runtime() != "cgo" {
					t.Fatal("-update writes the C runtime's trees; run it with CGO_ENABLED=1")
				}
				if err := os.WriteFile(golden, []byte(sb.String()), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got := sb.String(); got != string(want) {
				t.Errorf("%s: tree differs from the C runtime's (%s)\ngot:\n%s", name, golden, got)
			}
		})
	}
}

func TestQueryCapturesByName(t *testing.T) {
	src := []byte("package p\n\nfunc A() {}\nfunc B() {}\n")
	tree := parse(t, Go(), src)
	defer tree.Close()
	q, err := NewQuery([]byte(`(function_declaration name: (identifier) @fn body: (block) @body)`), Go())
	if err != nil {
		t.Fatal(err)
	}
	cur := NewQueryCursor()
	defer cur.Close()
	cur.Exec(q, tree.RootNode())
	var got []string
	for {
		m, ok := cur.NextMatch()
		if !ok {
			break
		}
		for _, c := range m.Captures {
			got = append(got, q.CaptureNameForId(c.Index)+"="+c.Node.Content(src))
		}
	}
	if want := "fn=A body={} fn=B body={}"; strings.Join(got, " ") != want {
		t.Errorf("captures = %q, want %q", strings.Join(got, " "), want)
	}
}

// Call sites compare nodes with == as well as Equal (smacker caches one
// *Node per tree node); the pure-Go side must keep that identity.
func TestNodeIdentity(t *testing.T) {
	src := []byte("package p\n\nfunc A() {}\n")
	tree := parse(t, Go(), src)
	defer tree.Close()
	fn := tree.RootNode().NamedChild(1)
	name := fn.ChildByFieldName("name")
	if name.Parent() != fn || !name.Parent().Equal(fn) {
		t.Error("a node's parent is not the node it came from")
	}
	if fn.NamedChild(0) != name {
		t.Error("the same child came back as two nodes")
	}
	pt := name.StartPoint()
	if d := tree.RootNode().NamedDescendantForPointRange(pt, pt); d != name {
		t.Errorf("descendant at %v = %v, want the name", pt, d.Type())
	}
	if prev := fn.PrevNamedSibling(); prev == nil || prev.Type() != "package_clause" {
		t.Error("PrevNamedSibling did not reach the package clause")
	}
}

func TestSymbolNameMatchesType(t *testing.T) {
	src := []byte("package p\n\nfunc A(x int) {}\n")
	tree := parse(t, Go(), src)
	defer tree.Close()
	lang := Go()
	var walk func(n *Node)
	walk = func(n *Node) {
		if got := lang.SymbolName(n.Symbol()); got != n.Type() {
			t.Errorf("SymbolName(%d) = %q, Type() = %q", n.Symbol(), got, n.Type())
		}
		for i := range int(n.ChildCount()) {
			walk(n.Child(i))
		}
	}
	walk(tree.RootNode())
}
