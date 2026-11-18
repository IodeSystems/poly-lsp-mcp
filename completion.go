package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/daemon"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// Shell completion is cobra's: the script a shell sources asks the binary
// (`poly-lsp-mcp __complete ...`) for the words, so subcommands and flags
// cannot drift from the command tree. This file adds what cobra cannot know:
// the roots the daemon hosts, the configured servers, the language names.
//
// A completion request must be cheap and must change nothing: it reads files
// and nothing else. It never starts the daemon or a language server, it does
// not create the daemon home, and it does not call multiplex.ReadStatusFiles,
// which deletes the status files of dead processes. main() also skips the
// self-rebuild for it.

// completionCmd is `completion [bash|zsh|fish|powershell]`; with no shell
// it takes it from $SHELL.
func completionCmd() *cobra.Command {
	shells := []string{"bash", "zsh", "fish", "powershell"}
	return &cobra.Command{
		Use:   "completion [" + strings.Join(shells, "|") + "]",
		Short: "Print the shell completion script (shell from $SHELL when not named)",
		Long: `Print a shell completion script. Load it in the current shell with:

  bash: source <(poly-lsp-mcp completion bash)
  zsh:  source <(poly-lsp-mcp completion zsh)
  fish: poly-lsp-mcp completion fish | source

Put the line in ~/.bashrc, ~/.zshrc or ~/.config/fish/config.fish to load it
in every shell. With no shell named, the one in $SHELL is used.

The script asks poly-lsp-mcp for the words as you type. --root and the other
root flags complete to the roots the daemon hosts (read from its status
files), lsp remove to the servers in servers.yaml, --lsp-off and its kin to
the language names. A completion reads files only: it never starts the daemon
or a language server.`,
		GroupID:               groupSetup,
		Args:                  cobra.MaximumNArgs(1),
		ValidArgs:             shells,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			shell := filepath.Base(os.Getenv("SHELL"))
			if len(args) == 1 {
				shell = args[0]
			}
			root, out := cmd.Root(), cmd.OutOrStdout()
			switch shell {
			case "bash":
				return root.GenBashCompletionV2(out, true)
			case "zsh":
				return root.GenZshCompletion(out)
			case "fish":
				return root.GenFishCompletion(out, true)
			case "powershell", "pwsh":
				return root.GenPowerShellCompletionWithDesc(out)
			}
			if len(args) == 0 {
				return fmt.Errorf("cannot tell the shell from $SHELL (%q); name it: %s completion bash|zsh|fish", os.Getenv("SHELL"), root.Name())
			}
			return fmt.Errorf("%q is not a shell completed here; use bash, zsh, fish or powershell", shell)
		},
	}
}

// firstClause is s up to its first ':', ';', '(' or sentence end, cut to 60
// runes: short enough for a completion menu. Runes, not bytes: a cut in the
// middle of a UTF-8 sequence prints as garbage in the menu.
func firstClause(s string) string {
	if i := strings.IndexAny(s, ":;("); i > 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > 60 {
		s = string([]rune(s)[:57]) + "..."
	}
	return s
}

// shortenForCompletion cuts every command's and flag's description to a menu
// line. Only in a completion request: --help wants the full text.
func shortenForCompletion(root *cobra.Command) {
	seen := map[*pflag.Flag]bool{}
	cut := func(f *pflag.Flag) {
		if !seen[f] {
			seen[f] = true
			_, usage := pflag.UnquoteUsage(f)
			f.Usage = firstClause(usage)
		}
	}
	for _, c := range allCommands(root) {
		c.Short = firstClause(c.Short)
		c.Flags().VisitAll(cut)
		c.PersistentFlags().VisitAll(cut)
	}
}

// installCompletion adds the completion command, the menu-length descriptions
// for a completion request, and the dynamic completions.
func installCompletion(root *cobra.Command) {
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(completionCmd())
	root.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
		if cmd.Name() == cobra.ShellCompRequestCmd {
			shortenForCompletion(root)
		}
	}

	for _, c := range allCommands(root) {
		for _, name := range []string{"root", "allow", "warm", "allowed"} {
			register(c, name, completeRoots)
		}
		register(c, "lsp-roots", func(*cobra.Command, string) ([]string, cobra.ShellCompDirective) {
			return []string{"single\tone child per language, at the workspace root", "auto\tone child per project root"}, cobra.ShellCompDirectiveNoFileComp
		})
		for _, name := range []string{"lsp-off", "lsp-on", "format-on-edit", "no-format-on-edit"} {
			register(c, name, completeLanguageList)
		}
		register(c, "daemon-home", func(*cobra.Command, string) ([]string, cobra.ShellCompDirective) {
			return nil, cobra.ShellCompDirectiveFilterDirs
		})
	}
	if c, _, err := root.Find([]string{"lsp", "remove"}); err == nil && c.Name() == "remove" {
		c.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return withPrefix(userServerNames(cmd), toComplete), cobra.ShellCompDirectiveNoFileComp
		}
	}
	if c, _, err := root.Find([]string{"lsp", "add"}); err == nil && c.Name() == "add" {
		c.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return withPrefix(languageNames(cmd), toComplete), cobra.ShellCompDirectiveNoFileComp
		}
	}
}

// register completes c's own flag name with fn, when c has a flag of that
// name that takes a value.
func register(c *cobra.Command, name string, fn func(*cobra.Command, string) ([]string, cobra.ShellCompDirective)) {
	f := c.Flags().Lookup(name)
	if f == nil || f.Value.Type() == "bool" {
		return
	}
	_ = c.RegisterFlagCompletionFunc(name, func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return fn(cmd, toComplete)
	})
}

// withPrefix is the names that start with prefix.
func withPrefix(names []string, prefix string) []string {
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	return out
}

// homeFlag is the --daemon-home given on the command line so far, absolute,
// or "" (then $POLY_LSP_HOME, else the default home, by the daemon package).
// It does not call daemon.SetHome, which creates the directory.
func homeFlag(cmd *cobra.Command) string {
	h, err := cmd.Flags().GetString("daemon-home")
	if err != nil || h == "" {
		return ""
	}
	if abs, err := filepath.Abs(h); err == nil {
		return abs
	}
	return h
}

// knownRoots are the roots in the status files of the daemon home: those the
// daemon hosts and those of stdio servers, sorted.
func knownRoots(cmd *cobra.Command) []string {
	dir := daemon.StatusDir()
	if h := homeFlag(cmd); h != "" {
		dir = filepath.Join(h, "children")
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	seen := map[string]bool{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var sf multiplex.StatusFile
		if json.Unmarshal(b, &sf) == nil && sf.Root != "" {
			seen[sf.Root] = true
		}
	}
	roots := make([]string, 0, len(seen))
	for r := range seen {
		roots = append(roots, r)
	}
	sort.Strings(roots)
	return roots
}

// completeRoots: the known roots; with none, the shell completes paths.
func completeRoots(cmd *cobra.Command, toComplete string) ([]string, cobra.ShellCompDirective) {
	return withPrefix(knownRoots(cmd), toComplete), cobra.ShellCompDirectiveDefault
}

// userServers are the servers.yaml entries, nil when there are none or the
// file is unreadable.
func userServers(cmd *cobra.Command) []config.UserServer {
	path := daemon.ServersPath()
	if h := homeFlag(cmd); h != "" {
		path = filepath.Join(h, config.ServersFile)
	}
	s, err := config.LoadServers(path)
	if err != nil {
		return nil
	}
	return s.Servers
}

func userServerNames(cmd *cobra.Command) []string {
	var names []string
	for _, u := range userServers(cmd) {
		names = append(names, u.Name)
	}
	sort.Strings(names)
	return names
}

// languageNames are the built-in languages and the servers.yaml ones.
func languageNames(cmd *cobra.Command) []string {
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, l := range config.Default().Languages {
		add(l.Name)
	}
	for _, n := range userServerNames(cmd) {
		add(n)
	}
	sort.Strings(names)
	return names
}

// completeLanguageList completes a comma list of languages (--lsp-off go,py):
// the word is replaced whole, so each candidate keeps what precedes the last
// comma and leaves out the languages already named.
func completeLanguageList(cmd *cobra.Command, toComplete string) ([]string, cobra.ShellCompDirective) {
	head, tail := "", toComplete
	if i := strings.LastIndex(toComplete, ","); i >= 0 {
		head, tail = toComplete[:i+1], toComplete[i+1:]
	}
	named := map[string]bool{}
	for _, n := range strings.Split(head, ",") {
		named[n] = true
	}
	var out []string
	for _, n := range languageNames(cmd) {
		if !named[n] && strings.HasPrefix(n, tail) {
			out = append(out, head+n)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
}
