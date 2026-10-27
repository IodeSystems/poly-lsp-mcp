package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/daemon"
	"github.com/iodesystems/poly-lsp-mcp/internal/git"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

func mcpCmd(l *loader) *cobra.Command {
	var (
		rootPath       string
		legacyTools    bool
		readOnly       bool
		validate       bool
		noValidate     bool
		daemonMode     bool
		noDaemon       bool
		readCharBudget int
		hookPath       string
		validateWait   time.Duration
	)
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run the MCP server on stdin/stdout",
		Args:  cobra.NoArgs,
	}
	f := cmd.Flags()
	f.StringVar(&rootPath, "root", ".", "workspace root directory the symbol index covers")
	f.BoolVar(&legacyTools, "legacy-tools", false, "expose the legacy 9-tool MCP surface instead of the 3-tool surface")
	f.BoolVar(&readOnly, "read-only", false, "hide every mutating tool (node_edit/node_delete/node_refactor/node_rename_file); navigation + reading only")
	// ON by default. A safety net that must be asked for is a safety net
	// nobody has: dun spawns this server with no flags, so every agent edit
	// ran unvalidated until now — and node_edit's `return` op will happily
	// leave a signature the body no longer satisfies.
	//
	// Safe to default because it degrades rather than blocks: with no child
	// LSP for the language it is off entirely, and when diagnostics are
	// unavailable or time out the edit APPLIES and is flagged (fail-open,
	// Skipped: no-lsp / lsp-timeout). It cannot make an unvalidatable
	// workspace uneditable.
	//
	// --no-validate is the escape hatch, for when validation itself is the
	// problem — the known one being a never-analyzed file with pre-existing
	// errors, which has no baseline and can false-revert its first edit.
	f.BoolVar(&validate, "validate", true, "revert-on-new-diagnostics: an edit that introduces a new error is rolled back instead of landing (needs a child LSP; degrades to apply-and-flag without one)")
	f.DurationVar(&validateWait, "validate-wait", 2*time.Second, "how long an edit waits for the child LSP's diagnostics of the edited text; past it the edit is kept and reported not validated (validationSkipped: lsp-timeout). A cold child (sourcekit-lsp loading a package) can need longer")
	f.BoolVar(&noValidate, "no-validate", false, "turn OFF revert-on-new-diagnostics (see --validate); for when validation itself misfires, e.g. a never-analyzed file with pre-existing errors")
	// Daemon mode is the default. --daemon stays accepted: client configs
	// written when it was opt-in pass it, and dun detects the proxy by the
	// two phrases in its help (and --hook's), so they stay as they are.
	f.BoolVar(&daemonMode, "daemon", false, "the default (see --no-daemon); accepted for existing configs. Proxy tool calls to the shared per-user poly-lsp daemon (auto-starting it) instead of building the index in-process; one warm index + child-LSP fleet is shared across every client; a root outside --allow is admitted")
	f.BoolVar(&noDaemon, "no-daemon", false, "build the index and run child LSPs in this process instead of proxying to the shared per-user daemon; daemon mode is the default. $"+noDaemonEnv+"=1 does the same. Implied (logged) by an in-process-only flag given without --daemon: "+inProcessOnlyList()+"; also used when the daemon cannot start")
	f.IntVar(&readCharBudget, "read-char-budget", 0, "implicit char cap for a node_read with no lineLimit (0 = default 2048). Raising it trades round-trips for payload size: at the default, reading a ~1100-line file takes many truncated reads, and each re-read costs a turn plus the tokens to compose it")
	f.StringVar(&hookPath, "hook", os.Getenv("POLY_LSP_HOOK"), "path to a host hook executable consulted before every file read/edit (exit 2 = cancel, stdout = injected note); $POLY_LSP_HOOK fallback; see mcp/hook.go. --daemon: the proxy runs it")
	lifecycle := lifecycleFlags(f)
	rootsFlag := lspRootsFlags(f)
	idx := indexFlags(f)
	daemonHome := daemonHomeFlag(f)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if _, err := daemonHome(); err != nil {
			return err
		}
		roots, err := rootsFlag()
		if err != nil {
			return err
		}
		if readCharBudget > 0 {
			mcp.SetReadCharBudget(readCharBudget)
		}
		if noValidate {
			validate = false
		}
		if validateWait <= 0 {
			return fmt.Errorf("--validate-wait %s: want a positive duration", validateWait)
		}
		root, err := filepath.Abs(rootPath)
		if err != nil {
			return fmt.Errorf("root: %w", err)
		}

		useDaemon, err := chooseDaemon(cmd, daemonMode, noDaemon)
		if err != nil {
			return err
		}

		// Daemon proxy mode: keep the stdio MCP surface, forward tool calls
		// to the shared daemon over its unix socket. The workspace index,
		// child LSPs, and parse cache live once in the daemon.
		if useDaemon {
			// The client's index choices travel with its /open: they shape
			// the root when this open builds it. Only flags given on this
			// command line; the rest are the daemon's own.
			opts := daemon.OpenOptions{Excludes: idx.excludes, Includes: idx.includes, IndexAnyway: idx.anyway}
			for _, d := range idx.excludes {
				if git.CleanExclude(d) == "" {
					return fmt.Errorf("--exclude %q: want a directory inside the root, relative to it", d)
				}
			}
			if cmd.Flags().Changed("index-budget") {
				opts.Budget = idx.budget
			}
			if cmd.Flags().Changed("symlink-depth") {
				if idx.symlinkDepth < 0 {
					return fmt.Errorf("--symlink-depth %d: want 0 or more", idx.symlinkDepth)
				}
				opts.SymlinkDepth = &idx.symlinkDepth
			}
			if cmd.Flags().Changed("lsp-roots") {
				opts.LSPRoots = "single"
				if roots.auto {
					opts.LSPRoots = "auto"
				}
			}
			// Only reached with an explicit --daemon: without it, these
			// flags chose in-process (chooseDaemon).
			for _, name := range inProcessOnly(cmd) {
				log.Printf("mcp: %s is ignored in --daemon mode (the daemon's own setting applies)", name)
			}
			// Reach the daemon (starting it if none answers) before serving:
			// a daemon that cannot start — no writable ~/.poly-lsp, a socket
			// that will not bind — falls back to in-process here, while that
			// is still possible. The cost is the start itself on the first
			// client: a daemon binds its socket before it loads anything.
			// Reconnects after a lost daemon still run behind the reply.
			first, err := daemon.EnsureRunning(nil)
			if err != nil {
				log.Printf("mcp: the shared daemon is unavailable (%v); serving in-process instead (--no-daemon skips the attempt)", err)
			} else {
				log.Printf("mcp: proxying root %s to shared daemon (readOnly=%v validate=%v)", root, readOnly, validate)
				var connectMu sync.Mutex
				connect := func() (*daemon.Client, error) {
					connectMu.Lock()
					client := first
					first = nil
					connectMu.Unlock()
					if client == nil {
						c, err := daemon.EnsureRunning(nil)
						if err != nil {
							return nil, err
						}
						client = c
					}
					// Carry this client's policy to the daemon (enforced
					// per-connection at its boundary).
					client.SetPolicy(readOnly, validate)
					client.SetValidateWait(validateWait)
					return client, nil
				}
				// The file-access hook runs here, client-side, with this
				// process's env: the daemon runs no client's hook.
				var hook *mcp.FileHook
				if hookPath != "" {
					hook = &mcp.FileHook{Path: hookPath}
					log.Printf("mcp: file-access hook armed (proxy): %s", hookPath)
				}
				return daemon.RunProxy(connect, root, opts, hook, os.Stdin, os.Stdout)
			}
		}

		cfg, reg, err := l.load(true)
		if err != nil {
			return err
		}
		log.Printf("mcp: workspace root %s", root)

		if cfg.AutoSchemas {
			detected := config.DetectSchemas(root, cfg.Schemas)
			if len(detected) > 0 {
				log.Printf("auto-schemas: detected %d schema file(s):", len(detected))
				for _, s := range detected {
					log.Printf("  - %s (%s)", s.File, s.Dialect)
				}
				cfg.Schemas = append(cfg.Schemas, detected...)
			}
		}

		srv := mcp.New(reg, root, cfg.Bindings, cfg.Schemas)
		if err := idx.apply(srv, cfg); err != nil {
			return err
		}
		if len(idx.excludes) > 0 {
			log.Printf("mcp: excluded from the index: %v", []string(idx.excludes))
		}
		// initialize replies at once; the index builds behind it and tool
		// calls wait for it. See mcp/async_init.go.
		srv.SetAsyncInit(true)
		srv.SetUsageLog(daemon.UsagePath())
		srv.SetLegacyTools(legacyTools)
		srv.SetReadOnly(readOnly)
		if readOnly {
			log.Printf("mcp: READ-ONLY — mutating tools are not registered")
		}
		srv.SetValidateEdits(validate)
		srv.SetDiagnosticWait(validateWait)
		if hookPath != "" {
			srv.SetFileAccessHook(&mcp.FileHook{Path: hookPath})
			log.Printf("mcp: file-access hook armed: %s", hookPath)
		}
		// Log the SURPRISING state. Validation is the default now, so announcing
		// it every run is noise; running WITHOUT it is the fact worth recording,
		// because an edit that breaks the build will then simply land.
		if !validate {
			log.Printf("mcp: --no-validate — edits that introduce a new error will LAND, not revert")
		}
		srv.SetCachePath(cachePathFor(root))
		// Spawn child LSPs so node_edit / node_delete / node_refactor can
		// surface publishDiagnostics in their responses. Manager.Start runs
		// inside handleInitialize once we know which languages the
		// workspace actually contains, so we just hand the pre-built
		// Manager over here.
		mgr := multiplex.NewManager(reg)
		mgr.Lifecycle = lifecycle()
		mgr.StatusDir = daemon.StatusDir()
		srv.SetManager(mgr)
		srv.SetLSPRoots(roots.auto, roots.max)
		return srv.Serve(os.Stdin, os.Stdout)
	}
	return cmd
}

// noDaemonEnv is --no-daemon's environment fallback, for a client that can
// only set env (as $POLY_LSP_HOOK, $POLY_LSP_READ_CHAR_BUDGET).
const noDaemonEnv = "POLY_LSP_NO_DAEMON"

// inProcessOnlyFlags are the mcp flags the daemon does not take from a
// client: they configure this process's own server (--legacy-tools,
// --read-char-budget, --config) or its child-LSP fleet, which in daemon
// mode is the daemon's, set by its own flags. Given without --daemon, any
// of them means in-process.
var inProcessOnlyFlags = []string{"legacy-tools", "read-char-budget", "config", "lsp-idle-stop", "lsp-max-rss-mb", "lsp-psi-threshold", "lsp-roots-max"}

func inProcessOnlyList() string {
	names := make([]string, len(inProcessOnlyFlags))
	for i, name := range inProcessOnlyFlags {
		names[i] = "--" + name
	}
	return strings.Join(names, ", ")
}

// inProcessOnly names the in-process-only settings this invocation gives:
// the flags set on its command line, $POLY_LSP_READ_CHAR_BUDGET, and a
// config file at the default --config path (the daemon reads the config
// of the directory it was started in, not this client's).
func inProcessOnly(cmd *cobra.Command) []string {
	var given []string
	for _, name := range inProcessOnlyFlags {
		if f := cmd.Flag(name); f != nil && f.Changed {
			given = append(given, "--"+name)
		}
	}
	if !cmd.Flag("read-char-budget").Changed && os.Getenv("POLY_LSP_READ_CHAR_BUDGET") != "" {
		given = append(given, "$POLY_LSP_READ_CHAR_BUDGET")
	}
	if f := cmd.Flag("config"); f != nil && !f.Changed {
		if _, err := os.Stat(f.Value.String()); err == nil {
			given = append(given, "config file "+f.Value.String())
		}
	}
	return given
}

// chooseDaemon decides daemon mode (the default) or in-process. In order:
// --no-daemon / $POLY_LSP_NO_DAEMON=1 or --daemon=false: in-process; an
// explicit --daemon: daemon, whatever else is given (the in-process-only
// settings are then ignored, logged); an in-process-only setting: in-process,
// logged; else daemon.
func chooseDaemon(cmd *cobra.Command, daemonMode, noDaemon bool) (bool, error) {
	explicitDaemon := cmd.Flags().Changed("daemon")
	if cmd.Flags().Changed("no-daemon") {
		if explicitDaemon && daemonMode && noDaemon {
			return false, fmt.Errorf("--daemon and --no-daemon: give one")
		}
		if noDaemon {
			return false, nil
		}
	} else if os.Getenv(noDaemonEnv) == "1" && !(explicitDaemon && daemonMode) {
		log.Printf("mcp: $%s=1: serving in-process", noDaemonEnv)
		return false, nil
	}
	if explicitDaemon {
		return daemonMode, nil
	}
	if given := inProcessOnly(cmd); len(given) > 0 {
		log.Printf("mcp: %s given: serving in-process (implies --no-daemon; the daemon would not apply it)", strings.Join(given, ", "))
		return false, nil
	}
	return true, nil
}

// lifecycleFlags registers the child-LSP lifecycle flags shared by `mcp`
// and `daemon`; call the result after parsing. See multiplex.Lifecycle.
func lifecycleFlags(f *pflag.FlagSet) func() multiplex.Lifecycle {
	idle := f.Duration("lsp-idle-stop", 15*time.Minute, "stop a child LSP (gopls etc.) that got no request for this long; it starts again on the next request. 0 = never")
	rssMB := f.Int64("lsp-max-rss-mb", 6144, "stop a child LSP whose RSS is above this many MiB; it starts again on the next request. 0 = no limit")
	psi := f.Float64("lsp-psi-threshold", 20, "when /proc/pressure/memory 'some avg10' is at or above this, stop one child LSP per 10s (idle longest, then largest). Linux only. 0 = off")
	return func() multiplex.Lifecycle {
		return multiplex.Lifecycle{IdleStop: *idle, MaxRSS: *rssMB << 20, PSIThreshold: *psi}
	}
}

// lspRoots is the --lsp-roots choice; see mcp/lsp_roots.go.
type lspRoots struct {
	auto bool
	max  int
}

// lspRootsFlags registers --lsp-roots and --lsp-roots-max, shared by `mcp`
// and `daemon`; call the result after parsing.
func lspRootsFlags(f *pflag.FlagSet) func() (lspRoots, error) {
	mode := f.String("lsp-roots", "single", "child LSPs per language: single = one at the workspace root; auto = one per project root (go.mod, tsconfig.json, pyproject.toml/setup.py/setup.cfg, compile_commands.json/CMakeLists.txt), each started on first use. For monorepos where one pylsp/clangd resolves wrong or grows large")
	max := f.Int("lsp-roots-max", 8, "with --lsp-roots auto: at most this many children running per language; the least recently used is stopped to start another")
	return func() (lspRoots, error) {
		switch *mode {
		case "single":
			return lspRoots{max: *max}, nil
		case "auto":
			return lspRoots{auto: true, max: *max}, nil
		}
		return lspRoots{}, fmt.Errorf("--lsp-roots %q: want single or auto", *mode)
	}
}

// statusCmd prints the child LSPs of every running poly-lsp-mcp process
// (stdio mcp servers and daemon roots), from their status files.
func statusCmd() *cobra.Command {
	var asJSON bool
	var daemonHome func() (string, error)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "List the child LSPs and index cost of every running poly-lsp-mcp process",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := daemonHome(); err != nil {
				return err
			}
			dir := daemon.StatusDir()
			files, err := multiplex.ReadStatusFiles(dir)
			if err != nil {
				return err
			}
			sort.Slice(files, func(i, j int) bool { return files[i].PID < files[j].PID })
			if asJSON {
				b, _ := json.MarshalIndent(files, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if len(files) == 0 {
				fmt.Fprintf(os.Stderr, "no running poly-lsp-mcp process has written status (looked in %s)\n", dir)
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "PID\tROOT\tLANG\tLSP ROOT\tSTATE\tCHILD\tRSS\tIDLE\tSTARTS\tCRASH\tIDLE-STOP\tMEM-STOP\tPSI-STOP\tLAST STOP")
			for _, f := range files {
				if len(f.Children) == 0 {
					fmt.Fprintf(w, "%d\t%s\t-\t-\tno child LSPs\t\t\t\t\t\t\t\t\t\n", f.PID, f.Root)
				}
				for _, c := range f.Children {
					last := "-"
					if c.LastStop != "" {
						last = fmt.Sprintf("%s %s (%dM)", c.LastStop, c.LastStopAt, c.LastStopRSS>>20)
					}
					child, rss := "-", "-"
					if c.PID != 0 {
						child, rss = strconv.Itoa(c.PID), fmt.Sprintf("%dM", c.RSS>>20)
					}
					fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\n",
						f.PID, f.Root, c.Language, lspRoot(f.Root, c.Root), c.State, child, rss, time.Duration(c.IdleSec)*time.Second,
						c.Starts, c.CrashRestarts, c.IdleStops, c.MemoryStops, c.PressureStops, last)
				}
			}
			if err := w.Flush(); err != nil {
				return err
			}
			for _, f := range files {
				var rep symbols.CostReport
				if len(f.Index) == 0 || json.Unmarshal(f.Index, &rep) != nil || rep.Files == 0 {
					continue
				}
				fmt.Printf("\n%d %s\n%s", f.PID, f.Root, rep.Text())
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the status files as one JSON array")
	daemonHome = daemonHomeFlag(cmd.Flags())
	return cmd
}

// lspRoot renders a child's root relative to its workspace root: "." for
// a child at the workspace root, "-" for a status file from before roots.
func lspRoot(workspace, root string) string {
	if root == "" {
		return "-"
	}
	return symbols.RelRoot(workspace, root)
}
