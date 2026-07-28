package mcp

import "unicode/utf8"

// LSP counts a position's `character` in UTF-16 CODE UNITS. Every other
// column in this codebase — MCP tool arguments, index sites, node ranges
// — is a 1-based BYTE offset within the line. The two agree only while a
// line is pure ASCII, which is why this went unnoticed: identifiers are
// ASCII even in files that are not.
//
// The mismatch bites wherever a line has a non-ASCII character BEFORE
// the position of interest — a comment, a string literal, an accented
// name — and it bites in both directions:
//
//   - OUTBOUND, sending a byte column to gopls as `character`, points the
//     server at the wrong offset, so a rename resolves the wrong symbol
//     or none at all.
//   - INBOUND, reading gopls's `character` as a byte column, produces
//     edit ranges that slice mid-character. That one CORRUPTS the file it
//     was asked to rename.
//
// These two helpers are the boundary. Nothing else should convert an LSP
// character field by hand.

// lineStartOffset returns the byte offset where a 1-based line begins.
func lineStartOffset(data []byte, line int) (int, bool) {
	pos := 0
	cur := 1
	for cur < line && pos < len(data) {
		nl := bytesIndexNewline(data[pos:])
		if nl < 0 {
			return 0, false
		}
		pos += nl + 1
		cur++
	}
	if cur != line {
		return 0, false
	}
	return pos, true
}

// utf16ColToByteOffset converts a 1-based line and a 1-based UTF-16
// column — the shape an LSP position takes after the usual `+1` — into a
// byte offset into data.
//
// A column past the end of the line clamps to the line's end rather than
// failing: servers legitimately report an end position one past the last
// character.
func utf16ColToByteOffset(data []byte, line, utf16Col int) (int, bool) {
	pos, ok := lineStartOffset(data, line)
	if !ok {
		return 0, false
	}
	want := utf16Col - 1
	if want < 0 {
		return 0, false
	}
	units := 0
	off := pos
	for units < want && off < len(data) {
		if data[off] == '\n' {
			break
		}
		r, size := utf8.DecodeRune(data[off:])
		if r == utf8.RuneError && size <= 1 {
			// Invalid byte: count it as one unit so a malformed file
			// degrades instead of hanging.
			units++
			off++
			continue
		}
		units += utf16Len(r)
		off += size
	}
	return off, true
}

// byteOffsetToUTF16Col is the inverse: given a 1-based line and a
// 1-based BYTE column, produce the 1-based UTF-16 column an LSP server
// expects.
func byteOffsetToUTF16Col(data []byte, line, byteCol int) (int, bool) {
	pos, ok := lineStartOffset(data, line)
	if !ok {
		return 0, false
	}
	target := pos + byteCol - 1
	if target < pos {
		return 0, false
	}
	if target > len(data) {
		target = len(data)
	}
	units := 0
	off := pos
	for off < target && off < len(data) {
		if data[off] == '\n' {
			break
		}
		r, size := utf8.DecodeRune(data[off:])
		if r == utf8.RuneError && size <= 1 {
			units++
			off++
			continue
		}
		units += utf16Len(r)
		off += size
	}
	return units + 1, true
}

// utf16Len is how many UTF-16 code units encode r: two for anything
// outside the Basic Multilingual Plane, which is a surrogate pair.
func utf16Len(r rune) int {
	if r > 0xFFFF {
		return 2
	}
	return 1
}

// utf16ColToByteColumn converts a 1-based UTF-16 column into a 1-based
// BYTE column within the same line — the conversion a caller wants when
// it is reporting a position rather than slicing at one.
func utf16ColToByteColumn(data []byte, line, utf16Col int) (int, bool) {
	off, ok := utf16ColToByteOffset(data, line, utf16Col)
	if !ok {
		return 0, false
	}
	start, ok := lineStartOffset(data, line)
	if !ok {
		return 0, false
	}
	return off - start + 1, true
}
