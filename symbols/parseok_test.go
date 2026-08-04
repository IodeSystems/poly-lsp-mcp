package symbols

import "testing"

// The whole point: tree-sitter RETURNS SYMBOLS for input it could not parse,
// so "FileSymbols succeeded" is not evidence the source is valid. A
// conflicted file is the case that matters — both sides land in the table.
func TestParsesCleanlySeesWhatFileSymbolsHides(t *testing.T) {
	conflicted := []byte("package p\n\n<<<<<<< HEAD\nfunc A() {}\n=======\nfunc B() {}\n>>>>>>> x\n")
	syms, err := FileSymbols("go", conflicted)
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) < 3 {
		t.Fatalf("fixture drift: expected tree-sitter to recover symbols from both sides, got %d", len(syms))
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
