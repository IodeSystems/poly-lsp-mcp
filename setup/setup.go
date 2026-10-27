// Package setup says what each language's child LSP would add and how to
// install it, for the languages a workspace actually holds.
//
// Without its child LSP a language still works, from tree-sitter alone, so
// the server never fails over a missing one — and so nothing said it was
// missing. A client (dun) relays these hints: one quiet line per missing
// LSP in a language the project uses. `doctor` prints the same thing for a
// person.
package setup

import (
	"os/exec"
	"sort"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

// Adds is what a child LSP adds over tree-sitter, in poly-lsp's terms.
const Adds = "resolved references (::in/::out), workspace rename, diagnostics and edit validation"

// Install is how to get one child LSP, and what that needs on the machine.
type Install struct {
	Cmd   string   `json:"cmd"`
	Needs []string `json:"needs"` // "go", "node", "python", "system"
}

// installs covers the default child LSPs. A config may name others; those
// get no install hint.
var installs = map[string]Install{
	"gopls":                      {Cmd: "go install golang.org/x/tools/gopls@latest", Needs: []string{"go"}},
	"typescript-language-server": {Cmd: "npm install -g typescript-language-server typescript", Needs: []string{"node"}},
	"pylsp":                      {Cmd: "pipx install python-lsp-server", Needs: []string{"python"}},
	"clangd":                     {Cmd: "apt install clangd  |  brew install llvm", Needs: []string{"system"}},
	// Part of every Swift toolchain; there is no separate package.
	"sourcekit-lsp": {Cmd: "macOS: xcode-select --install (or Xcode)  |  Linux: a swift.org toolchain (https://www.swift.org/install) or Nix (nix-shell -p swift sourcekit-lsp)", Needs: []string{"system"}},
}

// LSP is one child LSP's state on this machine.
type LSP struct {
	Name  string `json:"name"`
	Found bool   `json:"found"`
	Path  string `json:"path,omitempty"`
}

// Lang is one language the workspace holds.
type Lang struct {
	Lang    string   `json:"lang"`
	Files   int      `json:"files"`
	LSP     *LSP     `json:"lsp,omitempty"`     // nil: tree-sitter only by design
	Adds    string   `json:"adds,omitempty"`    // set when the LSP is missing
	Install *Install `json:"install,omitempty"` // set when missing and known
}

// Hints returns one entry per language with files > 0, most files first.
func Hints(reg *config.Registry, files map[string]int) []Lang {
	var out []Lang
	for _, lang := range reg.Languages() {
		n := files[lang.Name]
		if n == 0 {
			continue
		}
		l := Lang{Lang: lang.Name, Files: n}
		if lang.LSP != nil {
			l.LSP = &LSP{Name: lang.LSP.Cmd}
			if p, err := exec.LookPath(lang.LSP.Cmd); err == nil {
				l.LSP.Found, l.LSP.Path = true, p
			} else {
				l.Adds = Adds
				if in, ok := installs[lang.LSP.Cmd]; ok {
					in := in
					l.Install = &in
				}
			}
		}
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Files > out[j].Files })
	return out
}

// Missing counts the languages whose child LSP is configured but not found.
func Missing(langs []Lang) int {
	n := 0
	for _, l := range langs {
		if l.LSP != nil && !l.LSP.Found {
			n++
		}
	}
	return n
}
