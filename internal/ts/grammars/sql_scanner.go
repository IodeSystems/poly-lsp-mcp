package grammars

import (
	"unicode"

	gts "github.com/odvcencio/gotreesitter"
)

// External token indexes, in the grammar's `externals` order.
const (
	sqlDollarStartTag = iota
	sqlDollarEndTag
	sqlDollarString
)

// sqlScanner ports smacker's sql/scanner.c: PostgreSQL dollar-quoted strings.
// The state is the open $tag$, so the closing tag can be matched.
type sqlScanner struct{ lang *gts.Language }

type sqlScanState struct{ startTag string }

func (sqlScanner) Create() any         { return &sqlScanState{} }
func (sqlScanner) Destroy(payload any) {}

// Serialize writes the tag and a NUL, like the C scanner; an empty state is
// zero bytes.
func (sqlScanner) Serialize(payload any, buf []byte) int {
	s := payload.(*sqlScanState)
	if s.startTag == "" || len(s.startTag)+1 > len(buf) {
		return 0
	}
	copy(buf, s.startTag)
	buf[len(s.startTag)] = 0
	return len(s.startTag) + 1
}

func (sqlScanner) Deserialize(payload any, buf []byte) {
	s := payload.(*sqlScanState)
	s.startTag = ""
	if len(buf) > 1 {
		end := len(buf)
		for i, b := range buf {
			if b == 0 {
				end = i
				break
			}
		}
		s.startTag = string(buf[:end])
	}
}

func atEOF(l *gts.ExternalLexer) bool { return l.Lookahead() == 0 }

// scanDollarTag reads $ident$ (or $$) and returns it, or "" when the input
// at the cursor is not a tag.
func scanDollarTag(l *gts.ExternalLexer) string {
	if l.Lookahead() != '$' {
		return ""
	}
	tag := []rune{'$'}
	l.Advance(false)
	for l.Lookahead() != '$' && !unicode.IsSpace(l.Lookahead()) && !atEOF(l) {
		tag = append(tag, l.Lookahead())
		l.Advance(false)
	}
	if l.Lookahead() != '$' {
		return ""
	}
	l.Advance(false)
	return string(append(tag, '$'))
}

func (sc sqlScanner) Scan(payload any, l *gts.ExternalLexer, valid []bool) bool {
	s := payload.(*sqlScanState)
	ok := func(i int) bool { return i < len(valid) && valid[i] }
	sym := func(i int) gts.Symbol { return sc.lang.ExternalSymbols[i] }

	if ok(sqlDollarStartTag) && s.startTag == "" {
		for unicode.IsSpace(l.Lookahead()) {
			l.Advance(true)
		}
		tag := scanDollarTag(l)
		if tag == "" {
			return false
		}
		s.startTag = tag
		l.SetResultSymbol(sym(sqlDollarStartTag))
		return true
	}

	if ok(sqlDollarEndTag) && s.startTag != "" {
		for unicode.IsSpace(l.Lookahead()) {
			l.Advance(true)
		}
		if tag := scanDollarTag(l); tag != "" && tag == s.startTag {
			s.startTag = ""
			l.SetResultSymbol(sym(sqlDollarEndTag))
			return true
		}
		return false
	}

	if ok(sqlDollarString) {
		l.MarkEnd()
		for unicode.IsSpace(l.Lookahead()) {
			l.Advance(true)
		}
		start := scanDollarTag(l)
		if start == "" || start == s.startTag {
			return false
		}
		for !atEOF(l) {
			end := scanDollarTag(l)
			if end == "" {
				l.Advance(false)
				continue
			}
			if end == start {
				l.MarkEnd()
				l.SetResultSymbol(sym(sqlDollarString))
				return true
			}
		}
		return false
	}
	return false
}
