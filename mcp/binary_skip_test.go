package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

// The projection walk (walkDir) must not turn binary files into file
// nodes: they have no symbols, node_read dumps megabytes of garbage, and
// countFileLines would slurp the whole thing. A source file next to the
// binary still shows. Regression for the dogfood find where mcp.test and
// the built binary polluted `:root > *`.
func TestWalkDirSkipsBinaryFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, b []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", []byte("module bt\ngo 1.21\n"))
	write("real.go", []byte("package bt\n\nfunc Real() {}\n"))
	// A "binary": null byte in the first 8 KB, like an ELF or image.
	write("app", append([]byte("\x7fELF\x00\x00\x00some junk"), make([]byte, 32)...))
	write("suite.test", []byte("prefix\x00\x00binary test binary"))

	s := newQueryServer(t, dir)
	e, err := s.buildTree()
	if err != nil {
		t.Fatal(err)
	}

	// fileByRel is the authoritative set of file nodes.
	for _, bin := range []string{"app", "suite.test"} {
		if _, ok := e.fileByRel[bin]; ok {
			t.Errorf("binary %q became a file node; must be skipped", bin)
		}
	}
	for _, want := range []string{"go.mod", "real.go"} {
		if _, ok := e.fileByRel[want]; !ok {
			t.Errorf("text file %q missing from projection; must stay", want)
		}
	}
}

// lateNull is 9000 non-null bytes followed by a null — the null sits
// past the 8 KB probe window, so it must NOT be flagged binary.
func lateNull() []byte {
	b := make([]byte, 9000)
	for i := range b {
		b[i] = 'a'
	}
	return append(b, 0)
}

// looksBinaryFile probes only the 8 KB prefix and treats a missing file
// as non-binary (the caller's own read surfaces the real error).
func TestLooksBinaryFile(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"text", mk("t.txt", []byte("hello\nworld\n")), false},
		{"empty", mk("e.txt", []byte{}), false},
		{"null-early", mk("b.bin", []byte("ab\x00cd")), true},
		// Null byte only AFTER the 8 KB probe window -> not detected (matches search.go).
		{"null-late", mk("l.bin", lateNull()), false},
		{"missing", filepath.Join(dir, "nope"), false},
	}
	for _, c := range cases {
		if got := looksBinaryFile(c.path); got != c.want {
			t.Errorf("%s: looksBinaryFile=%v want %v", c.name, got, c.want)
		}
	}
}
