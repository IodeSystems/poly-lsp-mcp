package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// Engine-level benchmarks: what a node_query costs end to end, and which
// layer it is spent in. The symbols package benchmarks the parse/walk beneath
// this; these measure the tree build and the selector shapes on top.
//
// The workspace is GENERATED so a number means the same thing next month —
// benchmarking the live repo would drift with every commit.

// benchWorkspace writes a deterministic Go workspace of `files` files, each
// with `perFile` type+method+func triples, and returns its root.
func benchWorkspace(tb testing.TB, files, perFile int) string {
	tb.Helper()
	dir := tb.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module bench\ngo 1.21\n"), 0o644); err != nil {
		tb.Fatal(err)
	}
	for f := 0; f < files; f++ {
		var b strings.Builder
		fmt.Fprintf(&b, "package bench\n\nimport \"fmt\"\n")
		for i := 0; i < perFile; i++ {
			id := f*perFile + i
			fmt.Fprintf(&b, `
// T%d is a documented type, at a length real doc comments reach.
type T%d struct{ Name string }

func (t T%d) Method%d(ctx string) error {
	fmt.Println(ctx, t.Name)
	return nil
}

func Fn%d(a, b int) int {
	if a > b {
		return Fn%d(b, a)
	}
	return a + b
}
`, id, id, id, id, id, id)
		}
		name := filepath.Join(dir, fmt.Sprintf("f%03d.go", f))
		if err := os.WriteFile(name, []byte(b.String()), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	return dir
}

func benchServer(tb testing.TB, root string) *Server {
	tb.Helper()
	reg, err := config.Default().Build()
	if err != nil {
		tb.Fatal(err)
	}
	s := New(reg, root, nil, nil)
	s.SetFileWatch(false) // a watcher would add goroutines and noise, not signal
	return s
}

// A COLD tree build: every file read and parsed. This is what the first query
// of a session pays, and what the CLI used to pay on every invocation.
func BenchmarkBuildTreeCold(b *testing.B) {
	for _, sz := range []struct{ files, per int }{{10, 5}, {50, 5}} {
		root := benchWorkspace(b, sz.files, sz.per)
		b.Run(fmt.Sprintf("files=%d", sz.files), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				// A fresh Server each time: no symbol cache carried over,
				// which is what "cold" means here.
				s := benchServer(b, root)
				e, err := s.buildTree()
				if err != nil {
					b.Fatal(err)
				}
				list, _ := parseModernSelector("func")
				if len(e.evaluate(list)) == 0 {
					b.Fatal("fixture drift: no funcs")
				}
			}
		})
	}
}

// The same work on a WARM server — the shape a long-running MCP session
// actually has, and the case symbols.SymbolCache exists for. buildTree still
// makes a fresh engine per query; only the parse is reused.
func BenchmarkBuildTreeWarm(b *testing.B) {
	root := benchWorkspace(b, 50, 5)
	s := benchServer(b, root)
	list, _ := parseModernSelector("func")
	if e, err := s.buildTree(); err != nil { // prime the cache
		b.Fatal(err)
	} else {
		e.evaluate(list)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		e, err := s.buildTree()
		if err != nil {
			b.Fatal(err)
		}
		e.evaluate(list)
	}
}

// Selector shapes, all on one warm server so the differences are the
// SELECTOR's cost rather than the tree build's.
func BenchmarkQueryShapes(b *testing.B) {
	root := benchWorkspace(b, 50, 5)
	s := benchServer(b, root)
	if e, err := s.buildTree(); err == nil { // prime
		list, _ := parseModernSelector("*")
		e.evaluate(list)
	}

	for _, tc := range []struct{ name, sel string }{
		{"tag", "func"},
		{"path_scoped", "path=f001.go func"},
		{"attr_exact", "func[name=Fn7]"},
		{"attr_regex", "func[name~=Fn1|Fn2|Fn3]"},
		{"star", "*"},
		{"grep", "::grep('-E Fn1[0-9]')"},
		{"edge_anchored", "#'f001.go#Fn5'::in.call"},
		{"edge_crossed", "#'f001.go#Fn5'::in.call > *"},
		{"generated_body", "func::body"},
	} {
		list, err := parseModernSelector(tc.sel)
		if err != nil {
			b.Fatalf("%s: %v", tc.name, err)
		}
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				e, err := s.buildTree()
				if err != nil {
					b.Fatal(err)
				}
				e.evaluate(list)
			}
		})
	}
}

// Conflict parsing runs on every watched write and inside the query-time
// warning, so its CLEAN-file path — the overwhelmingly common one — has to be
// nearly free. It short-circuits on a single Contains.
func BenchmarkParseConflicts(b *testing.B) {
	clean := []byte(strings.Repeat("func A() {}\n", 500))
	conflicted := append([]byte("<<<<<<< HEAD\na\n=======\nb\n>>>>>>> x\n"), clean...)
	for _, tc := range []struct {
		name string
		src  []byte
	}{{"clean", clean}, {"conflicted", conflicted}} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.src)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				parseConflicts(tc.src)
			}
		})
	}
}
