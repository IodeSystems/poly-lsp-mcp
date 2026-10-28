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

	"github.com/iodesystems/poly-lsp-mcp/daemon"
	"github.com/iodesystems/poly-lsp-mcp/internal/ts"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// doctor names the running daemon's build and says whether it is this
// binary's — the check a person runs when --daemon answers look stale.
func TestDoctorReportsTheDaemonBuild(t *testing.T) {
	home := t.TempDir()
	t.Setenv(daemon.HomeEnv, home)
	st := daemon.State{PID: os.Getpid(), Socket: "/nowhere", Build: "0123456789ab/deadbeef"}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(daemon.StatePath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	// A refused build and a kept binary: doctor names both.
	fails := `{"bad/00000001":{"failures":2,"error":"exited","at":"2026-10-24T00:00:00Z"}}`
	if err := os.WriteFile(daemon.UpgradeFailuresPath(), []byte(fails), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemon.PrevExePath(), []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := newRoot()
	root.SetArgs([]string{"doctor", "--root", t.TempDir(), "--json"})
	stdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	_ = root.Execute()
	w.Close()
	os.Stdout = stdout
	out, _ := io.ReadAll(r)
	var rep doctorReport
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.DaemonBuild != st.Build || rep.DaemonMatches == nil || *rep.DaemonMatches {
		t.Fatalf("daemonBuild=%q matches=%v, want %q and false", rep.DaemonBuild, rep.DaemonMatches, st.Build)
	}
	if rep.BuildID != daemon.BuildID() || !strings.Contains(rep.Daemon, "NOT this binary's") {
		t.Fatalf("buildId=%q daemon=%q", rep.BuildID, rep.Daemon)
	}
	if rep.UpgradeFailures["bad/00000001"].Failures != 2 || rep.PrevExe != daemon.PrevExePath() {
		t.Fatalf("upgradeFailures=%v prevExe=%q", rep.UpgradeFailures, rep.PrevExe)
	}
	if rep.DaemonHome != home || rep.DaemonHomeSource != "$"+daemon.HomeEnv {
		t.Errorf("daemonHome=%q source=%q, want %q from $%s", rep.DaemonHome, rep.DaemonHomeSource, home, daemon.HomeEnv)
	}
	if !strings.Contains(strings.Join(rep.Advisories, "\n"), "REFUSED") {
		t.Errorf("advisories do not say the build is refused: %v", rep.Advisories)
	}
}

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

// `index` and `doctor` on a directory of clones say what the server says:
// not a project, the repositories found, and the ways out; `index
// --index-anyway` indexes it.
func TestIndexAndDoctorRefuseDirOfClones(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(daemon.HomeEnv, t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir := t.TempDir()
	for _, r := range []string{"a", "b", "c"} {
		if err := os.MkdirAll(filepath.Join(dir, r, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, r, "main.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) ([]byte, error) {
		root := newRoot()
		root.SetArgs(args)
		stdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		err := root.Execute()
		w.Close()
		os.Stdout = stdout
		b, _ := io.ReadAll(r)
		return b, err
	}

	_, err := run("index", "--root", dir)
	var np *symbols.NotProjectError
	if !errors.As(err, &np) || !slices.Equal(np.Repos, []string{"a", "b", "c"}) {
		t.Fatalf("index: want a not-a-project refusal naming a, b, c; got %v", err)
	}
	if d := np.Detail(); !strings.Contains(d, "--root "+filepath.Join(dir, "a")) || !strings.Contains(d, "--index-anyway") {
		t.Errorf("the refusal names both ways out:\n%s", d)
	}
	if out, err := run("index", "--root", dir, "--index-anyway"); err != nil || !strings.Contains(string(out), "index: 3 files") {
		t.Errorf("index --index-anyway: %v\n%s", err, out)
	}

	out, err := run("doctor", "--root", dir, "--json")
	if !errors.Is(err, errDoctor) {
		t.Fatalf("doctor: want errDoctor, got %v", err)
	}
	var rep doctorReport
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	found := false
	for _, p := range rep.Problems {
		found = found || (strings.Contains(p, "not a project") && strings.Contains(p, "a, b, c") && strings.Contains(p, "--index-anyway"))
	}
	if !found {
		t.Errorf("doctor problems should carry the refusal: %v", rep.Problems)
	}
}

// The index flags replay too, each --include on its own, so a --restart or
// an upgrade's new daemon indexes as the old one did.
func TestDaemonReplaysIndexFlags(t *testing.T) {
	cmd, _, err := newRoot().Find([]string{"daemon"})
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--include", "vendor/", "--include", "*.min.js", "--index-anyway", "--symlink-depth", "2", "--exclude", "x", "--index-budget", "5000ops"}
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	got := replayFlags(cmd.Flags(), "stop", "restart", "help")
	want := []string{"--exclude=x", "--include=vendor/", "--include=*.min.js", "--index-anyway=true", "--index-budget=5000ops", "--symlink-depth=2"}
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
	// The fallback dirs (internal/lspbin) must not find a real gopls either.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PNPM_HOME", "")
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"doctor", "--root", dir, "--json", "--no-update-check"})
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
	advised := slices.ContainsFunc(rep.Advisories, func(a string) bool {
		return strings.HasPrefix(a, ts.FastPathHint+": ") && !strings.Contains(a, "@latest")
	})
	if advised != (ts.Runtime() == "pure-go") {
		t.Errorf("fast-path advisory present = %v on runtime %s: %v", advised, ts.Runtime(), rep.Advisories)
	}
}

// `mcp --help` carries the phrases clients detect: dun's two for the
// proxy (--hook's and --daemon's), and the one that says daemon mode is
// the default.
func TestMCPHelpMarkers(t *testing.T) {
	root := newRoot()
	var out bytes.Buffer
	root.SetArgs([]string{"mcp", "--help"})
	root.SetOut(&out)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"--daemon: the proxy runs it", "a root outside --allow is admitted", "daemon mode is the default"} {
		if !strings.Contains(out.String(), m) {
			t.Errorf("mcp --help does not contain %q:\n%s", m, out.String())
		}
	}
}

// Daemon mode is the default; --no-daemon, $POLY_LSP_NO_DAEMON=1 and an
// in-process-only setting given without --daemon choose in-process.
func TestMCPChooseDaemon(t *testing.T) {
	t.Chdir(t.TempDir()) // no poly-lsp-mcp.yaml here
	for _, tc := range []struct {
		args    []string
		env     map[string]string
		want    bool
		wantErr bool
	}{
		{args: nil, want: true},
		{args: []string{"--daemon"}, want: true},
		{args: []string{"--no-daemon"}, want: false},
		{args: []string{"--daemon=false"}, want: false},
		{args: []string{"--daemon", "--no-daemon"}, wantErr: true},
		{args: []string{"--read-only", "--exclude", "x", "--lsp-roots", "auto", "--hook", "/h"}, want: true},
		{args: []string{"--lsp-off", "go,python", "--lsp-on", "zig"}, want: true},
		{args: []string{"--format-on-edit", "go,typescript", "--no-format-on-edit", "python"}, want: true},
		{args: []string{"--legacy-tools"}, want: false},
		{args: []string{"--read-char-budget", "9000"}, want: false},
		{args: []string{"--config", "c.yaml"}, want: false},
		{args: []string{"--lsp-idle-stop", "1m"}, want: false},
		{args: []string{"--lsp-max-rss-mb", "1"}, want: false},
		{args: []string{"--lsp-psi-threshold", "0"}, want: false},
		{args: []string{"--lsp-roots-max", "2"}, want: false},
		{args: []string{"--daemon", "--legacy-tools"}, want: true},
		{env: map[string]string{"POLY_LSP_NO_DAEMON": "1"}, want: false},
		{args: []string{"--daemon"}, env: map[string]string{"POLY_LSP_NO_DAEMON": "1"}, want: true},
		{args: []string{"--no-daemon=false"}, env: map[string]string{"POLY_LSP_NO_DAEMON": "1"}, want: true},
		{env: map[string]string{"POLY_LSP_READ_CHAR_BUDGET": "9000"}, want: false},
	} {
		t.Setenv("POLY_LSP_NO_DAEMON", "")
		t.Setenv("POLY_LSP_READ_CHAR_BUDGET", "")
		for k, v := range tc.env {
			t.Setenv(k, v)
		}
		root := newRoot()
		cmd, rest, err := root.Find(append([]string{"mcp"}, tc.args...))
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.ParseFlags(rest); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		d, _ := cmd.Flags().GetBool("daemon")
		nd, _ := cmd.Flags().GetBool("no-daemon")
		got, err := chooseDaemon(cmd, d, nd)
		if (err != nil) != tc.wantErr || (err == nil && got != tc.want) {
			t.Errorf("args %v env %v: daemon=%v err=%v, want daemon=%v err=%v", tc.args, tc.env, got, err, tc.want, tc.wantErr)
		}
	}
	// A config file at the default --config path is the client's own
	// config, which the daemon would not read: in-process.
	if err := os.WriteFile("poly-lsp-mcp.yaml", []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, _, _ := newRoot().Find([]string{"mcp"})
	_ = cmd.ParseFlags(nil)
	if got, err := chooseDaemon(cmd, false, false); err != nil || got {
		t.Errorf("with ./poly-lsp-mcp.yaml: daemon=%v err=%v, want in-process", got, err)
	}
}

// lsp add / list / remove round-trip servers.yaml in the daemon home:
// add validates before it writes, list shows the merged registry with
// sources, remove restores a built-in.
func TestLSPAddListRemove(t *testing.T) {
	t.Chdir(t.TempDir()) // no poly-lsp-mcp.yaml here
	home := t.TempDir()
	t.Setenv(daemon.HomeEnv, home)
	run := func(args ...string) (string, error) {
		t.Helper()
		root := newRoot()
		root.SetArgs(args)
		stdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		err := root.Execute()
		w.Close()
		os.Stdout = stdout
		out, _ := io.ReadAll(r)
		return string(out), err
	}
	if _, err := run("lsp", "add", "zig", "--ext", ".zig,zon", "--cmd", "zls", "--arg", "--x", "--env", "A=b", "--root-marker", "build.zig", "--trust-clean-diagnostics"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("lsp", "add", "go", "--off-by-default"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("lsp", "add", "bad", "--ext", ".go", "--cmd", "x"); err == nil || !strings.Contains(err.Error(), "claimed by both go and bad") {
		t.Fatalf("an extension go claims: %v", err)
	}
	if _, err := run("lsp", "add", "nocmd", "--ext", "q"); err == nil || !strings.Contains(err.Error(), "cmd is required") {
		t.Fatalf("no cmd: %v", err)
	}
	out, err := run("lsp", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rep lspListReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Servers != filepath.Join(home, "servers.yaml") {
		t.Errorf("servers path %q", rep.Servers)
	}
	byName := map[string]lspEntry{}
	for _, e := range rep.Langs {
		byName[e.Name] = e
	}
	zig, g, py := byName["zig"], byName["go"], byName["python"]
	if zig.Source != "user" || zig.Overrides || zig.LSP == nil || zig.LSP.Name != "zls" || !slices.Equal(zig.Extensions, []string{"zig", "zon"}) ||
		!slices.Equal(zig.LSP.Args, []string{"--x"}) || !slices.Equal(zig.RootMarkers, []string{"build.zig"}) {
		t.Errorf("zig: %+v", zig)
	}
	if g.Source != "user" || !g.Overrides || !g.OffByDefault || g.LSP == nil || g.LSP.Name != "gopls" {
		t.Errorf("go: %+v", g)
	}
	if py.Source != "builtin" || py.OffByDefault {
		t.Errorf("python: %+v", py)
	}
	if out, err := run("lsp", "remove", "go"); err != nil || !strings.Contains(out, "built-in go is restored") {
		t.Fatalf("remove go: %v %q", err, out)
	}
	if _, err := run("lsp", "remove", "go"); err == nil {
		t.Fatal("removing a missing entry succeeded")
	}
	out, _ = run("lsp", "list")
	if !strings.Contains(out, "zig") || strings.Contains(out, "overrides builtin") {
		t.Errorf("lsp list after remove:\n%s", out)
	}
}
