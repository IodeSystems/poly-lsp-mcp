package grammars

import (
	"unicode"

	gts "github.com/odvcencio/gotreesitter"
)

// External token indexes, in the grammar's `externals` order.
const (
	ktAutomaticSemicolon = iota
	ktImportListDelimiter
	ktSafeNav
	ktMultilineComment
	ktStringStart
	ktStringEnd
	ktStringContent
)

// kotlinScanner is a line-for-line port of smacker's kotlin/scanner.c
// (fwcd/tree-sitter-kotlin at smacker's pin). gotreesitter ships a Go scanner
// for a NEWER Kotlin grammar with a different externals list, so it cannot
// drive this one. The state is a stack of open string delimiters; a triple
// quote is stored as '"'+1, as in C.
type kotlinScanner struct{ lang *gts.Language }

type kotlinScanState struct{ stack []byte }

func (kotlinScanner) Create() any         { return &kotlinScanState{} }
func (kotlinScanner) Destroy(payload any) {}

func (kotlinScanner) Serialize(payload any, buf []byte) int {
	s := payload.(*kotlinScanState)
	if len(s.stack) > len(buf) {
		return 0
	}
	return copy(buf, s.stack)
}

func (kotlinScanner) Deserialize(payload any, buf []byte) {
	s := payload.(*kotlinScanState)
	s.stack = append(s.stack[:0], buf...)
}

func (k kotlinScanner) result(l *gts.ExternalLexer, tok int) {
	l.SetResultSymbol(k.lang.ExternalSymbols[tok])
}

func ktSkip(l *gts.ExternalLexer)    { l.Advance(true) }
func ktAdvance(l *gts.ExternalLexer) { l.Advance(false) }

func ktScanStringStart(l *gts.ExternalLexer, s *kotlinScanState) bool {
	if l.Lookahead() != '"' {
		return false
	}
	ktAdvance(l)
	l.MarkEnd()
	for count := 1; count < 3; count++ {
		if l.Lookahead() != '"' {
			s.stack = append(s.stack, '"')
			return true
		}
		ktAdvance(l)
	}
	l.MarkEnd()
	s.stack = append(s.stack, '"'+1)
	return true
}

func (k kotlinScanner) scanStringContent(l *gts.ExternalLexer, s *kotlinScanState) bool {
	if len(s.stack) == 0 {
		return false
	}
	endChar := rune(s.stack[len(s.stack)-1])
	isTriple := false
	hasContent := false
	if endChar&1 != 0 {
		isTriple = true
		endChar--
	}
	pop := func() { s.stack = s.stack[:len(s.stack)-1] }
	for l.Lookahead() != 0 {
		switch {
		case l.Lookahead() == '$':
			// Stop before a `$` that follows content, so an interpolated
			// identifier can be lexed on its own.
			if hasContent {
				k.result(l, ktStringContent)
				return true
			}
			ktAdvance(l)
			if unicode.IsLetter(l.Lookahead()) || l.Lookahead() == '{' {
				return false // interpolation: let the grammar take it
			}
			k.result(l, ktStringContent)
			l.MarkEnd()
			return true
		case l.Lookahead() == '\\':
			// An escaped `$` is content, never an interpolation.
			ktAdvance(l)
			if l.Lookahead() == '$' {
				ktAdvance(l)
				if l.Lookahead() == endChar {
					pop()
					ktAdvance(l)
					l.MarkEnd()
					k.result(l, ktStringEnd)
					return true
				}
			}
		case l.Lookahead() == endChar:
			if isTriple {
				l.MarkEnd()
				for count := 1; count < 3; count++ {
					ktAdvance(l)
					if l.Lookahead() != endChar {
						l.MarkEnd()
						k.result(l, ktStringContent)
						return true
					}
				}
				if hasContent && l.Lookahead() == endChar {
					k.result(l, ktStringContent)
					return true
				}
				// A run of quotes past three is all string end.
				k.result(l, ktStringEnd)
				l.MarkEnd()
				for l.Lookahead() == endChar {
					ktAdvance(l)
					l.MarkEnd()
				}
				pop()
				return true
			}
			if hasContent {
				l.MarkEnd()
				k.result(l, ktStringContent)
				return true
			}
			pop()
			ktAdvance(l)
			l.MarkEnd()
			k.result(l, ktStringEnd)
			return true
		}
		ktAdvance(l)
		hasContent = true
	}
	return false
}

func (k kotlinScanner) scanMultilineComment(l *gts.ExternalLexer) bool {
	if l.Lookahead() != '/' {
		return false
	}
	ktAdvance(l)
	if l.Lookahead() != '*' {
		return false
	}
	ktAdvance(l)
	afterStar := false
	depth := 1
	for {
		switch l.Lookahead() {
		case '*':
			ktAdvance(l)
			afterStar = true
		case '/':
			ktAdvance(l)
			if afterStar {
				afterStar = false
				depth--
				if depth == 0 {
					k.result(l, ktMultilineComment)
					l.MarkEnd()
					return true
				}
			} else {
				afterStar = false
				if l.Lookahead() == '*' {
					depth++
					ktAdvance(l)
				}
			}
		case 0:
			return false
		default:
			ktAdvance(l)
			afterStar = false
		}
	}
}

func ktScanWhitespaceAndComments(l *gts.ExternalLexer) bool {
	for unicode.IsSpace(l.Lookahead()) {
		ktSkip(l)
	}
	return l.Lookahead() != '/'
}

func ktScanForWord(l *gts.ExternalLexer, word string) bool {
	ktSkip(l)
	for i := 0; i < len(word); i++ {
		if l.Lookahead() != rune(word[i]) {
			return false
		}
		ktSkip(l)
	}
	return true
}

func (k kotlinScanner) scanAutomaticSemicolon(l *gts.ExternalLexer) bool {
	k.result(l, ktAutomaticSemicolon)
	l.MarkEnd()
	sameline := true
	for {
		if l.Lookahead() == 0 {
			return true
		}
		if l.Lookahead() == ';' {
			ktAdvance(l)
			l.MarkEnd()
			return true
		}
		if !unicode.IsSpace(l.Lookahead()) {
			break
		}
		if l.Lookahead() == '\n' {
			ktSkip(l)
			sameline = false
			break
		}
		if l.Lookahead() == '\r' {
			ktSkip(l)
			if l.Lookahead() == '\n' {
				ktSkip(l)
			}
			sameline = false
			break
		}
		ktSkip(l)
	}

	if !ktScanWhitespaceAndComments(l) {
		return false
	}

	if sameline {
		switch l.Lookahead() {
		case 'e': // no semicolon before an else
			return !ktScanForWord(l, "lse")
		case 'i':
			return ktScanForWord(l, "mport")
		case ';':
			ktAdvance(l)
			l.MarkEnd()
			return true
		default:
			return false
		}
	}

	switch l.Lookahead() {
	case ',', '.', ':', '*', '%', '>', '<', '=', '{', '[', '(', '?', '|', '&', '/':
		return false
	case '+': // before ++ and +Float, not binary +
		ktSkip(l)
		if l.Lookahead() == '+' {
			return true
		}
		return unicode.IsDigit(l.Lookahead())
	case '-':
		ktSkip(l)
		if l.Lookahead() == '-' {
			return true
		}
		return unicode.IsDigit(l.Lookahead())
	case '!': // before unary !, not !=
		ktSkip(l)
		return l.Lookahead() != '='
	case 'e':
		return !ktScanForWord(l, "lse")
	case 'i': // not before `in` / `instanceof`; yes before an identifier
		ktSkip(l)
		if l.Lookahead() != 'n' {
			return true
		}
		ktSkip(l)
		if !unicode.IsLetter(l.Lookahead()) {
			return false
		}
		return !ktScanForWord(l, "stanceof")
	case ';':
		ktAdvance(l)
		l.MarkEnd()
		return true
	default:
		return true
	}
}

func (k kotlinScanner) scanSafeNav(l *gts.ExternalLexer) bool {
	k.result(l, ktSafeNav)
	l.MarkEnd()
	if !ktScanWhitespaceAndComments(l) {
		return false
	}
	if l.Lookahead() != '?' {
		return false
	}
	ktAdvance(l)
	if !ktScanWhitespaceAndComments(l) {
		return false
	}
	if l.Lookahead() != '.' {
		return false
	}
	ktAdvance(l)
	l.MarkEnd()
	return true
}

// ktScanLineSep consumes one line separator (CR, LF or CRLF) after optional
// blanks.
func ktScanLineSep(l *gts.ExternalLexer) bool {
	state := 0
	for {
		switch l.Lookahead() {
		case ' ', '\t', '\v':
			ktAdvance(l)
		case '\n':
			ktAdvance(l)
			return true
		case '\r':
			if state == 1 {
				return true
			}
			state = 1
			ktAdvance(l)
		default:
			return state == 1
		}
	}
}

func (k kotlinScanner) scanImportListDelimiter(l *gts.ExternalLexer) bool {
	// An import list ends at an empty line or a line that is not an import.
	k.result(l, ktImportListDelimiter)
	l.MarkEnd()
	if l.Lookahead() == 0 {
		return true
	}
	if !ktScanLineSep(l) {
		return false
	}
	if ktScanLineSep(l) {
		l.MarkEnd()
		return true
	}
	for {
		switch l.Lookahead() {
		case ' ', '\t', '\v':
			ktAdvance(l)
		case 'i':
			return !ktScanForWord(l, "mport")
		default:
			return true
		}
	}
}

func (k kotlinScanner) Scan(payload any, l *gts.ExternalLexer, valid []bool) bool {
	s := payload.(*kotlinScanState)
	ok := func(i int) bool { return i < len(valid) && valid[i] }

	if ok(ktAutomaticSemicolon) {
		ret := k.scanAutomaticSemicolon(l)
		if !ret && ok(ktSafeNav) && l.Lookahead() == '?' {
			return k.scanSafeNav(l)
		}
		// No semicolon: a string or comment may still follow.
		if ret {
			return true
		}
	}
	if ok(ktImportListDelimiter) {
		return k.scanImportListDelimiter(l)
	}
	if ok(ktStringContent) && k.scanStringContent(l, s) {
		return true
	}
	for unicode.IsSpace(l.Lookahead()) {
		ktSkip(l)
	}
	if ok(ktStringStart) && ktScanStringStart(l, s) {
		k.result(l, ktStringStart)
		return true
	}
	if ok(ktMultilineComment) && k.scanMultilineComment(l) {
		return true
	}
	if ok(ktSafeNav) {
		return k.scanSafeNav(l)
	}
	return false
}
