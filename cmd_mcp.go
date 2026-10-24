package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/daemon"
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
		readCharBudget int
		hookPath       string
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
	f.BoolVar(&noValidate, "no-validate", false, "turn OFF revert-on-new-diagnostics (see --validate); for when validation itself misfires, e.g. a never-analyzed file with pre-existing errors")
	f.BoolVar(&daemonMode, "daemon", false, "proxy tool calls to the shared per-user poly-lsp daemon (auto-starting it) instead of building the index in-process; one warm index + child-LSP fleet is shared across every client")
	f.IntVar(&readCharBudget, "read-char-budget", 0, "implicit char cap for a node_read with no lineLimit (0 = default 2048). Raising it trades round-trips for payload size: at the default, reading a ~1100-line file takes many truncated reads, and each re-read costs a turn plus the tokens to compose it")
	f.StringVar(&hookPath, "hook", os.Getenv("POLY_LSP_HOOK"), "path to a host hook executable consulted before every file read/edit (exit 2 = cancel, stdout = injected note); $POLY_LSP_HOOK fallback. See mcp/hook.go")
	lifecycle := lifecycleFlags(f)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if readCharBudget > 0 {
			mcp.SetReadCharBudget(readCharBudget)
		}
		if noValidate {
			validate = false
		}
		root, err := filepath.Abs(rootPath)
		if err != nil {
			return fmt.Errorf("root: %w", err)
		}

		// Daemon proxy mode: keep the stdio MCP surface, forward tool calls
		// to the shared daemon over its unix socket. The workspace index,
		// child LSPs, and parse cache live once in the daemon.
		if daemonMode {
			client, err := daemon.EnsureRunning(nil)
			if err != nil {
				return fmt.Errorf("mcp: daemon: %w", err)
			}
			// Carry this client's policy to the daemon (enforced per-connection at
			// its boundary). --legacy-tools has no daemon surface — the daemon
			// serves the modern surface only.
			client.SetPolicy(readOnly, validate)
			if legacyTools {
				log.Print("mcp: --legacy-tools is ignored in --daemon mode (the daemon serves the modern surface)")
			}
			log.Printf("mcp: proxying root %s to shared daemon (readOnly=%v validate=%v)", root, readOnly, validate)
			return daemon.RunProxy(client, root, os.Stdin, os.Stdout)
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
		srv.SetLegacyTools(legacyTools)
		srv.SetReadOnly(readOnly)
		if readOnly {
			log.Printf("mcp: READ-ONLY — mutating tools are not registered")
		}
		srv.SetValidateEdits(validate)
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
		return srv.Serve(os.Stdin, os.Stdout)
	}
	return cmd
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

// statusCmd prints the child LSPs of every running poly-lsp-mcp process
// (stdio mcp servers and daemon roots), from their status files.
func statusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "List the child LSPs and index cost of every running poly-lsp-mcp process",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			fmt.Fprintln(w, "PID\tROOT\tLANG\tSTATE\tCHILD\tRSS\tIDLE\tSTARTS\tCRASH\tIDLE-STOP\tMEM-STOP\tPSI-STOP\tLAST STOP")
			for _, f := range files {
				if len(f.Children) == 0 {
					fmt.Fprintf(w, "%d\t%s\t-\tno child LSPs\t\t\t\t\t\t\t\t\t\n", f.PID, f.Root)
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
					fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\n",
						f.PID, f.Root, c.Language, c.State, child, rss, time.Duration(c.IdleSec)*time.Second,
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
	return cmd
}
