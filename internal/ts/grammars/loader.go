// Package grammars is the pure-Go runtime's copy of the tree-sitter grammars
// the cgo runtime links: smacker/go-tree-sitter's parser.c files, converted
// to gotreesitter blobs by ts2go (generate.sh; every *.go here but this file
// and the *_scanner.go ports is generated).
//
// Why not gotreesitter's own grammars: several are different grammars or
// different revisions from smacker's, so the same source gave a different
// tree and the symbol walk, written against smacker's, read it wrong:
//
//   - sql: m-novikov/tree-sitter-sql instead of DerekStride's — no
//     create_table, no keyword_* nodes; every SQL consumer here found nothing.
//   - kotlin: a newer fwcd revision (extension receivers wrapped in
//     receiver_type), with a different external-token list.
//   - go: a newer revision (statement_list wrapper, string content nodes,
//     no anonymous newline tokens) — all 150 Go files of a local corpus parsed
//     differently.
//
// With these, all 150 of those Go files parse to the C runtime's tree. What
// still differs is the runtime: error recovery, and GLR ambiguity resolution
// in kotlin/tsx/c/swift (see keepRuntimeName for the measured parity). For
// swift it is most of the gap: `x.y` in an expression is a simple_identifier
// target in C and a user_type here. Declarations are rarely affected: of the
// 126 files of that corpus the C runtime parses cleanly, 125 give the same
// symbols in both runtimes.
//
// Scanners: sql, kotlin, tsx and swift are ports of smacker's C scanners
// (here, in *_scanner.go); gotreesitter's own scanners for those are for
// other externals lists. python, cpp and markdown reuse gotreesitter's scanners,
// whose externals match smacker's.
//
// Only internal/ts's !cgo side imports this package, so a cgo build does not
// link it.
package grammars

//go:generate sh generate.sh

import (
	"embed"
	"sync"

	gotreesitter "github.com/odvcencio/gotreesitter"
	// Only the scanner ports, not gotreesitter/grammars: that package embeds
	// all ~200 of its grammar blobs.
	grammarruntime "github.com/odvcencio/gotreesitter/grammars/runtime"
)

//go:embed grammar_blobs/*.bin
var blobs embed.FS

var (
	lexStates = map[string][][]bool{}
	loaded    sync.Map // blob name -> func() *gotreesitter.Language
)

// RegisterExternalLexStates is called by the generated *_external_lex_states_gen.go.
func RegisterExternalLexStates(name string, states [][]bool) { lexStates[name] = states }

// loadEmbeddedLanguage is what the generated <lang>.go constructors call. Each
// grammar decodes once per process, on first use: measured 5-77 ms apiece
// for gotreesitter's blobs, too much to pay for all twelve at startup.
func loadEmbeddedLanguage(blob string) *gotreesitter.Language {
	f, _ := loaded.LoadOrStore(blob, sync.OnceValue(func() *gotreesitter.Language { return decode(blob) }))
	return f.(func() *gotreesitter.Language)()
}

// decode finishes a blob the way gotreesitter's own loader does: lex-mode
// repair, the external scanner and its lex states, then the recovery
// certification that reads both.
func decode(blob string) *gotreesitter.Language {
	data, err := blobs.ReadFile("grammar_blobs/" + blob)
	if err != nil {
		panic("grammars: " + err.Error())
	}
	l, err := gotreesitter.LoadLanguage(data)
	if err != nil {
		panic("grammars: decode " + blob + ": " + err.Error())
	}
	name := blob[:len(blob)-len(".bin")]
	// The repair turns a state whose only real lookaheads are external
	// tokens into a no-lookahead state. Swift ends every statement on the
	// scanner's _implicit_semi, so with the repair the second line of any
	// file was an ERROR.
	if name != "swift" {
		gotreesitter.RepairNoLookaheadLexModes(l)
	}
	if st, ok := lexStates[name]; ok {
		l.ExternalLexStates = st
	}
	attachScanner(name, l)
	if !keepRuntimeName[name] {
		l.Name = "smacker_" + name
	}
	gotreesitter.CertifyCRecoveryCostCompetition(l)
	return l
}

// keepRuntimeName lists the grammars that keep their plain name.
//
// gotreesitter keys parse-time heuristics and result rewrites on
// Language.Name (parser_result_python.go, glr.go, parser_limits.go, ...),
// tuned for ITS revision of each grammar. On a different revision they can
// help or hurt, so the choice is measured: files whose tree matched the cgo
// runtime's exactly, on a local corpus (2026-10-24), plain name vs renamed:
//
//	kotlin 132 vs 117 of 150   tsx 81 vs 58 of 100   c 33 vs 30 of 60
//	python  9 vs  91 of 100    cpp 12 vs 40 of 60    java 96 vs 96 of 100
//	swift  36 vs  36 of 150 (2026-10-27; the plain name parsed 8x slower
//	and left 36 files with errors vs 28; the C runtime leaves 26)
//
// Renamed (no heuristics) unless the plain name measured better.
var keepRuntimeName = map[string]bool{"kotlin": true, "tsx": true, "c": true}

func attachScanner(name string, l *gotreesitter.Language) {
	if len(l.ExternalSymbols) == 0 {
		return
	}
	switch name {
	case "sql":
		// gotreesitter's sql scanner is for m-novikov's grammar.
		l.ExternalScanner = sqlScanner{lang: l}
	case "kotlin":
		// gotreesitter's kotlin scanner is for a newer externals list.
		l.ExternalScanner = kotlinScanner{lang: l}
	case "tsx":
		// gotreesitter's tsx scanner has jsx_text at index 7; this grammar
		// does not.
		l.ExternalScanner = tsxScanner{lang: l}
	case "swift":
		// gotreesitter's swift scanner is for a newer externals list.
		l.ExternalScanner = swiftScanner{lang: l}
	// The rest are gotreesitter's ports of the same upstream scanners. They
	// bind their token slots to this grammar's externals by position, and
	// smacker's externals lists for these are the ones they were written for.
	case "python":
		l.ExternalScanner = grammarruntime.PythonExternalScanner{}.ExternalScannerForLanguage(l)
	case "cpp":
		l.ExternalScanner = grammarruntime.CppExternalScanner{}.ExternalScannerForLanguage(l)
	case "markdown":
		l.ExternalScanner = grammarruntime.MarkdownExternalScanner{}.ExternalScannerForLanguage(l)
	default:
		panic("grammars: no external scanner for " + name)
	}
}
