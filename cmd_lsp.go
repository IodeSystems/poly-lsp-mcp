package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/daemon"
	"github.com/iodesystems/poly-lsp-mcp/internal/lspbin"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
	"github.com/iodesystems/poly-lsp-mcp/setup"
)

// The user's server registry: `lsp list`, `lsp add`, `lsp remove`. See
// config/servers.go for the file and its merge, mcp/lsp_off.go for the
// per-client switch.

// lspEntry is one language of `lsp list --json`.
type lspEntry struct {
	Name       string   `json:"name"`
	Extensions []string `json:"extensions"`
	// Source: builtin, user (servers.yaml; Overrides when it overrides a
	// built-in of that name), or project (a config file's languages:
	// replace the registry).
	Source       string         `json:"source"`
	Overrides    bool           `json:"overridesBuiltin,omitempty"`
	TreeSitter   string         `json:"treesitter,omitempty"`
	LSP          *lspListLSP    `json:"lsp,omitempty"` // nil: tree-sitter only
	Install      *setup.Install `json:"install,omitempty"`
	OffByDefault bool           `json:"offByDefault"`
	FormatOnEdit bool           `json:"formatOnEdit"`
	RootMarkers  []string       `json:"rootMarkers,omitempty"`
	// Child is the state of this language's child LSP for --root, from
	// the status files of running poly-lsp processes (running, stopped,
	// down); "" when none has one.
	Child string `json:"child,omitempty"`
}

type lspListLSP struct {
	setup.LSP
	Args []string `json:"args,omitempty"`
	Env  []string `json:"env,omitempty"`
}

type lspListReport struct {
	Servers string     `json:"servers"` // the servers.yaml path
	Config  string     `json:"config,omitempty"`
	Root    string     `json:"root,omitempty"`
	Langs   []lspEntry `json:"languages"`
}

// lspRegistryCmds are `lsp list`, `lsp add` and `lsp remove`.
func lspRegistryCmds(l *loader) []*cobra.Command {
	return []*cobra.Command{lspListCmd(l), lspAddCmd(), lspRemoveCmd()}
}

func lspListCmd(l *loader) *cobra.Command {
	var (
		asJSON bool
		root   string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the effective language registry: built-ins, user servers (servers.yaml), their child LSPs",
		Long: `List every language poly-lsp-mcp knows: its extensions, where it comes from
(builtin, user: servers.yaml in the daemon home, or project: a config file's
languages:), its child LSP command and whether it is found on this machine,
an install hint for a known missing one, whether it is off by default, and
whether its kept edits are formatted by the child LSP (format_on_edit).

With --root, also the state of that root's child for each language, as
running poly-lsp processes (the daemon, in-process servers) report it.`,
		Args: cobra.NoArgs,
	}
	f := cmd.Flags()
	f.BoolVar(&asJSON, "json", false, "print one JSON document")
	f.StringVar(&root, "root", "", "also show the state of this workspace root's child LSPs")
	daemonHome := daemonHomeFlag(f)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if _, err := daemonHome(); err != nil {
			return err
		}
		cfg, reg, err := l.load(false)
		if err != nil {
			return err
		}
		sp := daemon.ServersPath()
		user, err := config.LoadServers(sp)
		if err != nil {
			return err
		}
		builtin := map[string]bool{}
		for _, b := range config.Default().Languages {
			builtin[b.Name] = true
		}
		rep := lspListReport{Servers: sp}
		if _, err := os.Stat(l.configPath); err == nil {
			rep.Config = l.configPath
		}
		var states map[string]string
		if root != "" {
			abs, err := filepath.Abs(root)
			if err != nil {
				return err
			}
			rep.Root = abs
			states = childStates(abs)
		}
		for _, lang := range reg.Languages() {
			e := lspEntry{Name: lang.Name, Extensions: lang.Extensions, Source: "builtin", TreeSitter: lang.TreeSitter,
				OffByDefault: lang.OffByDefault, FormatOnEdit: lang.FormatOnEdit, RootMarkers: lang.RootMarkers, Child: states[lang.Name]}
			switch {
			case cfg.ProjectLanguages:
				e.Source = "project"
			case user.Find(lang.Name) != nil:
				e.Source, e.Overrides = "user", builtin[lang.Name]
			}
			if lang.LSP != nil {
				found, install := setup.Resolve(lang.LSP.Cmd)
				e.LSP = &lspListLSP{LSP: *found, Args: lang.LSP.Args, Env: lang.LSP.Env}
				e.Install = install
			}
			rep.Langs = append(rep.Langs, e)
		}
		if asJSON {
			b, _ := json.MarshalIndent(rep, "", "  ")
			fmt.Println(string(b))
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		head := "NAME\tEXTENSIONS\tSOURCE\tLSP\tFOUND\tOFF BY DEFAULT\tFORMAT ON EDIT"
		if root != "" {
			head += "\tCHILD"
		}
		fmt.Fprintln(w, head)
		for _, e := range rep.Langs {
			src := e.Source
			if e.Overrides {
				src += " (overrides builtin)"
			}
			lsp, found := "-", "-"
			if e.LSP != nil {
				lsp = strings.TrimSpace(e.LSP.Name + " " + strings.Join(e.LSP.Args, " "))
				found = "no"
				if e.LSP.Found {
					found = e.LSP.Path
					if e.LSP.Via != "" {
						found += " (" + e.LSP.Via + ")"
					}
				}
			}
			off := "no"
			if e.OffByDefault {
				off = "yes"
			}
			format := "no"
			if e.FormatOnEdit {
				format = "yes"
			}
			row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s", e.Name, "."+strings.Join(e.Extensions, " ."), src, lsp, found, off, format)
			if root != "" {
				child := e.Child
				if child == "" {
					child = "-"
				}
				row += "\t" + child
			}
			fmt.Fprintln(w, row)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		hinted := map[string]bool{}
		for _, e := range rep.Langs {
			if e.LSP != nil && !e.LSP.Found && e.Install != nil && !hinted[e.LSP.Name] {
				hinted[e.LSP.Name] = true
				fmt.Printf("install %s (needs %s): %s\n", e.LSP.Name, strings.Join(e.Install.Needs, ", "), e.Install.Cmd)
			}
		}
		fmt.Printf("user servers: %s\n", sp)
		if cfg.ProjectLanguages {
			fmt.Printf("%s declares languages:, which replace the registry; servers.yaml is not merged (and %s implies in-process)\n", l.configPath, l.configPath)
		}
		return nil
	}
	return cmd
}

// childStates maps each language to the state of root's child LSP, from
// the status files of running poly-lsp processes: running wins over
// stopped over down.
func childStates(root string) map[string]string {
	out := map[string]string{}
	files, err := multiplex.ReadStatusFiles(daemon.StatusDir())
	if err != nil {
		return out
	}
	rank := map[string]int{"running": 3, "stopped": 2, "down": 1}
	for _, f := range files {
		if f.Root != root {
			continue
		}
		for _, c := range f.Children {
			if rank[c.State] > rank[out[c.Language]] {
				out[c.Language] = c.State
			}
		}
	}
	return out
}

func lspAddCmd() *cobra.Command {
	var (
		exts, args, env, markers []string
		cmdName                  string
		offByDefault, trust      bool
		formatOnEdit             bool
	)
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a language's child LSP to servers.yaml, or override a built-in language by name",
		Long: `Add a user server to <daemon home>/servers.yaml (default ~/.poly-lsp; see
--daemon-home, $POLY_LSP_HOME). The file is user-level: the shared daemon runs
the commands it names, and a repository's poly-lsp-mcp.yaml never adds one to
the daemon (it implies in-process).

Entries merge over the built-in languages by NAME. A new name adds a
language served by its child LSP only: with no tree-sitter grammar, node_query
does not cover its files. An existing name overrides only the fields given
here and keeps the rest; ` + "`lsp remove <name>`" + ` restores the built-in. An entry
of the same name replaces the earlier one.

To turn a built-in's child off for every client by default, override it
with --off-by-default (` + "`lsp add go --off-by-default`" + `); a client turns it back
on with mcp --lsp-on go.

--format-on-edit has an edit to the language's files that is kept formatted
by its child LSP (range formatting over the changed lines when the server
declares it, else the whole file); --format-on-edit=false, or lsp remove,
turns it back off. A client overrides it with mcp --format-on-edit /
--no-format-on-edit. Like every field, it goes in this entry: an entry of
the same name replaces the earlier one, so give the other flags again.

The running daemon re-reads the file on each client open and rebuilds the
opened root (index and child LSPs) when it changed. --root-marker is stored
but not yet used: --lsp-roots auto reads the built-in markers only.`,
		Example: `  poly-lsp-mcp lsp add zig --ext .zig,.zon --cmd zls
  poly-lsp-mcp lsp add java --cmd jdtls --off-by-default
  poly-lsp-mcp lsp add go --off-by-default
  poly-lsp-mcp lsp add go --format-on-edit
  poly-lsp-mcp lsp add go --format-on-edit=false
  poly-lsp-mcp lsp add python --cmd pyright-langserver --arg --stdio`,
		Args: cobra.ExactArgs(1),
	}
	f := cmd.Flags()
	f.StringSliceVar(&exts, "ext", nil, "file extension the language claims (.foo or foo; comma list or repeated). Required for a new language; on a built-in it replaces the built-in's")
	f.StringVar(&cmdName, "cmd", "", "child LSP command (name on PATH or a path). Required for a new language; on a built-in it replaces the command and its arguments")
	f.StringArrayVar(&args, "arg", nil, "argument to the command (repeatable, in order)")
	f.StringArrayVar(&env, "env", nil, "KEY=VALUE added to the command's environment (repeatable)")
	f.StringArrayVar(&markers, "root-marker", nil, "file name that marks a project root for this language (repeatable). Stored, not yet used")
	f.BoolVar(&offByDefault, "off-by-default", false, "no client gets this child LSP unless it turns it on (mcp --lsp-on <name>)")
	f.BoolVar(&formatOnEdit, "format-on-edit", false, "format a kept edit to this language's files through its child LSP (format_on_edit); =false writes an explicit off")
	f.BoolVar(&trust, "trust-clean-diagnostics", false, "a clean diagnostics result from this server may validate an edit (see trust_clean_diagnostics)")
	daemonHome := daemonHomeFlag(f)
	cmd.RunE = func(c *cobra.Command, pos []string) error {
		if _, err := daemonHome(); err != nil {
			return err
		}
		u := config.UserServer{Name: pos[0], Extensions: exts, Cmd: cmdName, Args: args, Env: env, RootMarkers: markers, OffByDefault: offByDefault}
		if c.Flags().Changed("trust-clean-diagnostics") {
			u.TrustCleanDiagnostics = &trust
		}
		if c.Flags().Changed("format-on-edit") {
			u.FormatOnEdit = &formatOnEdit
		}
		sp := daemon.ServersPath()
		s, err := config.LoadServers(sp)
		if err != nil {
			return err
		}
		verb := "added"
		if s.Find(u.Name) != nil {
			verb = "replaced"
		}
		if err := s.Add(config.Default().Languages, u); err != nil {
			return err
		}
		if err := s.Save(sp); err != nil {
			return err
		}
		fmt.Printf("%s %s in %s\n", verb, u.Name, sp)
		if u.Cmd != "" {
			if _, err := lspbin.Resolve(u.Cmd); err != nil {
				fmt.Fprintf(os.Stderr, "warning: %s is not found on this machine (%v); the language works from tree-sitter alone, if it has a grammar, until it is\n", u.Cmd, err)
			}
		}
		fmt.Println("the daemon picks it up on each root's next open; an in-process server on its next start")
		return nil
	}
	return cmd
}

func lspRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a user server from servers.yaml (a built-in it overrode is restored)",
		Args:  cobra.ExactArgs(1),
	}
	daemonHome := daemonHomeFlag(cmd.Flags())
	cmd.RunE = func(_ *cobra.Command, pos []string) error {
		if _, err := daemonHome(); err != nil {
			return err
		}
		name := pos[0]
		sp := daemon.ServersPath()
		s, err := config.LoadServers(sp)
		if err != nil {
			return err
		}
		if !s.Remove(name) {
			return fmt.Errorf("%s: no user server named %q (built-ins are not removed; override one with lsp add %s --off-by-default)", sp, name, name)
		}
		if err := s.Save(sp); err != nil {
			return err
		}
		msg := fmt.Sprintf("removed %s from %s", name, sp)
		for _, b := range config.Default().Languages {
			if b.Name == name {
				msg += "; the built-in " + name + " is restored"
			}
		}
		fmt.Println(msg)
		return nil
	}
	return cmd
}
