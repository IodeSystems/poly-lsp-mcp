package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// --help is a custom formatter (cobra's default prints one line per flag,
// 300 characters wide for the longest). Layout: what the command is, usage,
// `start:` (its Example), the subcommands by group, the flags by group, each
// flag as `--name type` with its text on the next lines, indented and
// wrapped. Rarely used flags (advancedFlags) are listed by --help-all only.
// No colour. It prints to stdout and exits 0: a request for help is not an
// error.

const (
	helpAllFlag   = "help-all"
	defaultWidth  = 80
	flagTextInset = 6
)

// flagGroup is a --help section; a flag belongs to the group that names it.
type flagGroup struct {
	title string
	names []string
}

// flagGroups orders the flag sections. A flag in none of them is listed under
// "options"; TestHelpEveryFlagGrouped keeps the table complete. --api renders
// the per-declaration API summary (kind, name, owner, head, doc, field type).
var flagGroups = []flagGroup{
	{"workspace", []string{"root", "config", "exclude", "include", "index-budget", "index-anyway", "symlink-depth", "max-files", "allow-path", "exclude-path", "rev"}},
	{"editing", []string{"read-only", "validate", "validate-wait", "format-on-edit", "legacy-tools", "read-char-budget", "hook"}},
	{"language servers", []string{"lsp-off", "lsp-on", "lsp-roots", "lsp-roots-max", "lsp-idle-stop", "lsp-max-rss-mb", "lsp-psi-threshold", "lsp-max-children", "lsp-rss-budget-pct", "lsp-mac-pressure-level", "proactive-open"}},
	{"server entry", []string{"ext", "cmd", "arg", "env", "root-marker", "off-by-default", "trust-clean-diagnostics"}},
	{"daemon", []string{"daemon", "daemon-home", "no-daemon", "allow", "socket", "stop", "restart", "idle-exit", "root-lsp-grace", "idle-roots-mb", "warm-recent", "index-concurrency", "memory-limit-mb"}},
	{"query and output", []string{"api", "rev", "resolve", "limit", "offset", "json", "verbose", "budget", "no-update-check"}},
}

// advancedFlags are listed by --help-all only: tuning knobs with working
// defaults (budgets, caps, timeouts, lifecycle thresholds), stored-but-unused
// flags, and detail switches. Not here: --hook, --daemon and --daemon-home,
// whose help text clients read to detect what this binary supports (noBreak).
var advancedFlags = setOf(
	"index-budget", "index-anyway", "symlink-depth", "max-files", "allow-path", "exclude-path",
	"validate-wait", "legacy-tools", "read-char-budget",
	"lsp-roots-max", "lsp-idle-stop", "lsp-max-rss-mb", "lsp-psi-threshold", "lsp-max-children",
	"lsp-rss-budget-pct", "lsp-mac-pressure-level", "proactive-open",
	"root-marker", "trust-clean-diagnostics",
	"socket", "idle-exit", "root-lsp-grace", "idle-roots-mb", "warm-recent", "index-concurrency", "memory-limit-mb",
	"verbose", "budget",
)

// noBreak are phrases a client finds in the help text to learn what this
// binary does (TestMCPHelpMarkers); wrapping must not split them.
var noBreak = []string{
	"--daemon: the proxy runs it",
	"a root outside --allow is admitted",
	"daemon mode is the default",
}

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// helpWidth is the terminal width when w is a terminal, else 80.
func helpWidth(w io.Writer) int {
	if f, ok := w.(*os.File); ok {
		if cols, ok := termSize(f); ok && cols > 0 {
			return cols
		}
	}
	return defaultWidth
}

// installHelp sets the help and usage output of the whole tree and makes
// --help-all (a persistent flag) print the full help instead of running the
// command.
func installHelp(root *cobra.Command) {
	root.PersistentFlags().Bool(helpAllFlag, false, "like --help, with the rarely used flags")
	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		w := cmd.OutOrStdout()
		all, _ := cmd.Flags().GetBool(helpAllFlag)
		writeHelp(w, cmd, helpWidth(w), all)
	})
	root.SetUsageFunc(func(cmd *cobra.Command) error {
		fmt.Fprintf(cmd.OutOrStderr(), "usage: %s\nrun '%s --help' for the flags\n", cmd.UseLine(), cmd.CommandPath())
		return nil
	})
	asked := func(c *cobra.Command) bool {
		all, _ := c.Flags().GetBool(helpAllFlag)
		return all
	}
	var wrap func(*cobra.Command)
	wrap = func(c *cobra.Command) {
		if args := c.Args; args != nil {
			c.Args = func(c *cobra.Command, a []string) error {
				if asked(c) {
					return nil
				}
				return args(c, a)
			}
		}
		if run := c.RunE; run != nil {
			c.RunE = func(c *cobra.Command, a []string) error {
				if asked(c) {
					return c.Help()
				}
				return run(c, a)
			}
		}
		for _, sub := range c.Commands() {
			wrap(sub)
		}
	}
	wrap(root)
}

// wrapText breaks s into lines of at most width runes, at spaces; a word
// longer than width is a line of its own. The noBreak phrases stay whole.
func wrapText(s string, width int) []string {
	const glue = "\x1f"
	for _, p := range noBreak {
		s = strings.ReplaceAll(s, p, strings.ReplaceAll(p, " ", glue))
	}
	var lines []string
	cur := ""
	for _, w := range strings.Fields(s) {
		w = strings.ReplaceAll(w, glue, " ")
		switch {
		case cur == "":
			cur = w
		case utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(w) <= width:
			cur += " " + w
		default:
			lines = append(lines, cur)
			cur = w
		}
		// A word wider than a line (a path list) is cut after a '/' that
		// fits, else at the width.
		for utf8.RuneCountInString(cur) > width {
			r := []rune(cur)
			cut := width
			if i := strings.LastIndex(string(r[:width]), "/"); i > 0 {
				cut = utf8.RuneCountInString(string(r[:width])[:i+1])
			}
			lines = append(lines, string(r[:cut]))
			cur = string(r[cut:])
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// writeIndented writes s wrapped to width, each line after indent spaces.
func writeIndented(w io.Writer, s string, indent, width int) {
	pad := strings.Repeat(" ", indent)
	for _, l := range wrapText(s, max(width-indent, 20)) {
		fmt.Fprintln(w, pad+l)
	}
}

// writeHanging writes s wrapped, its first line after first spaces and the
// rest after rest.
func writeHanging(w io.Writer, s string, first, rest, width int) {
	lines := wrapText(s, max(width-first, 20))
	if len(lines) > 1 && rest != first {
		head := lines[0]
		tail := strings.TrimSpace(strings.TrimPrefix(s, head))
		lines = append([]string{head}, wrapText(tail, max(width-rest, 20))...)
	}
	for i, l := range lines {
		pad := rest
		if i == 0 {
			pad = first
		}
		fmt.Fprintln(w, strings.Repeat(" ", pad)+l)
	}
}

// writeParagraphs writes text wrapped; a paragraph with indented lines is
// preformatted (a list, a command): each line is kept, wrapped alone.
func writeParagraphs(w io.Writer, text string, width int) {
	for i, para := range strings.Split(strings.TrimSpace(text), "\n\n") {
		if i > 0 {
			fmt.Fprintln(w)
		}
		if !strings.HasPrefix(para, " ") && !strings.Contains(para, "\n ") {
			writeIndented(w, para, 0, width)
			continue
		}
		for _, line := range strings.Split(para, "\n") {
			trimmed := strings.TrimLeft(line, " ")
			indent := len(line) - len(trimmed)
			writeHanging(w, trimmed, indent, indent+4, width)
		}
	}
}

// writeHelp is --help (all: --help-all) of cmd, wrapped to width.
func writeHelp(w io.Writer, cmd *cobra.Command, width int, all bool) {
	desc := cmd.Long
	if desc == "" {
		desc = cmd.Short
	}
	writeParagraphs(w, desc, width)
	fmt.Fprintln(w)
	writeHanging(w, "usage: "+cmd.UseLine(), 0, 7, width)
	if ex := strings.TrimRight(cmd.Example, "\n"); ex != "" {
		fmt.Fprintln(w, "\nstart:")
		for _, line := range strings.Split(ex, "\n") {
			writeHanging(w, strings.TrimSpace(line), 2, 6, width)
		}
	}
	writeSubcommands(w, cmd, width)
	hidden := writeFlags(w, cmd, width, all)
	if cmd.Parent() == nil {
		fmt.Fprint(w, "\nshell completion:\n")
		for _, l := range completionHowTo(cmd.Name()) {
			writeHanging(w, l, 2, 6, width)
		}
	}
	switch {
	case cmd.Parent() == nil && !all:
		fmt.Fprintf(w, "\nmore flags: %s <cmd> --help-all\n", cmd.Name())
	case hidden > 0:
		fmt.Fprintf(w, "\nmore flags: %s --help-all\n", cmd.CommandPath())
	}
}

// writeSubcommands lists cmd's subcommands by group, then the ungrouped.
func writeSubcommands(w io.Writer, cmd *cobra.Command, width int) {
	var subs []*cobra.Command
	col := 0
	for _, c := range cmd.Commands() {
		if c.IsAvailableCommand() {
			subs = append(subs, c)
			col = max(col, len(c.Name()))
		}
	}
	if len(subs) == 0 {
		return
	}
	col += 4 // "  " + the widest name + "  "
	list := func(title string, cs []*cobra.Command) {
		if len(cs) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s:\n", title)
		for _, c := range cs {
			if width < 60 { // no room for two columns
				fmt.Fprintln(w, "  "+c.Name())
				writeIndented(w, c.Short, flagTextInset, width)
				continue
			}
			lines := wrapText(c.Short, width-col)
			if len(lines) == 0 {
				lines = []string{""}
			}
			fmt.Fprintf(w, "  %-*s%s\n", col-2, c.Name(), lines[0])
			for _, l := range lines[1:] {
				fmt.Fprintln(w, strings.Repeat(" ", col)+l)
			}
		}
	}
	listed := map[string]bool{}
	for _, g := range cmd.Groups() {
		var cs []*cobra.Command
		for _, c := range subs {
			if c.GroupID == g.ID {
				cs = append(cs, c)
				listed[c.Name()] = true
			}
		}
		list(g.Title, cs)
	}
	var rest []*cobra.Command
	for _, c := range subs {
		if !listed[c.Name()] {
			rest = append(rest, c)
		}
	}
	title := "commands"
	if len(listed) > 0 {
		title = "other"
	}
	list(title, rest)
}

// writeFlags prints cmd's flags by group and returns how many advanced flags
// it left out.
func writeFlags(w io.Writer, cmd *cobra.Command, width int, all bool) (left int) {
	fs := pflag.NewFlagSet("all", pflag.ContinueOnError)
	fs.AddFlagSet(cmd.LocalFlags())
	fs.AddFlagSet(cmd.InheritedFlags())

	// A --no-x flag is printed inside the --x entry.
	negOf := map[string]*pflag.Flag{}
	skip := map[string]bool{"help": true, helpAllFlag: true}
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			skip[f.Name] = true
		}
		if base, ok := strings.CutPrefix(f.Name, "no-"); ok && fs.Lookup(base) != nil {
			negOf[base] = f
			skip[f.Name] = true
		}
	})

	grouped := map[string]bool{}
	for _, g := range flagGroups {
		for _, n := range g.names {
			grouped[n] = true
		}
	}
	groups := append([]flagGroup(nil), flagGroups...)
	var other []string
	fs.VisitAll(func(f *pflag.Flag) {
		if !grouped[f.Name] && !skip[f.Name] {
			other = append(other, f.Name)
		}
	})
	if len(other) > 0 {
		groups = append(groups, flagGroup{"options", other})
	}

	for _, g := range groups {
		var fl []*pflag.Flag
		for _, n := range g.names {
			f := fs.Lookup(n)
			if f == nil || skip[n] {
				continue
			}
			if advancedFlags[n] && !all {
				left++
				continue
			}
			fl = append(fl, f)
		}
		if len(fl) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s:\n", g.title)
		for _, f := range fl {
			writeFlag(w, f, negOf[f.Name], width)
		}
	}
	return left
}

// flagTypeName is the argument a flag takes, "" for a switch.
func flagTypeName(f *pflag.Flag) string {
	name, _ := pflag.UnquoteUsage(f)
	switch name {
	case "stringArray":
		return "string"
	case "float64":
		return "float"
	}
	return name
}

// flagDefault is " (default X)", or "" when X is the zero value, when the
// text already says the default, or when X comes from the environment.
func flagDefault(f *pflag.Flag, help string) string {
	switch f.DefValue {
	case "", "false", "0", "0s", "[]", "-1":
		return ""
	}
	low := strings.ToLower(help)
	if strings.Contains(low, "default") || strings.Contains(low, "fallback") || strings.Contains(help, f.DefValue+" =") {
		return ""
	}
	return " (default " + f.DefValue + ")"
}

// writeFlag is one entry: `--x[, --no-x] type`, then the text. neg is the
// --no-x partner; its text follows the main text.
func writeFlag(w io.Writer, f, neg *pflag.Flag, width int) {
	_, help := pflag.UnquoteUsage(f)
	head := "--" + f.Name
	if neg != nil {
		head += ", --" + neg.Name
	}
	if t := flagTypeName(f); t != "" {
		head += " " + t
	}
	writeHanging(w, head, 2, flagTextInset, width)
	writeIndented(w, help+flagDefault(f, help), flagTextInset, width)
	if neg != nil {
		_, nh := pflag.UnquoteUsage(neg)
		writeIndented(w, "--"+neg.Name+": "+nh, flagTextInset, width)
	}
}

// completionHowTo are the lines that load completions, per shell.
func completionHowTo(prog string) []string {
	return []string{
		"bash: source <(" + prog + " completion bash)",
		"zsh: source <(" + prog + " completion zsh)",
		"fish: " + prog + " completion fish | source",
	}
}

// allCommands is root and every command below it.
func allCommands(c *cobra.Command) []*cobra.Command {
	out := []*cobra.Command{c}
	for _, s := range c.Commands() {
		out = append(out, allCommands(s)...)
	}
	return out
}
