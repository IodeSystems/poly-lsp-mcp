package mcp

import "testing"

// LSP counts `character` in UTF-16 code units; every column this tool
// reports is a 1-based byte. The two agree only on pure-ASCII lines,
// which is why the mismatch survived: identifiers are ASCII even in
// files that are not.
func TestUTF16ColumnConversions(t *testing.T) {
	// "héllo" — é is 2 bytes, 1 UTF-16 unit.
	// "x🙂y"  — 🙂 is 4 bytes, 2 UTF-16 units (a surrogate pair).
	data := []byte("héllo world\nx🙂y tail\nplain\n")

	cases := []struct {
		name             string
		line, utf16, byt int
	}{
		// Before any non-ASCII the two conventions agree.
		{"ascii head", 1, 1, 1},
		// After é: UTF-16 col 3 is byte col 4.
		{"after 2-byte rune", 1, 3, 4},
		{"word after accent", 1, 7, 8},
		// After the surrogate pair: UTF-16 col 4 is byte col 6.
		{"after surrogate pair", 2, 4, 6},
		{"line with no multibyte", 3, 3, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotByte, ok := utf16ColToByteColumn(data, c.line, c.utf16)
			if !ok || gotByte != c.byt {
				t.Errorf("utf16ColToByteColumn(%d, %d) = %d (ok=%v), want %d",
					c.line, c.utf16, gotByte, ok, c.byt)
			}
			gotU16, ok := byteOffsetToUTF16Col(data, c.line, c.byt)
			if !ok || gotU16 != c.utf16 {
				t.Errorf("byteOffsetToUTF16Col(%d, %d) = %d (ok=%v), want %d",
					c.line, c.byt, gotU16, ok, c.utf16)
			}
		})
	}
}

// The corrupting case: reading a UTF-16 column as a byte offset slices
// mid-character. This pins that the conversion lands on a rune boundary
// and selects the intended identifier.
func TestUTF16OffsetSelectsWholeIdentifier(t *testing.T) {
	// The comment before `value` contains a 2-byte rune, so the UTF-16
	// column of `value` is one less than its byte column.
	src := []byte("// café — value follows\nvalue := 1\n")
	// On line 1, `value` starts at UTF-16 col 11 and BYTE col 14: the
	// line carries é (2 bytes) and — (3 bytes) ahead of it.
	off, ok := utf16ColToByteOffset(src, 1, 11)
	if !ok {
		t.Fatal("conversion failed")
	}
	if got := string(src[off : off+5]); got != "value" {
		t.Errorf("sliced %q, want %q — the offset landed off a rune boundary", got, "value")
	}
	if off != 13 {
		t.Errorf("byte offset = %d, want 13", off)
	}
	// Reading that same column as BYTES is what the code used to do: it
	// lands at offset 10, inside the 3-byte em dash.
	if got := string(src[10 : 10+5]); got == "value" {
		t.Error("byte reading happened to agree; this case must diverge to be meaningful")
	}
}

func TestUTF16ClampsPastEndOfLine(t *testing.T) {
	data := []byte("ab\ncd\n")
	// A server may report an end position one past the last character.
	off, ok := utf16ColToByteOffset(data, 1, 99)
	if !ok {
		t.Fatal("expected clamping, not failure")
	}
	if off != 2 {
		t.Errorf("offset = %d, want 2 (end of line 1, before the newline)", off)
	}
}

func TestUTF16InvalidUTF8DegradesInsteadOfHanging(t *testing.T) {
	data := []byte{'a', 0xff, 'b', '\n'}
	if _, ok := utf16ColToByteOffset(data, 1, 3); !ok {
		t.Error("a malformed byte should degrade, not fail")
	}
}

// A rename result has to be CHECKABLE, not just a count. Dogfooding
// showed models spending ~30 calls grep-auditing a correct
// filesChanged:9, because the result gave them a number and nothing to
// verify it against.
func TestTouchedLinesAreSortedDedupedAndCapped(t *testing.T) {
	edits := []resolvedEdit{
		{Line: 7}, {Line: 3}, {Line: 7}, {Line: 5},
	}
	lines, truncated := touchedLines(edits)
	if truncated != 0 {
		t.Errorf("truncated = %d, want 0", truncated)
	}
	want := []int{3, 5, 7}
	if len(lines) != len(want) {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("lines = %v, want %v (sorted, de-duplicated)", lines, want)
		}
	}

	// The cap must REPORT its overflow rather than silently truncating.
	many := make([]resolvedEdit, 0, maxReportedLines+5)
	for i := 1; i <= maxReportedLines+5; i++ {
		many = append(many, resolvedEdit{Line: i})
	}
	lines, truncated = touchedLines(many)
	if len(lines) != maxReportedLines {
		t.Errorf("capped list = %d entries, want %d", len(lines), maxReportedLines)
	}
	if truncated != 5 {
		t.Errorf("truncated = %d, want 5 — a silent cap reads as 'that was all'", truncated)
	}
}
