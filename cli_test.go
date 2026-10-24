package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/internal/ts"
)

// An unknown subcommand used to fall through to the stdio LSP server, which
// waits on stdin: `poly-lsp-mcp explain …` looked like a hang. It must be an
// error that names the command.
func TestUnknownSubcommandIsAnError(t *testing.T) {
	root := newRoot()
	root.SetArgs([]string{"explain", "--root", ".", "#x"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), `unknown command "explain"`) {
		t.Fatalf("err = %v, want unknown command \"explain\"", err)
	}
}

// Root help must list the subcommands; it used to show only --config.
func TestRootHelpListsSubcommands(t *testing.T) {
	root := newRoot()
	var out bytes.Buffer
	root.SetArgs([]string{"--help"})
	root.SetOut(&out)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"lsp", "mcp", "query", "index", "status", "usage", "doctor", "daemon"} {
		if !strings.Contains(out.String(), "  "+sub+" ") {
			t.Errorf("root --help does not list %q:\n%s", sub, out.String())
		}
	}
}

// --restart relaunches the daemon with the flags it was given, minus the
// control flags, each repeat of --allow kept.
func TestDaemonReplayFlags(t *testing.T) {
	root := newRoot()
	cmd, _, err := root.Find([]string{"daemon"})
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--restart", "--allow", "/a", "--allow", "/b", "--read-only", "--lsp-idle-stop", "5m"}
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	got := replayFlags(cmd.Flags(), "stop", "restart", "help")
	want := []string{"--allow=/a", "--allow=/b", "--lsp-idle-stop=5m0s", "--read-only=true"}
	if !slices.Equal(got, want) {
		t.Fatalf("replayFlags = %q, want %q", got, want)
	}
}

// doctor names a language the workspace holds whose child LSP is not on
// PATH, says how to install it, and exits non-zero.
func TestDoctorFlagsMissingLSP(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // nothing on it: no gopls, no git
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"doctor", "--root", dir, "--json"})
	stdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := root.Execute()
	w.Close()
	os.Stdout = stdout
	b, _ := io.ReadAll(r)
	if !errors.Is(err, errDoctor) {
		t.Fatalf("want errDoctor, got %v", err)
	}
	var rep doctorReport
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	found := false
	for _, p := range rep.Problems {
		if strings.Contains(p, "gopls is not on PATH") && strings.Contains(p, "go install golang.org/x/tools/gopls") && strings.Contains(p, "needs go") {
			found = true
		}
	}
	if !found {
		t.Errorf("problems should name gopls and how to install it: %v", rep.Problems)
	}
	if len(rep.Languages) != 1 || rep.Languages[0].Install == nil || rep.Languages[0].Install.Needs[0] != "go" {
		t.Errorf("languages should carry the machine-readable hint: %+v", rep.Languages)
	}
	if rep.Runtime != ts.Runtime() {
		t.Errorf("runtime = %q, want %q", rep.Runtime, ts.Runtime())
	}
	// The slow parser is said out loud, with the way to the fast one.
	advised := slices.Contains(rep.Advisories, ts.FastPathHint)
	if advised != (ts.Runtime() == "pure-go") {
		t.Errorf("fast-path advisory present = %v on runtime %s: %v", advised, ts.Runtime(), rep.Advisories)
	}
}
