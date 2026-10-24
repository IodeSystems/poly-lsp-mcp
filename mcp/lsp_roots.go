package mcp

import (
	"errors"
	"sort"

	"github.com/iodesystems/poly-lsp-mcp/multiplex"
	"github.com/iodesystems/poly-lsp-mcp/setup"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// --lsp-roots: one child LSP per language (single, the default), or one
// per (language, project root) (auto). See multiplex.Manager.Roots for the
// routing and setup/roots.go for the hints that offer and revert it.
//
// In single mode the project-root search runs only when a hint could
// follow from it — the index budget was blown, or a child was stopped
// for memory — so the default bring-up pays nothing for it.

// SetLSPRoots turns on one child LSP per project root (auto), with at
// most max running per language (0 = 8). Set before Init/Serve.
func (s *Server) SetLSPRoots(auto bool, max int) {
	s.lspRootsAuto = auto
	s.lspRootsMax = max
}

func (s *Server) lspRootsLimit() int {
	if s.lspRootsMax > 0 {
		return s.lspRootsMax
	}
	return 8
}

// projectRoots searches the workspace for project roots once per server,
// with the ignore set the last build used.
func (s *Server) projectRoots() symbols.ProjectRoots {
	s.rootsMu.Lock()
	defer s.rootsMu.Unlock()
	if s.roots == nil {
		root := s.getRoot()
		ign := s.getIgnores()
		if ign == nil {
			ign = s.loadIgnores(root)
		}
		pr := symbols.DetectProjectRoots(root, ign, symbols.DefaultRootsTime, symbols.DefaultRootsDirs)
		s.roots = &pr
	}
	return *s.roots
}

// lspRoots is the project roots of each language in langs that has a
// child LSP configured.
func (s *Server) lspRoots(langs []string) map[string][]string {
	pr := s.projectRoots()
	out := map[string][]string{}
	for _, lang := range langs {
		l := s.registry.LookupByName(lang)
		if l == nil || l.LSP == nil || len(pr.Roots[lang]) == 0 {
			continue
		}
		out[lang] = pr.Roots[lang]
	}
	return out
}

// rootsHints are the --lsp-roots lines for the setup notification and
// the budget remedy, and the roots they are about, relative to the
// workspace root. auto: what runs per root and how to revert. single: an
// offer to refine, for each language with more than one root — all of
// them when budget is set, else those with a memory-stopped child.
func (s *Server) rootsHints(budget bool) ([]string, map[string][]string) {
	langs := s.IndexCost().Languages
	var names []string
	if s.lspRootsAuto || budget {
		for lang, n := range langs {
			if n > 0 {
				names = append(names, lang)
			}
		}
	} else {
		names = s.memoryStoppedLanguages()
	}
	if len(names) == 0 {
		return nil, nil
	}
	sort.Strings(names)
	root := s.getRoot()
	roots := s.lspRoots(names)
	var hints []string
	rel := map[string][]string{}
	for _, lang := range names {
		dirs := roots[lang]
		if len(dirs) == 0 || (!s.lspRootsAuto && len(dirs) < 2) {
			continue
		}
		for _, d := range dirs {
			rel[lang] = append(rel[lang], symbols.RelRoot(root, d))
		}
		lsp := s.registry.LookupByName(lang).LSP.Cmd
		if s.lspRootsAuto {
			hints = append(hints, setup.RootsAuto(lang, lsp, rel[lang], s.lspRootsLimit()))
		} else {
			hints = append(hints, setup.RootsRefine(lang, lsp, len(dirs), s.lspRootsLimit()))
		}
	}
	if len(hints) == 0 {
		return nil, nil
	}
	if b := s.projectRoots().Bounded; b != "" {
		hints = append(hints, setup.RootsBounded(b))
	}
	return hints, rel
}

// memoryStoppedLanguages are the languages with a child stopped for
// memory at least once.
func (s *Server) memoryStoppedLanguages() []string {
	if s.manager == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, st := range s.manager.Status() {
		if st.MemoryStops > 0 && !seen[st.Language] {
			seen[st.Language] = true
			out = append(out, st.Language)
		}
	}
	return out
}

// onChildStop is the manager's OnStop hook. The first memory stop of a
// language's child re-sends the setup notification, which then carries
// the refine hint if the language has several project roots. Run off the
// monitor goroutine: the root search can take up to DefaultRootsTime.
func (s *Server) onChildStop(lang, _, reason string) {
	if reason != multiplex.StopMemory || s.lspRootsAuto {
		return
	}
	s.hintMu.Lock()
	first := !s.memHinted[lang]
	if s.memHinted == nil {
		s.memHinted = map[string]bool{}
	}
	s.memHinted[lang] = true
	s.hintMu.Unlock()
	if first {
		go s.notifySetup()
	}
}

// budgetBlown reports whether err is an index BudgetError.
func budgetBlown(err error) bool {
	var be *symbols.BudgetError
	return errors.As(err, &be)
}
