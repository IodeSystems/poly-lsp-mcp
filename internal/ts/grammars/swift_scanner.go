package grammars

import (
	"unicode"

	gts "github.com/odvcencio/gotreesitter"
)

// External token indexes, in the grammar's `externals` order.
const (
	swBlockComment = iota
	swRawStrPart
	swRawStrContinuingIndicator
	swRawStrEndPart
	swImplicitSemi
	swExplicitSemi
	swArrowOperator
	swDotOperator
	swConjunctionOperator
	swDisjunctionOperator
	swNilCoalescingOperator
	swEqualSign
	swEqEq
	swPlusThenWs
	swMinusThenWs
	swBang
	swThrowsKeyword
	swRethrowsKeyword
	swDefaultKeyword
	swWhereKeyword
	swElseKeyword
	swCatchKeyword
	swAsKeyword
	swAsQuest
	swAsBang
	swAsyncKeyword
	swCustomOperator
	swFakeTryBang
	swTokenCount
)

// swiftScanner is a line-for-line port of smacker's swift/scanner.c
// (alex-pinkus/tree-sitter-swift at smacker's pin). gotreesitter ships a Go
// scanner for a NEWER Swift grammar with 34 externals (double-optional,
// #if directives); this grammar has 28, so that one cannot drive it. The
// state is the hash count of the raw string being read.
type swiftScanner struct{ lang *gts.Language }

type swiftScanState struct{ rawStrHashCount uint32 }

func (swiftScanner) Create() any         { return &swiftScanState{} }
func (swiftScanner) Destroy(payload any) {}

func (swiftScanner) Serialize(payload any, buf []byte) int {
	if len(buf) < 4 {
		return 0
	}
	h := payload.(*swiftScanState).rawStrHashCount
	buf[0], buf[1], buf[2], buf[3] = byte(h>>24), byte(h>>16), byte(h>>8), byte(h)
	return 4
}

// Deserialize leaves the state alone on a short buffer, as the C scanner does.
func (swiftScanner) Deserialize(payload any, buf []byte) {
	if len(buf) < 4 {
		return
	}
	payload.(*swiftScanState).rawStrHashCount = uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])
}

var swOperators = [...]string{
	"->", ".", "&&", "||", "??", "=", "==", "+", "-", "!",
	"throws", "rethrows", "default", "where", "else", "catch", "as", "as?", "as!", "async",
}

// Which next character may not follow an operator for it to count.
const (
	swAlphanumeric = iota
	swOperatorSymbols
	swOperatorOrDot
	swNonWhitespace
)

var swOpIllegalTerminators = [len(swOperators)]int{
	swOperatorSymbols, // ->
	swOperatorOrDot,   // .
	swOperatorSymbols, // &&
	swOperatorSymbols, // ||
	swOperatorSymbols, // ??
	swOperatorSymbols, // =
	swOperatorSymbols, // ==
	swNonWhitespace,   // +
	swNonWhitespace,   // -
	swOperatorSymbols, // !
	swAlphanumeric,    // throws
	swAlphanumeric,    // rethrows
	swAlphanumeric,    // default
	swAlphanumeric,    // where
	swAlphanumeric,    // else
	swAlphanumeric,    // catch
	swAlphanumeric,    // as
	swOperatorSymbols, // as?
	swOperatorSymbols, // as!
	swAlphanumeric,    // async
}

var swOpSymbols = [len(swOperators)]int{
	swArrowOperator, swDotOperator, swConjunctionOperator, swDisjunctionOperator,
	swNilCoalescingOperator, swEqualSign, swEqEq, swPlusThenWs, swMinusThenWs, swBang,
	swThrowsKeyword, swRethrowsKeyword, swDefaultKeyword, swWhereKeyword, swElseKeyword,
	swCatchKeyword, swAsKeyword, swAsQuest, swAsBang, swAsyncKeyword,
}

// swOpSymbolSuppressor: a valid token here suppresses the operator. `try!`
// leaves its `!` to the grammar.
var swOpSymbolSuppressor = [len(swOperators)]uint64{9: 1 << swFakeTryBang}

var swReservedOps = [...]string{
	"/", "=", "-", "+", "!", "*", "%", "<", ">", "&", "|", "^", "?", "~", ".",
	"..", "->", "/*", "*/", "+=", "-=", "*=", "/=", "%=", ">>", "<<", "++", "--",
	"===", "...", "..<",
}

// swAt reads a C string: 0 past the end.
func swAt(s string, i int) rune {
	if i >= len(s) {
		return 0
	}
	return rune(s[i])
}

func swIsCrossSemiToken(op int) bool {
	switch op {
	case swArrowOperator, swDotOperator, swConjunctionOperator, swDisjunctionOperator,
		swNilCoalescingOperator, swEqualSign, swEqEq, swPlusThenWs, swMinusThenWs,
		swThrowsKeyword, swRethrowsKeyword, swDefaultKeyword, swWhereKeyword,
		swElseKeyword, swCatchKeyword, swAsKeyword, swAsQuest, swAsBang,
		swAsyncKeyword, swCustomOperator:
		return true
	}
	return false
}

// swNonConsumingCrossSemiChars suppress a newline's semicolon without being
// consumed.
var swNonConsumingCrossSemiChars = [...]rune{'?', ':', '{'}

// Parse directives, as in the C scanner.
const (
	swContinueNothingFound = iota
	swContinueTokenFound
	swContinueSlashConsumed
	swStopNothingFound
	swStopTokenFound
	swStopEOF
)

func swIsSpace(c rune) bool { return unicode.IsSpace(c) }
func swIsAlnum(c rune) bool { return unicode.IsLetter(c) || unicode.IsDigit(c) }

func swTreatAsSpace(c rune) bool { return swIsSpace(c) || c == ';' }

func swIsLegalCustomOperator(charIdx int, firstChar, cur rune) bool {
	isFirst := charIdx == 0
	switch cur {
	case '=', '-', '+', '!', '%', '<', '>', '&', '|', '^', '?', '~':
		return true
	case '.':
		// The grammar allows `.` in any operator that starts with `.`.
		return isFirst || firstChar == '.'
	case '*', '/':
		// `/*` and `//` start comments, not operators.
		return charIdx != 1 || firstChar != '/'
	}
	c := cur
	if (c >= 0x00A1 && c <= 0x00A7) || c == 0x00A9 || c == 0x00AB || c == 0x00AC ||
		c == 0x00AE || (c >= 0x00B0 && c <= 0x00B1) || c == 0x00B6 || c == 0x00BB ||
		c == 0x00BF || c == 0x00D7 || c == 0x00F7 || (c >= 0x2016 && c <= 0x2017) ||
		(c >= 0x2020 && c <= 0x2027) || (c >= 0x2030 && c <= 0x203E) ||
		(c >= 0x2041 && c <= 0x2053) || (c >= 0x2055 && c <= 0x205E) ||
		(c >= 0x2190 && c <= 0x23FF) || (c >= 0x2500 && c <= 0x2775) ||
		(c >= 0x2794 && c <= 0x2BFF) || (c >= 0x2E00 && c <= 0x2E7F) ||
		(c >= 0x3001 && c <= 0x3003) || (c >= 0x3008 && c <= 0x3020) || c == 0x3030 {
		return true
	}
	if (c >= 0x0300 && c <= 0x036f) || (c >= 0x1DC0 && c <= 0x1DFF) ||
		(c >= 0x20D0 && c <= 0x20FF) || (c >= 0xFE00 && c <= 0xFE0F) ||
		(c >= 0xFE20 && c <= 0xFE2F) || (c >= 0xE0100 && c <= 0xE01EF) {
		return !isFirst
	}
	return false
}

func swEatOperators(l *gts.ExternalLexer, ok func(int) bool, markEnd bool, priorChar rune) (int, bool) {
	var possible [len(swOperators)]bool
	var reserved [len(swReservedOps)]uint8
	for i, op := range swOperators {
		possible[i] = ok(swOpSymbols[i]) && (priorChar == 0 || rune(op[0]) == priorChar)
	}
	for i, op := range swReservedOps {
		if priorChar == 0 || rune(op[0]) == priorChar {
			reserved[i] = 1
		}
	}

	possibleCustom := ok(swCustomOperator)
	firstChar := priorChar
	if firstChar == 0 {
		firstChar = l.Lookahead()
	}
	lastExamined := firstChar

	strIdx := 0
	if priorChar != 0 {
		strIdx = 1
	}
	fullMatch := -1
	for {
		for i, op := range swOperators {
			if !possible[i] {
				continue
			}
			if swAt(op, strIdx) == 0 {
				// The operator is complete; may the next character follow it?
				illegal := swOpIllegalTerminators[i]
				la := l.Lookahead()
				legal := true
				switch la {
				case '/', '=', '-', '+', '!', '*', '%', '<', '>', '&', '|', '^', '?', '~':
					if illegal == swOperatorSymbols {
						legal = false
						break
					}
					fallthrough
				case '.':
					if illegal == swOperatorOrDot {
						legal = false
						break
					}
					fallthrough
				default:
					if swIsAlnum(la) && illegal == swAlphanumeric {
						legal = false
						break
					}
					if !swIsSpace(la) && illegal == swNonWhitespace {
						legal = false
						break
					}
				}
				if legal {
					fullMatch = i
					if markEnd {
						l.MarkEnd()
					}
				}
				possible[i] = false
				continue
			}
			if swAt(op, strIdx) != l.Lookahead() {
				possible[i] = false
				continue
			}
		}

		for i, op := range swReservedOps {
			if reserved[i] == 0 {
				continue
			}
			if swAt(op, strIdx) == 0 {
				reserved[i] = 0
				continue
			}
			if swAt(op, strIdx) != l.Lookahead() {
				reserved[i] = 0
				continue
			}
			if swAt(op, strIdx+1) == 0 {
				reserved[i] = 2
				continue
			}
		}

		possibleCustom = possibleCustom && swIsLegalCustomOperator(strIdx, firstChar, l.Lookahead())

		encountered := 0
		for _, p := range possible {
			if p {
				encountered++
			}
		}
		if encountered == 0 {
			if !possibleCustom {
				break
			} else if markEnd && fullMatch == -1 {
				l.MarkEnd()
			}
		}

		lastExamined = l.Lookahead()
		l.Advance(false)
		strIdx++

		if encountered == 0 && !swIsLegalCustomOperator(strIdx, firstChar, l.Lookahead()) {
			break
		}
	}

	if fullMatch != -1 {
		// A suppressing symbol valid here leaves the operator to the grammar.
		if sup := swOpSymbolSuppressor[fullMatch]; sup != 0 {
			for s := 0; s < swTokenCount; s++ {
				if sup&(1<<s) != 0 && ok(s) {
					return 0, false
				}
			}
		}
		return swOpSymbols[fullMatch], true
	}

	anyReserved := false
	for _, r := range reserved {
		if r == 2 {
			anyReserved = true
		}
	}
	if possibleCustom && !anyReserved {
		if (lastExamined != '<' || swIsSpace(l.Lookahead())) && markEnd {
			l.MarkEnd()
		}
		return swCustomOperator, true
	}
	return 0, false
}

func swEatComment(l *gts.ExternalLexer, markEnd bool) (int, int) {
	if l.Lookahead() != '/' {
		return swContinueNothingFound, 0
	}
	l.Advance(false)
	if l.Lookahead() != '*' {
		return swContinueSlashConsumed, 0
	}
	l.Advance(false)

	afterStar := false
	depth := 1
	for {
		switch l.Lookahead() {
		case 0:
			return swStopEOF, 0
		case '*':
			l.Advance(false)
			afterStar = true
		case '/':
			if afterStar {
				l.Advance(false)
				afterStar = false
				depth--
				if depth == 0 {
					if markEnd {
						l.MarkEnd()
					}
					return swStopTokenFound, swBlockComment
				}
			} else {
				l.Advance(false)
				afterStar = false
				if l.Lookahead() == '*' {
					depth++
					l.Advance(false)
				}
			}
		default:
			l.Advance(false)
			afterStar = false
		}
	}
}

func swEatWhitespace(l *gts.ExternalLexer, ok func(int) bool) (int, int) {
	directive := swContinueNothingFound
	result := 0
	semiValid := ok(swImplicitSemi) && ok(swExplicitSemi)
	var lookahead rune
	for {
		lookahead = l.Lookahead()
		if !swTreatAsSpace(lookahead) {
			break
		}
		if lookahead == ';' {
			if semiValid {
				directive = swStopTokenFound
				l.Advance(false)
			}
			break
		}
		l.Advance(true)
		l.MarkEnd()
		if directive == swContinueNothingFound && (lookahead == '\n' || lookahead == '\r') {
			directive = swContinueTokenFound
		}
	}

	if directive == swContinueTokenFound && lookahead == '/' {
		seenSingle := false
		for l.Lookahead() == '/' {
			// Possibly a comment: look past it for what decides the newline.
			anyComment, commentResult := swEatComment(l, false)
			if anyComment == swStopTokenFound {
				// A block comment is this scanner's to emit, unless a line
				// comment was passed on the way.
				if !seenSingle {
					l.MarkEnd()
					return swStopTokenFound, commentResult
				}
			} else if anyComment == swStopEOF {
				return swStopEOF, 0
			} else if anyComment == swContinueSlashConsumed {
				// A slash was eaten: report nothing and let the next pass
				// start after the newline.
				return swContinueSlashConsumed, 0
			} else if l.Lookahead() == '/' {
				// The second slash of a line comment.
				seenSingle = true
				for l.Lookahead() != '\n' && l.Lookahead() != 0 {
					l.Advance(true)
				}
			} else if swIsSpace(l.Lookahead()) {
				// An operator not normally treated as one; stop.
				return swStopNothingFound, 0
			}
			for swIsSpace(l.Lookahead()) {
				l.Advance(true)
			}
		}

		if _, saw := swEatOperators(l, ok, false, 0); saw {
			// The operator suppresses the newline.
			return swStopNothingFound, 0
		}
		// Promote the newline to an explicit token so operators are not
		// checked again.
		result = swImplicitSemi
		directive = swStopTokenFound
	}

	// Characters that suppress the semicolon without being consumed.
	if directive == swContinueTokenFound {
		for _, c := range swNonConsumingCrossSemiChars {
			if c == lookahead {
				return swContinueNothingFound, 0
			}
		}
	}

	if semiValid && directive != swContinueNothingFound {
		if lookahead == ';' {
			result = swExplicitSemi
		} else {
			result = swImplicitSemi
		}
		return directive, result
	}
	return swContinueNothingFound, 0
}

func swEatRawStrPart(s *swiftScanState, l *gts.ExternalLexer, ok func(int) bool) (int, bool) {
	hashCount := s.rawStrHashCount
	if !ok(swRawStrPart) {
		return 0, false
	} else if hashCount == 0 {
		// The first part: count the opening hashes.
		for l.Lookahead() == '#' {
			hashCount++
			l.Advance(false)
		}
		if hashCount == 0 {
			return 0, false
		}
		if l.Lookahead() != '"' {
			return 0, false
		}
		l.Advance(false)
	} else if ok(swRawStrContinuingIndicator) {
		// After an interpolation: the grammar just closed one, nothing to do.
	} else {
		return 0, false
	}

	// Everything but hashCount hashes in a row belongs to the string. The
	// character before the hashes decides what comes next.
	for l.Lookahead() != 0 {
		// lastChar is a uint8 in C: a rune is truncated to its low byte.
		var lastChar uint8
		l.MarkEnd()
		for l.Lookahead() != '#' && l.Lookahead() != 0 {
			lastChar = uint8(l.Lookahead())
			l.Advance(false)
			if lastChar != '\\' || l.Lookahead() == '\\' {
				// Not past a lone `\`, which stays out of the token.
				l.MarkEnd()
			}
		}

		var current uint32
		for l.Lookahead() == '#' && current < hashCount {
			current++
			l.Advance(false)
		}

		if current == hashCount {
			if lastChar == '\\' && l.Lookahead() == '(' {
				// An interpolation: leave `\#(` to the grammar.
				s.rawStrHashCount = hashCount
				return swRawStrPart, true
			} else if lastChar == '"' {
				// The end of the string, through the last hash.
				l.MarkEnd()
				s.rawStrHashCount = 0
				return swRawStrEndPart, true
			}
		}
	}
	return 0, false
}

func (sc swiftScanner) Scan(payload any, l *gts.ExternalLexer, valid []bool) bool {
	s := payload.(*swiftScanState)
	ok := func(i int) bool { return i < len(valid) && valid[i] }
	emit := func(tok int) bool {
		l.SetResultSymbol(sc.lang.ExternalSymbols[tok])
		return true
	}

	wsDirective, wsResult := swEatWhitespace(l, ok)
	if wsDirective == swStopTokenFound {
		return emit(wsResult)
	}
	if wsDirective == swStopNothingFound || wsDirective == swStopEOF {
		return false
	}
	hasWsResult := wsDirective == swContinueTokenFound

	// Comments before custom operators, so those are not read as comments.
	comment, commentResult := wsDirective, 0
	if wsDirective != swContinueSlashConsumed {
		comment, commentResult = swEatComment(l, true)
	}
	if comment == swStopTokenFound {
		l.MarkEnd()
		return emit(commentResult)
	}
	if comment == swStopEOF {
		return false
	}

	// Operators that may suppress the whitespace's semicolon.
	var prior rune
	if comment == swContinueSlashConsumed {
		prior = '/'
	}
	opResult, sawOp := swEatOperators(l, ok, !hasWsResult, prior)
	if sawOp && (!hasWsResult || swIsCrossSemiToken(opResult)) {
		if hasWsResult {
			l.MarkEnd()
		}
		return emit(opResult)
	}

	if hasWsResult {
		// No mark_end: operators may have been advanced through.
		return emit(wsResult)
	}

	// Consumes any `#` it sees even without a result, so it goes last.
	if tok, saw := swEatRawStrPart(s, l, ok); saw {
		return emit(tok)
	}
	return false
}
