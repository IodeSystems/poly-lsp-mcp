package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/iodesystems/poly-lsp-mcp/daemon"
	"github.com/iodesystems/poly-lsp-mcp/internal/git"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
)

// daemonCmd runs (or controls) the shared per-user daemon: one process
// hosting many workspace roots over a unix socket, gated by peer
// credentials and declared root prefixes. --stop / --restart control an
// already-running daemon and exit.
func daemonCmd(l *loader) *cobra.Command {
	var (
		socket     string
		readOnly   bool
		validate   bool
		noValidate bool
		stop       bool
		restart    bool
		buildID    bool
		allow      multiFlag
	)
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run, stop or restart the shared per-user daemon",
		Long: `Run the shared per-user daemon: one process hosting many workspace roots
over a unix socket. 'poly-lsp-mcp mcp --daemon' starts it on demand and
proxies to it. --stop and --restart control a running daemon and exit.`,
		Args: cobra.NoArgs,
	}
	f := cmd.Flags()
	f.StringVar(&socket, "socket", "", "unix socket path (default $XDG_RUNTIME_DIR/poly-lsp/daemon.sock)")
	f.BoolVar(&readOnly, "read-only", false, "host every root read-only (mutating tools hidden)")
	// Same default as the stdio server (see mcpCmd): on, degrading to
	// apply-and-flag where diagnostics are unavailable.
	f.BoolVar(&validate, "validate", true, "revert-on-new-diagnostics for every hosted root's edits")
	f.BoolVar(&noValidate, "no-validate", false, "turn OFF revert-on-new-diagnostics for hosted roots")
	f.BoolVar(&stop, "stop", false, "stop the running daemon and exit")
	f.BoolVar(&restart, "restart", false, "restart the running daemon (replaying its flags) and exit")
	f.Var(&allow, "allow", "directory prefix a client may address roots under (repeatable; default $HOME)")
	// Asked of a candidate binary before an upgrade runs it (ProbeBuild):
	// it says which build it is, and that it runs at all.
	f.BoolVar(&buildID, "build-id", false, "print this binary's build identity and exit")
	_ = f.MarkHidden("build-id")
	lifecycle := lifecycleFlags(f)
	rootsFlag := lspRootsFlags(f)
	// For every hosted root; a client's /open adds its own excludes and
	// includes and may set the rest (mcp --daemon passes those it is given).
	idx := indexFlags(f)
	warmRecent := f.Duration("warm-recent", 30*time.Minute, "an upgrade's new daemon warms the roots requested within this long before it takes over; older roots come up on their next request")
	// Set by a daemon starting its replacement (blue/green; daemon/takeover.go).
	takeoverFrom := f.Int("takeover-from", 0, "pid of the daemon this one replaces")
	var warm multiFlag
	f.Var(&warm, "warm", "root to bring up before taking over (repeatable)")
	_ = f.MarkHidden("takeover-from")
	_ = f.MarkHidden("warm")

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if buildID {
			fmt.Println(daemon.BuildID())
			return nil
		}
		if stop {
			return daemon.Stop()
		}
		roots, err := rootsFlag()
		if err != nil {
			return err
		}
		if noValidate {
			validate = false
		}
		// What this daemon was started with, recorded in daemon.json: a bare
		// --restart replays it, and a takeover starts the new build with it.
		// A takeover daemon's --socket is its side socket, not a choice to
		// replay; it records the main one when it takes over.
		skip := []string{"stop", "restart", "help", "build-id", "takeover-from", "warm"}
		if *takeoverFrom != 0 {
			skip = append(skip, "socket")
		}
		args := replayFlags(cmd.Flags(), skip...)
		if restart {
			if len(args) == 0 {
				if st, ok := daemon.ReadState(); ok {
					args = st.Args
				}
			}
			return daemon.Restart(args)
		}
		for _, d := range idx.excludes {
			if git.CleanExclude(d) == "" {
				return fmt.Errorf("--exclude %q: want a directory inside the root, relative to it", d)
			}
		}
		if idx.symlinkDepth < 0 {
			return fmt.Errorf("--symlink-depth %d: want 0 or more", idx.symlinkDepth)
		}
		if err := mcp.New(nil, "", nil, nil).SetIndexBudget(idx.budget); err != nil {
			return err
		}

		cfg, reg, err := l.load(true)
		if err != nil {
			return err
		}

		prefixes := []string(allow)
		if len(prefixes) == 0 {
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				prefixes = []string{home}
			}
		}
		al := daemon.NewAllowList(prefixes)
		if len(al.Prefixes()) == 0 {
			return errors.New("daemon: no usable --allow prefix (and no home dir); refusing to start with an empty allow-list")
		}

		dreg := daemon.NewRegistry(cfg, reg, readOnly, validate)
		dreg.SetLifecycle(lifecycle())
		dreg.SetLSPRoots(roots.auto, roots.max)
		dreg.SetStatusDir(daemon.StatusDir())
		dreg.SetUsageLog(daemon.UsagePath())
		dreg.SetIndexDefaults(daemon.IndexDefaults{Excludes: idx.excludes, Includes: idx.includes,
			Budget: idx.budget, Anyway: idx.anyway, SymlinkDepth: idx.symlinkDepth})
		return daemon.Serve(daemon.Config{Socket: socket, Allow: al, Reg: dreg, Args: args,
			TakeoverFrom: *takeoverFrom, Warm: warm, WarmRecent: *warmRecent})
	}
	return cmd
}

// replayFlags returns the flags set on this command line as --name=value
// arguments, without the named control flags, so --restart can relaunch the
// daemon with the same configuration. A repeatable flag replays each value.
func replayFlags(fs *pflag.FlagSet, skip ...string) []string {
	drop := map[string]bool{}
	for _, n := range skip {
		drop[n] = true
	}
	var out []string
	fs.Visit(func(fl *pflag.Flag) {
		if drop[fl.Name] {
			return
		}
		if m, ok := fl.Value.(*multiFlag); ok {
			for _, v := range *m {
				out = append(out, "--"+fl.Name+"="+v)
			}
			return
		}
		// A StringArray (--include) prints as "[a,b]"; replay each value.
		if s, ok := fl.Value.(pflag.SliceValue); ok {
			for _, v := range s.GetSlice() {
				out = append(out, "--"+fl.Name+"="+v)
			}
			return
		}
		out = append(out, "--"+fl.Name+"="+fl.Value.String())
	})
	return out
}
