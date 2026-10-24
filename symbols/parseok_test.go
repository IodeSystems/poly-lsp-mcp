package symbols

import (
	"testing"

	sitter "github.com/iodesystems/poly-lsp-mcp/internal/ts"
)

// The whole point: tree-sitter RETURNS SYMBOLS for input it could not parse,
// so "FileSymbols succeeded" is not evidence the source is valid. A
// conflicted file is the case that matters — both sides land in the table.
func TestParsesCleanlySeesWhatFileSymbolsHides(t *testing.T) {
	conflicted := []byte("package p\n\n<<<<<<< HEAD\nfunc A() {}\n=======\nfunc B() {}\n>>>>>>> x\n")
	syms, err := FileSymbols("go", conflicted)
	if err != nil {
		t.Fatal(err)
	}
	// How much survives is the runtime's error recovery: the C runtime
	// keeps declarations from both sides; the pure-Go one reads `func B()`
	// after `=======` as a call expression and keeps one side. Either way
	// symbols come back from a file that does not parse, which is the point.
	want := 3
	if sitter.Runtime() == "pure-go" {
		want = 2
	}
	if len(syms) < want {
		t.Fatalf("fixture drift: expected tree-sitter to recover symbols from the conflicted file, got %d, want >= %d", len(syms), want)
	}
	if ParsesCleanly("go", conflicted) {
		t.Error("a file full of conflict markers must not report a clean parse")
	}
	if !ParsesCleanly("go", []byte("package p\n\nfunc A() {}\n")) {
		t.Error("valid source should parse cleanly")
	}
}

func TestParsesCleanlyOnBrokenAndUnknown(t *testing.T) {
	if ParsesCleanly("go", []byte("package p\nfunc A( {\n")) {
		t.Error("a syntax error is not a clean parse")
	}
	// No grammar means nothing was verified, so nothing may be claimed.
	if ParsesCleanly("nosuchlang", []byte("anything")) {
		t.Error("an unknown language must not report a clean parse")
	}
}
