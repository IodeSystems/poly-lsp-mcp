package setup

import (
	"fmt"
	"strings"
)

// Project-root hints: the info/refine pair for --lsp-roots.
//
// --lsp-roots single (the default) runs one child per language at the
// workspace root. That is what gopls and typescript-language-server want;
// pylsp, clangd and JVM servers given a monorepo resolve against the wrong
// package and hold every package at once. Nothing switches modes on its
// own: a hint offers auto when there is evidence it would help (the index
// budget was blown, or a child was stopped for memory), and auto says how
// to go back.

// manyRoots are the child LSPs that resolve several modules or packages
// correctly from one root. For them a split only bounds memory.
var manyRoots = map[string]bool{"gopls": true, "typescript-language-server": true}

// RootsRefine offers --lsp-roots auto for a language with n project roots
// under one child.
func RootsRefine(lang, lsp string, n, max int) string {
	why := fmt.Sprintf("one %s at the workspace root may resolve references wrong and grow large", lsp)
	if manyRoots[lsp] {
		why = fmt.Sprintf("one %s at the workspace root holds them all in memory", lsp)
	}
	return fmt.Sprintf("%s: %d project roots; %s. "+
		"Refine: --lsp-roots auto (one child per root, at most %d via --lsp-roots-max, idle-stopped)", lang, n, why, max)
}

// RootsAuto says what --lsp-roots auto did for a language, and how to
// revert. rel are the roots relative to the workspace root; at most five
// are named.
func RootsAuto(lang, lsp string, rel []string, max int) string {
	names := rel
	more := ""
	if len(names) > 5 {
		names, more = names[:5], fmt.Sprintf(", +%d more", len(rel)-5)
	}
	noun := "project roots"
	if len(rel) == 1 {
		noun = "project root"
	}
	return fmt.Sprintf("%s: %d %s (%s%s), one %s each, started on first use, at most %d running. "+
		"Revert: --lsp-roots single", lang, len(rel), noun, strings.Join(names, ", "), more, lsp, max)
}

// RootsBounded notes that the root search stopped early.
func RootsBounded(why string) string {
	return "project-root search " + why + "; there may be more roots"
}
