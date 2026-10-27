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
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iodesystems/poly-lsp-mcp/daemon"
	"github.com/iodesystems/poly-lsp-mcp/internal/git"
	"github.com/iodesystems/poly-lsp-mcp/internal/ts"
	"github.com/iodesystems/poly-lsp-mcp/setup"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// errDoctor: doctor printed its findings and found a problem; exit 1 with
// no further message.
var errDoctor = errors.New("doctor found problems")

// lspVersionArgs asks each default child LSP its version. A config may
// name others; those are reported without one.
var lspVersionArgs = map[string][]string{
	"gopls":                      {"version"},
	"typescript-language-server": {"--version"},
	"pylsp":                      {"--version"},
	"clangd":                     {"--version"},
}

type doctorReport struct {
	Binary    string            `json:"binary"`
	OnPath    string            `json:"onPath"` // what `poly-lsp-mcp` resolves to
	Build     string            `json:"build"`
	BuildID   string            `json:"buildId"` // daemon.BuildID: what the daemon handshake compares
	Platform  string            `json:"platform"`
	Runtime   string            `json:"runtime"` // tree-sitter runtime: "cgo" or "pure-go"
	Config    string            `json:"config"`
	Root      string            `json:"root"`
	Partial   bool              `json:"partialCount,omitempty"`
	Languages []setup.Lang      `json:"languages"`          // the shape of the poly-lsp-mcp/setup notification
	Versions  map[string]string `json:"versions,omitempty"` // child LSP → its version line
	Git       string            `json:"git"`
	Daemon    string            `json:"daemon"`
	// DaemonHome is the directory the daemon paths derive from, and Source
	// what chose it: --daemon-home, $POLY_LSP_HOME or the default.
	DaemonHome       string `json:"daemonHome"`
	DaemonHomeSource string `json:"daemonHomeSource"`
	// DaemonBuild is the running daemon's build identity; DaemonMatches
	// whether it is this binary's (daemon.BuildID). Absent with no daemon.
	DaemonBuild   string `json:"daemonBuild,omitempty"`
	DaemonMatches *bool  `json:"daemonMatches,omitempty"`
	// DaemonRoots are the running daemon's roots: clients, resident
	// estimate (bytes indexed × a measured factor; see daemon.RootStat);
	// DaemonIdleCap bounds the client-less ones (bytes, < 0 none).
	DaemonRoots   []daemon.RootStat `json:"daemonRoots,omitempty"`
	DaemonIdleCap *int64            `json:"daemonIdleRootsCap,omitempty"`
	// UpgradeFailures are the builds a daemon upgrade failed to; PrevExe
	// the binary kept from before the last upgrade.
	UpgradeFailures map[string]daemon.UpgradeFailure `json:"upgradeFailures,omitempty"`
	PrevExe         string                           `json:"prevExe,omitempty"`
	Problems        []string                         `json:"problems,omitempty"`
	Advisories      []string                         `json:"advisories,omitempty"`
	Update          *updateInfo                      `json:"update,omitempty"` // absent when nothing is known

	// Roots are the project roots of each language with a child LSP,
	// relative to Root: where --lsp-roots auto would start one each.
	Roots        map[string][]string `json:"projectRoots,omitempty"`
	RootsBounded string              `json:"projectRootsBounded,omitempty"`
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
		noUpdate bool
	)
	var daemonHome func() (string, error)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the install: binary on PATH, child LSPs for this workspace's languages, git, daemon",
		Long: `Check what poly-lsp needs to run well here and say how to fix what is missing:
the binary PATH resolves to, the child LSP for each language the workspace
holds (gopls, typescript-language-server, pylsp, clangd, sourcekit-lsp), git,
the daemon. An Xcode project without a buildServer.json gets the
xcode-build-server command sourcekit-lsp needs to resolve across files.

A language without its child LSP still works, from tree-sitter alone, but
its ::in/::out edges are not resolved and edits to it are not validated.
Exits 1 when a language the workspace holds has no child LSP, or PATH runs
a different poly-lsp-mcp than this one.

It also checks, at most once a day and quietly offline, whether a newer
poly-lsp-mcp exists for how this binary was built (release, go install, or a
checkout named by LSP_REBUILD_PATH). --no-update-check skips it.`,
		Example: `  poly-lsp-mcp doctor
  poly-lsp-mcp doctor --root ../other --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fromEnv := os.Getenv(daemon.HomeEnv) != ""
			home, err := daemonHome()
			if err != nil {
				return err
			}
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
				Runtime:  ts.Runtime(),
				Root:     root,
				Config:   "defaults (no " + l.configPath + ")",

				DaemonHome:       daemon.ConfigHome(),
				DaemonHomeSource: "default",
			}
			switch {
			case cmd.Flags().Changed("daemon-home"):
				rep.DaemonHomeSource = "--daemon-home"
			case home != "" && fromEnv:
				rep.DaemonHomeSource = "$" + daemon.HomeEnv
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
			rep.BuildID = daemon.BuildID()
			// `go install` with no C compiler on the machine builds the pure-Go
			// parser without a word; this is the one place that says so.
			if rep.Runtime == "pure-go" {
				rebuild, _ := rebuildDir()
				rep.Advisories = append(rep.Advisories, ts.FastPathHint+": "+rebuildCmd(classifyBuild(readBuildInfo()), rebuild))
			}
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
			// A root the server would refuse: say so, and do not count its
			// languages — they would be every repository's under it.
			np := symbols.CheckProjectRoot(root)
			if np != nil {
				msg := np.Error()
				if np.Total > 0 {
					msg += fmt.Sprintf("; git repositories under it (%d): %s", np.Total, strings.Join(np.Repos, ", "))
					if np.Total > len(np.Repos) {
						msg += fmt.Sprintf(" … and %d more", np.Total-len(np.Repos))
					}
				}
				rep.Problems = append(rep.Problems, msg+". The server will not index it: "+np.Remedy())
			}
			counts := map[string]int{}
			if np == nil {
				counts, rep.Partial = symbols.CountLanguages(root, reg, ign, 5*time.Second)
			}
			rep.Languages = setup.Hints(reg, counts)
			rep.Versions = map[string]string{}
			for _, l := range rep.Languages {
				switch {
				case l.LSP == nil:
				case l.LSP.Found && l.LSP.Name == "sourcekit-lsp":
					// No --version: the toolchain's swift beside it says.
					v := setup.SwiftVersion(l.LSP.Path)
					rep.Versions[l.LSP.Name] = v
					if h := setup.SourceKitIndexHint(v); h != "" {
						rep.Advisories = append(rep.Advisories, h)
					}
				case l.LSP.Found:
					rep.Versions[l.LSP.Name] = probeVersion(l.LSP.Path, lspVersionArgs[l.LSP.Name])
				default:
					msg := fmt.Sprintf("%s: %d file(s) but %s is not on PATH — tree-sitter only; it would add %s", l.Lang, l.Files, l.LSP.Name, l.Adds)
					if l.Install != nil {
						msg += fmt.Sprintf("; install (needs %s): %s", strings.Join(l.Install.Needs, ", "), l.Install.Cmd)
					}
					rep.Problems = append(rep.Problems, msg)
				}
			}

			// No languages (a root refused above, or an empty one): no roots
			// to look for, and no 2 s search for them.
			var pr symbols.ProjectRoots
			if len(rep.Languages) > 0 {
				pr = symbols.DetectProjectRoots(root, ign, symbols.DefaultRootsTime, symbols.DefaultRootsDirs)
			}
			rep.RootsBounded = pr.Bounded
			for _, l := range rep.Languages {
				dirs := pr.Roots[l.Lang]
				if l.LSP == nil || len(dirs) == 0 {
					continue
				}
				if rep.Roots == nil {
					rep.Roots = map[string][]string{}
				}
				for _, d := range dirs {
					rep.Roots[l.Lang] = append(rep.Roots[l.Lang], symbols.RelRoot(root, d))
				}
				if len(dirs) > 1 {
					rep.Advisories = append(rep.Advisories, setup.RootsRefine(l.Lang, l.LSP.Name, len(dirs), 8))
				}
			}
			// An Xcode project without a build server: a hint, never a
			// problem — sourcekit-lsp still runs, file by file.
			if counts["swift"] > 0 {
				rep.Advisories = append(rep.Advisories, setup.XcodeHints(pr.Xcode, func(d string) string { return symbols.RelRoot(root, d) })...)
			}

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
				build := st.Build
				if build == "" {
					build = "unknown (a daemon from before build identities)"
				}
				match := st.Build == daemon.BuildID()
				rep.DaemonBuild, rep.DaemonMatches = build, &match
				if match {
					rep.Daemon += ", this build"
				} else {
					rep.Daemon += ", build " + build + ", NOT this binary's (" + daemon.BuildID() + ")"
					rep.Advisories = append(rep.Advisories, "the running daemon is another build; the next `mcp` client of this build asks it to upgrade (blue/green), or restart it now: poly-lsp-mcp daemon --restart")
				}
				// A daemon from before root stats answers without them.
				if h, err := daemon.NewClient(st.Socket).Health(2 * time.Second); err == nil {
					rep.DaemonRoots, rep.DaemonIdleCap = h.RootStats, &h.IdleRootsCap
				}
			}
			// Upgrades that failed, and the binary to go back to (see
			// daemon/rollback.go).
			if fails := daemon.LoadUpgradeFailures(); len(fails) > 0 {
				rep.UpgradeFailures = fails
				for build, f := range fails {
					state := "will be tried again"
					if f.Failures >= daemon.MaxUpgradeFailures {
						state = "REFUSED until " + daemon.UpgradeFailuresPath() + " is removed"
					}
					rep.Advisories = append(rep.Advisories, fmt.Sprintf("daemon upgrade to build %s failed %d time(s), last at %s: %s; %s", build, f.Failures, f.At, f.Error, state))
				}
			}
			if st, err := os.Stat(daemon.PrevExePath()); err == nil {
				rep.PrevExe = daemon.PrevExePath()
				rep.Advisories = append(rep.Advisories, fmt.Sprintf("the daemon binary before the last upgrade is kept at %s (%s); to go back to it: %s daemon --restart",
					rep.PrevExe, st.ModTime().Format(time.RFC3339), rep.PrevExe))
			}

			// Last: it is the only step that waits on the network.
			if !noUpdate {
				rep.Update = newUpdateChecker().check(classifyBuild(readBuildInfo()))
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
	cmd.Flags().BoolVar(&noUpdate, "no-update-check", false, "do not look for a newer poly-lsp-mcp")
	daemonHome = daemonHomeFlag(cmd.Flags())
	return cmd
}

func printDoctor(r doctorReport) {
	fmt.Printf("binary    %s\n          %s, %s\n          build id %s\n", r.Binary, r.Build, r.Platform, r.BuildID)
	fmt.Printf("runtime   tree-sitter %s\n", r.Runtime)
	fmt.Printf("on PATH   %s\n", r.OnPath)
	fmt.Printf("config    %s\n", r.Config)
	fmt.Printf("git       %s\n", r.Git)
	fmt.Printf("daemon    %s\n", r.Daemon)
	fmt.Printf("          home %s (%s); socket %s\n", r.DaemonHome, r.DaemonHomeSource, daemon.SocketPath())
	var idle int64
	for _, d := range r.DaemonRoots {
		who := fmt.Sprintf("%d client(s)", d.Clients)
		if d.Clients == 0 {
			who = "no client, used " + (time.Duration(d.IdleSec) * time.Second).String() + " ago"
			if d.LSPsStopped {
				who += ", child LSPs stopped"
			}
			idle += d.Resident
		}
		fmt.Printf("          root %s: %s, ~%.1f MiB resident (est.), %s\n", d.Root, d.State, float64(d.Resident)/(1<<20), who)
	}
	if len(r.DaemonRoots) > 0 && r.DaemonIdleCap != nil && *r.DaemonIdleCap >= 0 {
		fmt.Printf("          client-less roots ~%.1f of %d MiB (--idle-roots-mb)\n", float64(idle)/(1<<20), *r.DaemonIdleCap>>20)
	}
	if r.Update != nil {
		fmt.Printf("update    %s\n", r.Update.text())
	}
	note := ""
	if r.Partial {
		note = " (count stopped at 5 s; partial)"
	}
	fmt.Printf("\nlanguages in %s%s:\n", r.Root, note)
	for _, d := range r.Languages {
		state := "tree-sitter only (no child LSP configured)"
		switch {
		case d.LSP == nil:
		case d.LSP.Found:
			state = d.LSP.Name + " " + r.Versions[d.LSP.Name]
		default:
			state = d.LSP.Name + " MISSING"
		}
		fmt.Printf("  %-12s %6d files  %s\n", d.Lang, d.Files, state)
	}
	if len(r.Roots) > 0 {
		note := ""
		if r.RootsBounded != "" {
			note = " (search " + r.RootsBounded + "; partial)"
		}
		fmt.Printf("\nproject roots%s:\n", note)
		for _, d := range r.Languages {
			rs := r.Roots[d.Lang]
			if len(rs) == 0 {
				continue
			}
			names, more := rs, ""
			if len(names) > 8 { // --json lists them all
				names, more = names[:8], fmt.Sprintf(", +%d more", len(rs)-8)
			}
			fmt.Printf("  %-12s %6d roots  %s%s\n", d.Lang, len(rs), strings.Join(names, ", "), more)
		}
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

// readBuildInfo adapts debug.ReadBuildInfo to classifyBuild's arguments.
func readBuildInfo() (*debug.BuildInfo, bool) {
	bi, _ := debug.ReadBuildInfo()
	return bi, releaseBuild
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
