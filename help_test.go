package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/spf13/pflag"
)

// TestMain lets a test run this package's main() as a child process
// (runSelf): the binary is the test binary itself.
func TestMain(m *testing.M) {
	if os.Getenv("POLY_LSP_TEST_RUN_MAIN") == "1" {
		os.Args = os.Args[:1]
		for _, a := range strings.Fields(os.Getenv("POLY_LSP_TEST_ARGS")) {
			os.Args = append(os.Args, strings.Trim(a, "'")) // '' is an empty argument
		}
		main()
		return
	}
	os.Exit(m.Run())
}

// runSelf runs main() with args in a child process and returns what it wrote
// to stdout and stderr, and its exit code.
func runSelf(t *testing.T, args string, env ...string) (stdout, stderr string, code int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "LSP_REBUILD_PATH=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "POLY_LSP_TEST_RUN_MAIN=1", "POLY_LSP_TEST_ARGS="+args)
	cmd.Env = append(cmd.Env, env...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return so.String(), se.String(), code
}

// No help line is wider than the width, for every command, with and without
// the advanced flags.
func TestHelpWidth(t *testing.T) {
	root := newRoot()
	for _, width := range []int{50, 80, 120} {
		for _, c := range allCommands(root) {
			for _, all := range []bool{false, true} {
				var b bytes.Buffer
				writeHelp(&b, c, width, all)
				for _, line := range strings.Split(b.String(), "\n") {
					if n := utf8.RuneCountInString(line); n > width {
						t.Errorf("%s width %d all=%v: line of %d runes: %q", c.CommandPath(), width, all, n, line)
					}
				}
			}
		}
	}
}

// --help hides the advanced flags and says how to see them; --help-all lists
// them.
func TestHelpAdvancedFlags(t *testing.T) {
	root := newRoot()
	mcp, _, err := root.Find([]string{"mcp"})
	if err != nil {
		t.Fatal(err)
	}
	var plain, full bytes.Buffer
	writeHelp(&plain, mcp, 80, false)
	writeHelp(&full, mcp, 80, true)
	for _, f := range []string{"--lsp-idle-stop", "--index-budget", "--validate-wait", "--max-files"} {
		if strings.Contains(plain.String(), f+" ") {
			t.Errorf("mcp --help lists advanced %s", f)
		}
		if !strings.Contains(full.String(), f+" ") {
			t.Errorf("mcp --help-all does not list %s", f)
		}
	}
	for _, f := range []string{"--root ", "--read-only", "--validate, --no-validate", "--daemon, --no-daemon"} {
		if !strings.Contains(plain.String(), f) {
			t.Errorf("mcp --help does not list %s:\n%s", f, plain.String())
		}
	}
	if strings.Contains(plain.String(), "\n  --no-validate") || strings.Contains(plain.String(), "(default false)") || strings.Contains(plain.String(), "(default 0)") {
		t.Errorf("a --no-x entry of its own, or a zero default, is listed:\n%s", plain.String())
	}
	if !strings.HasSuffix(plain.String(), "more flags: poly-lsp-mcp mcp --help-all\n") {
		t.Errorf("mcp --help does not end with the --help-all hint:\n%s", plain.String())
	}
	if strings.Contains(full.String(), "more flags:") {
		t.Errorf("--help-all hints at itself")
	}
}

// Every flag of every command is in a flagGroups entry (or is the --no-x of
// one), so none falls into a nameless "options" section by accident.
func TestHelpEveryFlagGrouped(t *testing.T) {
	grouped := map[string]bool{}
	for _, g := range flagGroups {
		for _, n := range g.names {
			grouped[n] = true
		}
	}
	for n := range advancedFlags {
		if !grouped[n] {
			t.Errorf("advanced flag %s is in no group", n)
		}
	}
	for _, c := range allCommands(newRoot()) {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if f.Hidden || f.Name == "help" || f.Name == helpAllFlag || grouped[f.Name] {
				return
			}
			if base, ok := strings.CutPrefix(f.Name, "no-"); ok && c.Flags().Lookup(base) != nil {
				return
			}
			t.Errorf("%s --%s is in no flagGroups entry", c.CommandPath(), f.Name)
		})
	}
}

// Wrapping keeps the phrases clients detect whole, even where the width
// would break them.
func TestHelpKeepsMarkers(t *testing.T) {
	for _, width := range []int{50, 60, 80, 120} {
		mcp, _, _ := newRoot().Find([]string{"mcp"})
		var b bytes.Buffer
		writeHelp(&b, mcp, width, false)
		for _, m := range noBreak {
			if !strings.Contains(b.String(), m) {
				t.Errorf("width %d: mcp --help lacks %q", width, m)
			}
		}
	}
}

func TestHelpDefaults(t *testing.T) {
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	n := fs.Int("n", 0, "a count")
	k := fs.Int("k", 4, "a cap")
	d := fs.Int("d", 4, "a cap, default 4 here")
	m := fs.Int("m", -1, "unset = auto")
	s := fs.String("s", "x", "mode: x = one")
	_, _, _, _, _ = n, k, d, m, s
	want := map[string]string{"n": "", "k": " (default 4)", "d": "", "m": "", "s": ""}
	for name, w := range want {
		f := fs.Lookup(name)
		if got := flagDefault(f, f.Usage); got != w {
			t.Errorf("flagDefault(%s) = %q, want %q", name, got, w)
		}
	}
}

// --help prints to stdout, nothing to stderr, and exits 0; so does --help-all
// on a command that would otherwise run.
func TestHelpGoesToStdout(t *testing.T) {
	for _, args := range []string{"--help", "mcp --help", "mcp --help-all", "lsp add --help-all", "help mcp", "doctor -h"} {
		stdout, stderr, code := runSelf(t, args)
		if code != 0 {
			t.Errorf("%s: exit %d", args, code)
		}
		if stderr != "" {
			t.Errorf("%s: wrote to stderr: %q", args, stderr)
		}
		if !strings.Contains(stdout, "usage: poly-lsp-mcp") {
			t.Errorf("%s: stdout has no usage line:\n%s", args, stdout)
		}
		if strings.Contains(stdout, "\x1b[") {
			t.Errorf("%s: colour in the output", args)
		}
	}
}

func TestHelpCommandGroups(t *testing.T) {
	var b bytes.Buffer
	writeHelp(&b, newRoot(), 80, false)
	out := b.String()
	for _, want := range []string{"\nrun:\n  mcp ", "\ninspect:\n  status ", "\nsetup:\n  completion ", "start:\n", "source <(poly-lsp-mcp completion bash)", "more flags: poly-lsp-mcp <cmd> --help-all"} {
		if !strings.Contains(out, want) {
			t.Errorf("root help lacks %q:\n%s", want, out)
		}
	}
}
