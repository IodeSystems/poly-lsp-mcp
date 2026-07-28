package symbols

import (
	"bytes"
	"regexp"
)

// MarkdownExtractor indexes the parts of a document that NAME things,
// not every word in it.
//
// The lexical rule that suits YAML and JSON — keep every token, because a
// config value is a contract — is wrong for prose. Measured on this
// repo before this existed: markdown was 34,093 sites, 32% of the whole
// index and second only to Go; `plan/done.md` alone was the heaviest
// file in the repository at 16,372 sites, more than twice the largest Go
// file. Half of all indexed names (4,057 of 7,890) appeared ONLY in
// docs, and the bulk of those were English stopwords — `on` 251 sites,
// `of` 211, `for` 209. Those names cost budget and skew every
// cardinality estimate while linking nothing.
//
// Three places in a document actually name things, and they are kept:
//
//   - HEADINGS, which title a section. This is the unit a document is
//     navigated by, and FileSymbols builds the same sections into nodes.
//   - FENCED CODE BLOCKS, which are code by definition.
//   - INLINE CODE SPANS — `UserID` in prose. This is the one that earns
//     the cross-language claim: a doc that mentions a Go type in
//     backticks still links to it, which is what makes rename propagate
//     into prose.
//
// Everything else — paragraph text, list prose, link titles — is
// dropped.
type MarkdownExtractor struct{}

var (
	mdHeadingRe = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$`)
	// A fence opens and closes with three or more backticks or tildes.
	// The info string on the opening line (```go) names a LANGUAGE, not
	// a program entity, so the delimiter lines are never indexed.
	mdFenceRe = regexp.MustCompile("^\\s{0,3}(```+|~~~+)")
	// Inline code spans. Non-greedy so `a` and `b` on one line are two
	// spans rather than one that swallows the prose between them.
	mdCodeSpanRe = regexp.MustCompile("`+([^`\\n]+)`+")
)

func (e *MarkdownExtractor) Extract(content []byte) []Hit {
	var hits []Hit
	inFence := false

	// emit indexes the identifier tokens of span, offsetting columns by
	// the span's position within its line.
	emit := func(line []byte, lineIdx, colOffset int) {
		for _, m := range identRe.FindAllIndex(line, -1) {
			hits = append(hits, Hit{
				Name: string(line[m[0]:m[1]]),
				Line: lineIdx + 1,
				Col:  colOffset + m[0] + 1,
			})
		}
	}

	for lineIdx, line := range bytes.Split(content, []byte("\n")) {
		if mdFenceRe.Match(line) {
			// The delimiter itself is never content, in either direction.
			inFence = !inFence
			continue
		}
		if inFence {
			emit(line, lineIdx, 0)
			continue
		}
		if m := mdHeadingRe.FindSubmatchIndex(line); m != nil {
			emit(line[m[2]:m[3]], lineIdx, m[2])
			continue
		}
		for _, m := range mdCodeSpanRe.FindAllSubmatchIndex(line, -1) {
			emit(line[m[2]:m[3]], lineIdx, m[2])
		}
	}
	return hits
}
