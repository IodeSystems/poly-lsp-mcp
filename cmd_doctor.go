package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iodesystems/poly-lsp-mcp/daemon"
	"github.com/iodesystems/poly-lsp-mcp/internal/git"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// errDoctor: doctor printed its findings and found a problem; exit 1 with
// no further message.
var errDoctor = errors.New("doctor found problems")

// lspInstall says how to get each default child LSP, and lspVersionArgs how
// to ask it its version. A config may name others; those get no hint.
var (
	lspInstall = map[string]string{
		"gopls":                      "go install golang.org/x/tools/gopls@latest",
		"typescript-language-server": "npm install -g typescript-language-server typescript",
		"pylsp":                      "pipx install python-lsp-server   (or: pip install python-lsp-server)",
		"clangd":                     "apt install clangd   ·   brew install llvm (macOS; or the Xcode command line tools' clangd)",
	}
	lspVersionArgs = map[string][]string{
		"gopls":                      {"version"},
		"typescript-language-server": {"--version"},
		"pylsp":                      {"--version"},
		"clangd":                     {"--version"},
	}
)

// doctorLang is one language's backend check.
type doctorLang struct {
	Language string `json:"language"`
	Files    int    `json:"files"` // indexable files in the workspace
	LSP      string `json:"lsp,omitempty"`
	Path     string `json:"path,omitempty"`
	Version  string `json:"version,omitempty"`
	Missing  bool   `json:"missing,omitempty"`
	Install  string `json:"install,omitempty"`
}

type doctorReport struct {
	Binary     string       `json:"binary"`
	OnPath     string       `json:"onPath"` // what `poly-lsp-mcp` resolves to
	Build      string       `json:"build"`
	Platform   string       `json:"platform"`
	Config     string       `json:"config"`
	Root       string       `json:"root"`
	Partial    bool         `json:"partialCount,omitempty"`
	Languages  []doctorLang `json:"languages"`
	Git        string       `json:"git"`
	Daemon     string       `json:"daemon"`
	Problems   []string     `json:"problems,omitempty"`
	Advisories []string     `json:"advisories,omitempty"`
}

// doctorCmd checks what poly-lsp needs to run well here: the binary on
// PATH, the child LSPs for the languages this workspace holds, git, and
// the daemon. A missing child LSP is not an error to the server — that
// language falls back to tree-sitter — but it costs resolved ::in/::out
// edges and edit validation, and nothing else says so.
func doctorCmd(l *loader) *cobra.Command {
	var (
		rootPath string
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the install: binary on PATH, child LSPs for this workspace's languages, git, daemon",
		Long: `Check what poly-lsp needs to run well here and say how to fix what is missing:
the binary PATH resolves to, the child LSP for each language the workspace
holds (gopls, typescript-language-server, pylsp, clangd), git, the daemon.

A language without its child LSP still works, from tree-sitter alone, but
its ::in/::out edges are not resolved and edits to it are not validated.
Exits 1 when a language the workspace holds has no child LSP, or PATH runs
a different poly-lsp-mcp than this one.`,
		Example: `  poly-lsp-mcp doctor
  poly-lsp-mcp doctor --root ../other --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, reg, err := l.load(false)
			if err != nil {
				return err
			}
			root, err := filepath.Abs(rootPath)
			if err != nil {
				return fmt.Errorf("root: %w", err)
			}
			rep := doctorReport{
				Platform: runtime.GOOS + "/" + runtime.GOARCH,
				Root:     root,
				Config:   "defaults (no " + l.configPath + ")",
			}
			if _, err := os.Stat(l.configPath); err == nil {
				rep.Config = l.configPath
			}

			// The binary, and what PATH runs. A stale copy earlier on PATH
			// (~/go/bin from an old `go install`) is the usual surprise.
			rep.Binary, _ = os.Executable()
			if r, err := filepath.EvalSymlinks(rep.Binary); err == nil {
				rep.Binary = r
			}
			rep.Build = buildInfo()
			copies := pathCopies("poly-lsp-mcp")
			if len(copies) == 0 {
				rep.OnPath = "not on PATH"
				rep.Advisories = append(rep.Advisories, "poly-lsp-mcp is not on PATH; MCP configs must use the full path "+rep.Binary)
			} else {
				rep.OnPath = copies[0]
				if copies[0] != rep.Binary {
					rep.Problems = append(rep.Problems, fmt.Sprintf("PATH runs %s, not this binary (%s); remove the stale one or put %s first", copies[0], rep.Binary, filepath.Dir(rep.Binary)))
				}
				if len(copies) > 1 {
					rep.Advisories = append(rep.Advisories, "other copies on PATH, behind the first (an MCP config naming one by full path runs it): "+strings.Join(copies[1:], ", "))
				}
			}

			ign := git.LoadIgnores(root)
			counts, partial := symbols.CountLanguages(root, reg, ign, 5*time.Second)
			rep.Partial = partial
			for _, lang := range reg.Languages() {
				d := doctorLang{Language: lang.Name, Files: counts[lang.Name]}
				if lang.LSP == nil {
					if d.Files > 0 {
						rep.Languages = append(rep.Languages, d)
					}
					continue
				}
				d.LSP = lang.LSP.Cmd
				if p, err := exec.LookPath(lang.LSP.Cmd); err != nil {
					d.Missing = true
					d.Install = lspInstall[lang.LSP.Cmd]
				} else {
					d.Path = p
					d.Version = probeVersion(p, lspVersionArgs[lang.LSP.Cmd])
				}
				if d.Missing && d.Files > 0 {
					msg := fmt.Sprintf("%s: %d file(s) but %s is not on PATH — tree-sitter only: ::in/::out unresolved, edits not validated", lang.Name, d.Files, d.LSP)
					if d.Install != "" {
						msg += "; install: " + d.Install
					}
					rep.Problems = append(rep.Problems, msg)
				}
				rep.Languages = append(rep.Languages, d)
			}
			sort.SliceStable(rep.Languages, func(i, j int) bool { return rep.Languages[i].Files > rep.Languages[j].Files })

			if p, err := exec.LookPath("git"); err != nil {
				rep.Git = "missing"
				rep.Advisories = append(rep.Advisories, "git is not on PATH: .gitignore is not applied, so ignored files are indexed")
			} else {
				rep.Git = p
				if ign == nil {
					rep.Git += " (root is not a git repository: nothing is ignored)"
				}
			}

			rep.Daemon = "not running"
			if st, ok := daemon.ReadState(); ok && daemon.PidAlive(st.PID) {
				rep.Daemon = fmt.Sprintf("running, pid %d, since %s", st.PID, st.StartedAt)
				rep.Advisories = append(rep.Advisories, "a daemon is running; after an upgrade restart it (poly-lsp-mcp daemon --restart) or --daemon clients keep the old binary")
			}

			if asJSON {
				b, _ := json.MarshalIndent(rep, "", "  ")
				fmt.Println(string(b))
			} else {
				printDoctor(rep)
			}
			if len(rep.Problems) > 0 {
				return errDoctor
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&rootPath, "root", ".", "workspace to check the languages of")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the findings as JSON")
	return cmd
}

func printDoctor(r doctorReport) {
	fmt.Printf("binary    %s\n          %s, %s\n", r.Binary, r.Build, r.Platform)
	fmt.Printf("on PATH   %s\n", r.OnPath)
	fmt.Printf("config    %s\n", r.Config)
	fmt.Printf("git       %s\n", r.Git)
	fmt.Printf("daemon    %s\n", r.Daemon)
	note := ""
	if r.Partial {
		note = " (count stopped at 5 s; partial)"
	}
	fmt.Printf("\nlanguages in %s%s:\n", r.Root, note)
	unused := 0
	for _, d := range r.Languages {
		if d.Files == 0 {
			unused++ // in --json; a missing LSP for an absent language is noise
			continue
		}
		state := "tree-sitter only (no child LSP configured)"
		switch {
		case d.Missing:
			state = d.LSP + " MISSING"
		case d.LSP != "":
			state = d.LSP + " " + d.Version
		}
		fmt.Printf("  %-12s %6d files  %s\n", d.Language, d.Files, state)
	}
	if unused > 0 {
		fmt.Printf("  (%d more configured language(s) with no files here; --json lists them)\n", unused)
	}
	for _, p := range r.Problems {
		fmt.Printf("\nPROBLEM  %s\n", p)
	}
	for _, a := range r.Advisories {
		fmt.Printf("\nnote     %s\n", a)
	}
	if len(r.Problems) == 0 {
		fmt.Println("\nok")
	}
}

// pathCopies lists every distinct file named name on PATH, in PATH order,
// symlinks resolved. Unlike exec.LookPath it keeps a relative PATH entry
// ("bin"): the shell runs it, so doctor must report it.
func pathCopies(name string) []string {
	var out []string
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		p, err := filepath.Abs(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// buildInfo is the module version or VCS revision this binary was built
// from — enough to tell two installs apart.
func buildInfo() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "build info unavailable"
	}
	var rev, at, dirty string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			at = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}
		return fmt.Sprintf("commit %s%s (%s), %s", rev, dirty, at, bi.GoVersion)
	}
	return fmt.Sprintf("%s %s, %s", bi.Main.Path, bi.Main.Version, bi.GoVersion)
}

// probeVersion runs an LSP's version flag, bounded; first line only.
func probeVersion(path string, args []string) string {
	if args == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil && len(out) == 0 {
		return "(version unknown)"
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line
}
