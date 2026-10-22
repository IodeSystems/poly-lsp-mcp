package main

import (
	"errors"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/iodesystems/poly-lsp-mcp/daemon"
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
	lifecycle := lifecycleFlags(f)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if stop {
			return daemon.Stop()
		}
		if noValidate {
			validate = false
		}
		if restart {
			return daemon.Restart(replayFlags(cmd.Flags(), "stop", "restart", "help"))
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
		dreg.SetStatusDir(daemon.StatusDir())
		return daemon.Serve(daemon.Config{Socket: socket, Allow: al, Reg: dreg})
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
		out = append(out, "--"+fl.Name+"="+fl.Value.String())
	})
	return out
}
