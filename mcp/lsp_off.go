package mcp

import (
	"log"
	"path/filepath"
	"strings"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// Per-client child-LSP switch: lsp_off / lsp_on.
//
// A child LSP is shared by every client of a root (one per language, or
// per project root). The switch does not stop it; it filters what ONE
// client sees. For a language off for a client:
//
//   - node_query edges are not resolved through the child (the
//     tree-sitter index answers, lexical / unsettled);
//   - its edits get no diagnostics and are validated by a tree-sitter
//     parse only (validationSkipped: lsp-off);
//   - sibling diagnostics from that language are left out.
//
// A child starts only when at least one client of the root wants its
// language (Manager.Defer, lspWanted): a language every client turned off
// is left stopped, and starts on the first request of a client that
// wants it. It stops under the usual lifecycle (idle, memory, the root's
// last client gone), not when the client that wanted it leaves.
//
// A language is off for a client when it names it in lsp_off, or when it
// is off_by_default (servers.yaml) and the client does not name it in
// lsp_on. See config.Registry.LSPOff.

// LSPPrefs are a client's lsp_on / lsp_off language names.
type LSPPrefs struct {
	On  []string `json:"lspOn,omitempty"`
	Off []string `json:"lspOff,omitempty"`
}

// SetLSPPrefs sets this server's own client's switch (a stdio server:
// mcp --lsp-off / --lsp-on). Call before the bring-up.
func (s *Server) SetLSPPrefs(p LSPPrefs) { s.SetSessionLSP("", p) }

// SetSessionLSP sets a daemon session's switch. The daemon sets it from
// the session's /open, before the bring-up when the open builds the root.
// Names the registry does not hold are logged and ignored.
func (s *Server) SetSessionLSP(sess string, p LSPPrefs) {
	if s.registry != nil {
		for _, n := range append(append([]string{}, p.On...), p.Off...) {
			if s.registry.LookupByName(n) == nil {
				log.Printf("mcp: lsp_on/lsp_off: no language %q; ignored", n)
			}
		}
	}
	s.sessionWaitsMu.Lock()
	defer s.sessionWaitsMu.Unlock()
	if s.sessionLSP == nil {
		s.sessionLSP = map[sessionID]LSPPrefs{}
	}
	s.sessionLSP[normSession(sess)] = p
}

// lspOff is the set of languages off for sess.
func (s *Server) lspOff(sess sessionID) map[string]bool {
	if s.registry == nil {
		return nil
	}
	s.sessionWaitsMu.Lock()
	p := s.sessionLSP[sess]
	s.sessionWaitsMu.Unlock()
	return s.registry.LSPOff(p.On, p.Off)
}

// langOfPath is the registry language name of a path, "" for none.
func (s *Server) langOfPath(path string) string {
	if s.registry == nil {
		return ""
	}
	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	if ext == "" {
		return ""
	}
	if l := s.registry.LookupByExt(ext); l != nil {
		return l.Name
	}
	return ""
}

// lspOffURI reports whether uri's language is off in the set off.
func (s *Server) lspOffURI(off map[string]bool, uri string) bool {
	if len(off) == 0 {
		return false
	}
	return off[s.langOfPath(uriToPath(uri))]
}

// lspWanted reports whether any client of this server wants lang's child
// LSP: one with a switch that leaves it on, or, with no switch recorded
// (no client said), the defaults.
func (s *Server) lspWanted(lang string) bool {
	if s.registry == nil {
		return true
	}
	s.sessionWaitsMu.Lock()
	prefs := make([]LSPPrefs, 0, len(s.sessionLSP))
	for _, p := range s.sessionLSP {
		prefs = append(prefs, p)
	}
	s.sessionWaitsMu.Unlock()
	if len(prefs) == 0 {
		prefs = append(prefs, LSPPrefs{})
	}
	for _, p := range prefs {
		if !s.registry.LSPOff(p.On, p.Off)[lang] {
			return true
		}
	}
	return false
}

// ChildStates maps each language with a child the manager knows of
// (started, stopped, or left stopped until a client wants it) to its
// state: running, stopped or down. For tests and the daemon's status.
func (s *Server) ChildStates() map[string]string {
	out := map[string]string{}
	if s.manager == nil {
		return out
	}
	for _, st := range s.manager.Status() {
		if prev, ok := out[st.Language]; ok && prev == "running" {
			continue
		}
		out[st.Language] = st.State
	}
	return out
}

// lspOffNote is the validationNote of an edit to a language off for the
// client.
const lspOffNote = "this client turned the language's child LSP off (lsp_off / --lsp-off, or off_by_default without lsp_on); the edit was checked by a tree-sitter parse only"

// lspOffSkipNote is the validationNote of an edit in a batch or refactor
// to a language off for the client: no parse check runs there.
const lspOffSkipNote = "this client turned the language's child LSP off (lsp_off / --lsp-off, or off_by_default without lsp_on); edit kept, not validated"

// parseBroke reports whether out fails a tree-sitter parse that orig (nil:
// a create) passed. A language with no grammar never breaks.
func parseBroke(lang string, orig, out []byte) bool {
	if symbols.LanguageByName(lang) == nil {
		return false
	}
	if symbols.ParsesCleanly(lang, out) {
		return false
	}
	return orig == nil || symbols.ParsesCleanly(lang, orig)
}
