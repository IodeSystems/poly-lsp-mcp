package grammars

import (
	"unicode"

	gotreesitter "github.com/odvcencio/gotreesitter"
)

// External token indexes, in the grammar's `externals` order.
const (
	tsxAutomaticSemicolon = iota
	tsxTemplateChars
	tsxTernaryQmark
	tsxHTMLComment
	tsxLogicalOr
	tsxEscapeSequence
	tsxRegexPattern
	tsxFunctionSignatureAutomaticSemicolon
	tsxErrorRecovery
)

// tsxScanner is a line-for-line port of smacker's typescript/tsx/scanner.h.
// gotreesitter's tsx scanner is for a newer grammar that added jsx_text at
// external index 7, and it binds by position, so on this grammar it would
// answer the wrong tokens. Stateless, like the C one.
type tsxScanner struct{ lang *gotreesitter.Language }

func (tsxScanner) Create() any                         { return nil }
func (tsxScanner) Destroy(payload any)                 {}
func (tsxScanner) Serialize(payload any, b []byte) int { return 0 }
func (tsxScanner) Deserialize(payload any, b []byte)   {}

func (s tsxScanner) result(l *gotreesitter.ExternalLexer, tok int) {
	l.SetResultSymbol(s.lang.ExternalSymbols[tok])
}

func tsxSkip(l *gotreesitter.ExternalLexer)    { l.Advance(true) }
func tsxAdvance(l *gotreesitter.ExternalLexer) { l.Advance(false) }

func (s tsxScanner) scanTemplateChars(l *gotreesitter.ExternalLexer) bool {
	s.result(l, tsxTemplateChars)
	for hasContent := false; ; hasContent = true {
		l.MarkEnd()
		switch l.Lookahead() {
		case '`':
			return hasContent
		case 0:
			return false
		case '$':
			tsxAdvance(l)
			if l.Lookahead() == '{' {
				return hasContent
			}
		case '\\':
			return hasContent
		default:
			tsxAdvance(l)
		}
	}
}

func tsxScanWhitespaceAndComments(l *gotreesitter.ExternalLexer, scannedComment *bool) bool {
	for {
		for unicode.IsSpace(l.Lookahead()) {
			tsxSkip(l)
		}
		if l.Lookahead() != '/' {
			return true
		}
		tsxSkip(l)
		switch l.Lookahead() {
		case '/':
			tsxSkip(l)
			for l.Lookahead() != 0 && l.Lookahead() != '\n' {
				tsxSkip(l)
			}
			*scannedComment = true
		case '*':
			tsxSkip(l)
			for l.Lookahead() != 0 {
				if l.Lookahead() == '*' {
					tsxSkip(l)
					if l.Lookahead() == '/' {
						tsxSkip(l)
						break
					}
				} else {
					tsxSkip(l)
				}
			}
		default:
			return false
		}
	}
}

func (s tsxScanner) scanAutomaticSemicolon(l *gotreesitter.ExternalLexer, valid func(int) bool, scannedComment *bool) bool {
	s.result(l, tsxAutomaticSemicolon)
	l.MarkEnd()
	for {
		if l.Lookahead() == 0 {
			return true
		}
		if l.Lookahead() == '}' {
			// No automatic semicolon before a typed object pattern:
			//   type F = ({a}: {a: number}) => number;
			for {
				tsxSkip(l)
				if !unicode.IsSpace(l.Lookahead()) {
					break
				}
			}
			if l.Lookahead() == ':' {
				return valid(tsxLogicalOr) // still true inside a ternary
			}
			return true
		}
		if !unicode.IsSpace(l.Lookahead()) {
			return false
		}
		if l.Lookahead() == '\n' {
			break
		}
		tsxSkip(l)
	}
	tsxSkip(l)

	if !tsxScanWhitespaceAndComments(l, scannedComment) {
		return false
	}

	switch l.Lookahead() {
	case ',', '.', ';', '*', '%', '>', '<', '=', '?', '^', '|', '&', '/', ':':
		return false
	case '{':
		if valid(tsxFunctionSignatureAutomaticSemicolon) {
			return false
		}
	case '(', '[':
		// Not before ( or [ in an expression; a valid binary operator
		// says an expression is what is being parsed.
		if valid(tsxLogicalOr) {
			return false
		}
	case '+': // before ++, not binary +
		tsxSkip(l)
		return l.Lookahead() == '+'
	case '-':
		tsxSkip(l)
		return l.Lookahead() == '-'
	case '!': // before unary !, not !=
		tsxSkip(l)
		return l.Lookahead() != '='
	case 'i': // not before `in` / `instanceof`; yes before an identifier
		tsxSkip(l)
		if l.Lookahead() != 'n' {
			return true
		}
		tsxSkip(l)
		if !unicode.IsLetter(l.Lookahead()) {
			return false
		}
		for _, c := range "stanceof" {
			if l.Lookahead() != c {
				return true
			}
			tsxSkip(l)
		}
		if !unicode.IsLetter(l.Lookahead()) {
			return false
		}
	}
	return true
}

func (s tsxScanner) scanTernaryQmark(l *gotreesitter.ExternalLexer) bool {
	for unicode.IsSpace(l.Lookahead()) {
		tsxSkip(l)
	}
	if l.Lookahead() != '?' {
		return false
	}
	tsxAdvance(l)
	if l.Lookahead() == '?' || l.Lookahead() == '.' { // optional chaining
		return false
	}
	l.MarkEnd()
	s.result(l, tsxTernaryQmark)
	// Optional arguments spell ?: with optional whitespace between.
	for unicode.IsSpace(l.Lookahead()) {
		tsxAdvance(l)
	}
	switch l.Lookahead() {
	case ':', ')', ',':
		return false
	case '.':
		tsxAdvance(l)
		return unicode.IsDigit(l.Lookahead())
	}
	return true
}

func (s tsxScanner) scanClosingComment(l *gotreesitter.ExternalLexer) bool {
	for unicode.IsSpace(l.Lookahead()) || l.Lookahead() == 0x2028 || l.Lookahead() == 0x2029 {
		tsxSkip(l)
	}
	var want string
	switch l.Lookahead() {
	case '<':
		want = "<!--"
	case '-':
		want = "-->"
	default:
		return false
	}
	for _, c := range want {
		if l.Lookahead() != c {
			return false
		}
		tsxAdvance(l)
	}
	for l.Lookahead() != 0 && l.Lookahead() != '\n' && l.Lookahead() != 0x2028 && l.Lookahead() != 0x2029 {
		tsxAdvance(l)
	}
	s.result(l, tsxHTMLComment)
	l.MarkEnd()
	return true
}

func (s tsxScanner) Scan(payload any, l *gotreesitter.ExternalLexer, validSymbols []bool) bool {
	valid := func(i int) bool { return i < len(validSymbols) && validSymbols[i] }
	if valid(tsxTemplateChars) {
		if valid(tsxAutomaticSemicolon) {
			return false
		}
		return s.scanTemplateChars(l)
	}
	if valid(tsxAutomaticSemicolon) || valid(tsxFunctionSignatureAutomaticSemicolon) {
		scannedComment := false
		ret := s.scanAutomaticSemicolon(l, valid, &scannedComment)
		if !ret && !scannedComment && valid(tsxTernaryQmark) && l.Lookahead() == '?' {
			return s.scanTernaryQmark(l)
		}
		return ret
	}
	if valid(tsxTernaryQmark) {
		return s.scanTernaryQmark(l)
	}
	if valid(tsxHTMLComment) && !valid(tsxLogicalOr) && !valid(tsxEscapeSequence) && !valid(tsxRegexPattern) {
		return s.scanClosingComment(l)
	}
	return false
}
