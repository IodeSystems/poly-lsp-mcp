package bindings

import (
	"fmt"
	"regexp"
)

// Regex site form — the escape hatch for files we have no parser for
// (Dockerfile, TOML, HCL, prose markdown, etc.) and for content
// tree-sitter intentionally drops (comments, string literals, struct
// tags).
//
// Multi-pattern: a single site may declare a list of patterns; the
// resolver applies each independently and unions the matches.
//
// Capture-group convention: zero or one capture group per pattern.
//   - Zero: the whole match is the token.
//   - One:  the captured slice is the token. Lets the binding pin a
//           position inside a larger context (e.g., `name="(UserID)"`).
//   - Two+: rejected at compile time. Named groups are not supported.
//
// Aliasing: v0.2.x requires the captured/matched text to equal the
// binding name. Patterns that match content with different text are
// rejected per match by the resolver's safety check. Aliasing-via-regex
// is a future-work item that requires Site.Length plumbing through the
// rest of the index — out of scope here.

// regexHit is one position produced by evaluating a pattern. Text is
// the captured (or full-match) slice — callers verify it equals the
// binding name before promoting to a declared site.
type regexHit struct {
	Line int
	Col  int
	Text string
}

// compilePattern returns a stdlib regexp.Regexp restricted to zero or
// one capture group. The capture group, when present, must be the one
// used to extract the token text.
func compilePattern(pattern string) (*regexp.Regexp, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile %q: %w", pattern, err)
	}
	if n := re.NumSubexp(); n > 1 {
		return nil, fmt.Errorf("pattern %q has %d capture groups; at most 1 allowed", pattern, n)
	}
	return re, nil
}

// evalRegex runs every pattern against content and returns the union of
// hits across all of them. Position tracking is byte-accurate: Line and
// Col are 1-based, byte-offset within line. Patterns are compiled
// individually so a bad pattern in the list reports its own error.
func evalRegex(content []byte, patterns []string) ([]regexHit, error) {
	if len(patterns) == 0 {
		return nil, fmt.Errorf("regex site has no patterns")
	}
	// Precompute newline offsets so we can convert byte offsets into
	// (line, col) without re-scanning the file once per match.
	newlines := newlineOffsets(content)

	var hits []regexHit
	for _, p := range patterns {
		re, err := compilePattern(p)
		if err != nil {
			return hits, err
		}
		for _, m := range re.FindAllSubmatchIndex(content, -1) {
			var start, end int
			if re.NumSubexp() == 1 && m[2] >= 0 {
				start, end = m[2], m[3]
			} else {
				start, end = m[0], m[1]
			}
			if start < 0 || end < 0 || start >= len(content) {
				continue
			}
			line, col := offsetToLineCol(start, newlines)
			hits = append(hits, regexHit{
				Line: line,
				Col:  col,
				Text: string(content[start:end]),
			})
		}
	}
	return hits, nil
}

// newlineOffsets returns the byte offset of every '\n' in content,
// sorted. Used by offsetToLineCol to binary-search a byte offset into
// (line, col) coordinates in O(log n) per match.
func newlineOffsets(content []byte) []int {
	var out []int
	for i, b := range content {
		if b == '\n' {
			out = append(out, i)
		}
	}
	return out
}

// offsetToLineCol converts a byte offset into the 1-based (line, col)
// the rest of the symbol index uses. col is bytes-from-line-start + 1.
func offsetToLineCol(offset int, newlines []int) (int, int) {
	// Line number: how many newlines precede `offset`.
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
