package symbols

import (
	"regexp"
	"sort"
	"strings"
)

// Comment-marker scanner. Universal pass that runs on every file's
// contents — no per-language plumbing. Three conventions:
//
//   @see <name>            JSDoc / TSDoc / JavaDoc — soft reference.
//                          Emits a comment-confidence site (visible to
//                          node_references; node_refactor skips by
//                          default).
//
//   {@link <name>}         TSDoc / JavaDoc bracketed form. Same as
//                          @see — soft reference.
//
//   @ref <name>            Doxygen tag, our extension. HARD reference:
//   @ref <file>:<name>     emits a declared-confidence site that
//   @ref <file>:<line>:<col>  rename touches by default.
//
//   x-ref: <value>         YAML/JSON extension key. Same semantics as
//                          @ref but for spec-style files (OpenAPI,
//                          JSON Schema) that prefer extension keys
//                          over docstring tags.
//
// The scanner is regex-based and runs over the whole file content. It
// doesn't differentiate comments from code or strings — false
// positives are possible if someone literally writes `@see Foo` in a
// string literal, but that's rare and the dedup vs. lexical sites
// keeps the index honest.

// CommentRef is one parsed marker. Confidence is ConfidenceComment
// for @see / @link or ConfidenceDeclared for @ref / x-ref.
type CommentRef struct {
	Name       string
	Line       int
	Col        int
	Confidence Confidence
}

var (
	// @see captures the next non-whitespace token. Post-process strips
	// punctuation and trailing path separators.
	seeRe = regexp.MustCompile(`@see\s+(\S+)`)
	// {@link target} or {@link target | label}
	linkRe = regexp.MustCompile(`\{@link\s+([^}|]+?)(?:\s*\|[^}]*)?\}`)
	// @ref token  — same shape as @see; post-process parses file:symbol
	refRe = regexp.MustCompile(`@ref\s+(\S+)`)
	// x-ref / x-tslsmcp-source / x-source as YAML/JSON extension keys.
	// Match the value (quoted or bare). Same shape parser.
	xrefRe = regexp.MustCompile(`(?m)^\s*"?x-(?:ref|tslsmcp-source|source)"?\s*:\s*"?([^"\s,}]+)"?`)
)

// ExtractCommentRefs scans content for all four marker shapes and
// returns one CommentRef per match. Position is 1-based and points at
// the captured token's start (where the cross-reference will surface
// in node_references).
func ExtractCommentRefs(content []byte) []CommentRef {
	newlines := commentNewlineOffsets(content)
	var out []CommentRef

	emitSoft := func(name string, start int) {
		if name == "" {
			return
		}
		line, col := commentOffsetToLineCol(start, newlines)
		out = append(out, CommentRef{Name: name, Line: line, Col: col, Confidence: ConfidenceComment})
	}
	emitHard := func(name string, start int) {
		if name == "" {
			return
		}
		line, col := commentOffsetToLineCol(start, newlines)
		out = append(out, CommentRef{Name: name, Line: line, Col: col, Confidence: ConfidenceDeclared})
	}

	for _, m := range seeRe.FindAllSubmatchIndex(content, -1) {
		token := string(content[m[2]:m[3]])
		emitSoft(symbolFromRef(token), m[2])
	}
	for _, m := range linkRe.FindAllSubmatchIndex(content, -1) {
		token := strings.TrimSpace(string(content[m[2]:m[3]]))
		emitSoft(symbolFromRef(token), m[2])
	}
	for _, m := range refRe.FindAllSubmatchIndex(content, -1) {
		token := string(content[m[2]:m[3]])
		emitHard(symbolFromRef(token), m[2])
	}
	for _, m := range xrefRe.FindAllSubmatchIndex(content, -1) {
		token := string(content[m[2]:m[3]])
		emitHard(symbolFromRef(token), m[2])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Col < out[j].Col
	})
	return out
}

// symbolFromRef extracts the identifier-shaped tail from a reference
// token. Handles a few common shapes:
//
//   Foo                       → Foo
//   Class#method              → method      (JavaDoc/JSDoc)
//   path/file.ts!Symbol       → Symbol      (TypeDoc)
//   server/main.go:Symbol     → Symbol
//   server/main.go:42:18      → ""           (positional refs not surfaced
//                                            as names today)
//   https://example.com/x     → ""           (URL — not a reference)
//
// Trailing punctuation (commas, periods, semicolons) is stripped.
func symbolFromRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if strings.Contains(ref, "://") {
		// URL — not a symbol reference.
		return ""
	}
	// Strip trailing punctuation.
	ref = strings.TrimRightFunc(ref, isTrailingPunct)
	if ref == "" {
		return ""
	}
	// Walk separators back-to-front. Find the last segment.
	for _, sep := range []string{"#", "!", "/", ":", "."} {
		if i := strings.LastIndex(ref, sep); i >= 0 {
			ref = ref[i+1:]
		}
	}
	// Keep only the leading identifier-shaped run. The (\S+) capture
	// at the top of the pipeline can drag along non-identifier suffix
	// chars — JSON-encoded `\n` escape sequences inside a description
	// string are the common culprit ("Foo\n" arriving as the candidate
	// would otherwise miss the "Foo" entry in the index). Anything
	// after the first non-identifier byte is dropped.
	end := 0
	for end < len(ref) && isIdentCont(ref[end]) {
		end++
	}
	ref = ref[:end]
	// Positional refs (e.g., "42") aren't useful as symbol names.
	if ref == "" || isAllDigits(ref) {
		return ""
	}
	// Must be identifier-shaped (start with letter/underscore).
	if !isIdentStart(ref[0]) {
		return ""
	}
	return ref
}

func isTrailingPunct(r rune) bool {
	switch r {
	case '.', ',', ';', ':', '!', '?', ')', ']', '}', '\'', '"':
		return true
	}
	return false
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isIdentStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isIdentCont(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}

func commentNewlineOffsets(content []byte) []int {
	var out []int
	for i, b := range content {
		if b == '\n' {
			out = append(out, i)
		}
	}
	return out
}

func commentOffsetToLineCol(offset int, newlines []int) (int, int) {
	lo, hi := 0, len(newlines)
	for lo < hi {
		mid := (lo + hi) / 2
		if newlines[mid] < offset {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	line := lo + 1
	lineStart := 0
	if lo > 0 {
		lineStart = newlines[lo-1] + 1
	}
	return line, offset - lineStart + 1
}
