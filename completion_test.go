package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// complete runs `__complete args...` in a child process with POLY_LSP_HOME
// and XDG_RUNTIME_DIR set, and returns the candidate words (the part before
// a tab) the shell would be offered.
func complete(t *testing.T, home, runtime string, args ...string) []string {
	t.Helper()
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + a + "'"
	}
	stdout, stderr, code := runSelf(t, "__complete "+strings.Join(quoted, " "), "POLY_LSP_HOME="+home, "XDG_RUNTIME_DIR="+runtime)
	if code != 0 {
		t.Fatalf("__complete %v: exit %d: %s", args, code, stderr)
	}
	var words []string
	for _, line := range strings.Split(stdout, "\n") {
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "Completion ended") {
			continue
		}
		words = append(words, strings.SplitN(line, "\t", 2)[0])
	}
	return words
}

func contains(words []string, w string) bool {
	for _, x := range words {
		if x == w {
			return true
		}
	}
	return false
}

// Each shell's script loads, and asks the binary for the words, so every
// subcommand and key flag of the tree is completed: the test asks for them.
func TestCompletionScripts(t *testing.T) {
	home := t.TempDir()
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		out, stderr, code := runSelf(t, "completion "+shell, "POLY_LSP_HOME="+home)
		if code != 0 || out == "" {
			t.Fatalf("completion %s: exit %d, %d bytes, stderr %q", shell, code, len(out), stderr)
		}
		if !strings.Contains(out, "__complete") {
			t.Errorf("%s script does not ask the binary for the words", shell)
		}
		if shell == "powershell" {
			continue
		}
		path, err := exec.LookPath(shell)
		if err != nil {
			t.Logf("%s syntax check skipped: %s is not installed", shell, shell)
			continue
		}
		file := filepath.Join(t.TempDir(), "completion."+shell)
		if err := os.WriteFile(file, []byte(out), 0o600); err != nil {
			t.Fatal(err)
		}
		flag := "-n"
		if shell == "fish" {
			flag = "--no-execute"
		}
		if b, err := exec.Command(path, flag, file).CombinedOutput(); err != nil {
			t.Errorf("%s %s: %v\n%s", shell, flag, err, b)
		}
	}

	subs := complete(t, home, t.TempDir(), "")
	for _, s := range []string{"mcp", "lsp", "daemon", "status", "doctor", "index", "query", "usage", "completion"} {
		if !contains(subs, s) {
			t.Errorf("subcommand %s is not completed: %v", s, subs)
		}
	}
	flags := complete(t, home, t.TempDir(), "mcp", "--")
	for _, f := range []string{"--root", "--read-only", "--lsp-off", "--lsp-roots", "--daemon-home", "--help-all"} {
		if !contains(flags, f) {
			t.Errorf("mcp flag %s is not completed: %v", f, flags)
		}
	}
}

// With no shell named, the one in $SHELL is printed; with a shell that is
// not one, the command says so.
func TestCompletionShellFromEnv(t *testing.T) {
	out, _, code := runSelf(t, "completion", "SHELL=/usr/bin/fish")
	if code != 0 || !strings.Contains(out, "fish completion") {
		t.Errorf("SHELL=fish: exit %d: %.80q", code, out)
	}
	_, stderr, code := runSelf(t, "completion", "SHELL=/bin/tcsh")
	if code == 0 || !strings.Contains(stderr, "cannot tell the shell from $SHELL") {
		t.Errorf("SHELL=tcsh: exit %d, stderr %q", code, stderr)
	}
}

// Dynamic completions read the status files and servers.yaml of the daemon
// home, and start nothing: no daemon, no socket, no lock, no log, and the
// status files stay as they were (a dead process's file is not removed).
func TestCompletionDynamicStartsNothing(t *testing.T) {
	home, runtime := t.TempDir(), t.TempDir()
	children := filepath.Join(home, "children")
	if err := os.MkdirAll(children, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A pid above the default pid_max: not a live process.
	write(filepath.Join(children, "1.json"), `{"pid":4194305,"root":"/work/alpha"}`)
	write(filepath.Join(children, "2.json"), `{"pid":4194306,"root":"/work/beta"}`)
	write(filepath.Join(children, "bad.json"), `not json`)
	write(filepath.Join(home, "servers.yaml"), "servers:\n  - name: zig\n    extensions: [.zig]\n    cmd: zls\n")

	other := t.TempDir() // a second home, named by --daemon-home
	if err := os.MkdirAll(filepath.Join(other, "children"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(other, "children", "9.json"), `{"pid":4194307,"root":"/elsewhere"}`)

	snapshot := func() string {
		var names []string
		for _, d := range []string{home, runtime, other} {
			_ = filepath.WalkDir(d, func(p string, _ os.DirEntry, _ error) error {
				names = append(names, p)
				return nil
			})
		}
		return strings.Join(names, "\n")
	}
	before := snapshot()

	if got := complete(t, home, runtime, "mcp", "--root", ""); len(got) != 2 || !contains(got, "/work/alpha") || !contains(got, "/work/beta") {
		t.Errorf("--root = %v, want the two status roots", got)
	}
	if got := complete(t, home, runtime, "index", "--root", "/work/b"); len(got) != 1 || got[0] != "/work/beta" {
		t.Errorf("--root /work/b = %v", got)
	}
	if got := complete(t, home, runtime, "daemon", "--allow", "/work/a"); len(got) != 1 || got[0] != "/work/alpha" {
		t.Errorf("--allow = %v", got)
	}
	if got := complete(t, home, runtime, "lsp", "remove", ""); len(got) != 1 || got[0] != "zig" {
		t.Errorf("lsp remove = %v, want [zig]", got)
	}
	if got := complete(t, home, runtime, "lsp", "add", "z"); len(got) != 1 || got[0] != "zig" {
		t.Errorf("lsp add z = %v, want [zig]", got)
	}
	if got := complete(t, home, runtime, "mcp", "--lsp-roots", ""); len(got) != 2 || !contains(got, "single") || !contains(got, "auto") {
		t.Errorf("--lsp-roots = %v", got)
	}
	if got := complete(t, home, runtime, "mcp", "--lsp-off", "go,zi"); len(got) != 1 || got[0] != "go,zig" {
		t.Errorf("--lsp-off go,zi = %v", got)
	}
	if got := complete(t, home, runtime, "mcp", "--lsp-off", "go,p"); contains(got, "go,go") || !contains(got, "go,python") {
		t.Errorf("--lsp-off go,p = %v", got)
	}
	// --daemon-home on the line moves the lookup.
	if got := complete(t, home, runtime, "mcp", "--daemon-home", other, "--root", ""); len(got) != 1 || got[0] != "/elsewhere" {
		t.Errorf("--daemon-home %s --root = %v", other, got)
	}

	if after := snapshot(); after != before {
		t.Errorf("a completion changed the files:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	for _, f := range []string{"daemon.json", "daemon.lock", "daemon.log", "daemon.sock"} {
		if _, err := os.Stat(filepath.Join(home, f)); err == nil {
			t.Errorf("a completion created %s", f)
		}
	}
}

// Descriptions are cut to a menu line by runes: a cut in the middle of a
// multi-byte character would print as garbage.
func TestFirstClauseRunes(t *testing.T) {
	got := firstClause(strings.Repeat("é", 100))
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 60 {
		t.Errorf("firstClause = %q (%d runes), want 60 valid runes", got, utf8.RuneCountInString(got))
	}
	for in, want := range map[string]string{
		"stop a child: it starts again":  "stop a child",
		"index the root (see --x); more": "index the root",
		"one. Two sentences":             "one",
		"plain":                          "plain",
		strings.Repeat("a", 61):          strings.Repeat("a", 57) + "...",
	} {
		if got := firstClause(in); got != want {
			t.Errorf("firstClause(%q) = %q, want %q", in, got, want)
		}
	}
}

// The menu lines of a real request are short.
func TestCompletionDescriptionsAreShort(t *testing.T) {
	stdout, _, code := runSelf(t, "__complete mcp --", "POLY_LSP_HOME="+t.TempDir())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, line := range strings.Split(stdout, "\n") {
		if n := utf8.RuneCountInString(line); n > 90 {
			t.Errorf("long menu line (%d runes): %q", n, line)
		}
	}
}
