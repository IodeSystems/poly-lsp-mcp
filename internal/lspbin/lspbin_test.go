package lspbin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fake builds a finder over a temp HOME. PATH is the colon list in path;
// lookPath honours it with the real executable check.
func fake(t *testing.T, goos, path string, env map[string]string) (*finder, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("PATH", path)
	f := &finder{
		goos:      goos,
		getenv:    func(k string) string { return env[k] },
		home:      home,
		lookPath:  exec.LookPath,
		npmPrefix: func(string, []string) string { return "" },
		noSystem:  true, // a runner's /usr/local/bin (npm on GitHub's ubuntu) must not leak in
	}
	return f, home
}

func put(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFallbackPnpmHome(t *testing.T) {
	f, home := fake(t, "linux", t.TempDir(), nil)
	want := put(t, filepath.Join(home, ".local/share/pnpm"), "tsls", "#!/bin/sh\n", 0o755)
	got, err := f.resolve("tsls")
	if err != nil || got.Path != want || !got.Fallback {
		t.Fatalf("got %+v, %v; want %s via fallback", got, err, want)
	}

	pnpm := t.TempDir()
	f.getenv = func(k string) string {
		if k == "PNPM_HOME" {
			return pnpm
		}
		return ""
	}
	want = put(t, pnpm, "tsls", "#!/bin/sh\n", 0o755)
	if got, _ = f.resolve("tsls"); got.Path != want {
		t.Fatalf("PNPM_HOME must come first: got %s want %s", got.Path, want)
	}
}

func TestPathWinsOverFallback(t *testing.T) {
	onPath := t.TempDir()
	f, home := fake(t, "linux", onPath, nil)
	put(t, filepath.Join(home, ".local/share/pnpm"), "tsls", "#!/bin/sh\n", 0o755)
	want := put(t, onPath, "tsls", "#!/bin/sh\n", 0o755)
	got, err := f.resolve("tsls")
	if err != nil || got.Path != want || got.Fallback {
		t.Fatalf("got %+v, %v; want %s on PATH", got, err, want)
	}
}

func TestNonExecutableIgnored(t *testing.T) {
	f, home := fake(t, "linux", t.TempDir(), nil)
	put(t, filepath.Join(home, ".local/share/pnpm"), "tsls", "x", 0o644)
	os.Mkdir(filepath.Join(home, ".cargo"), 0o755)
	os.MkdirAll(filepath.Join(home, ".cargo/bin/tsls"), 0o755) // a directory
	if got, err := f.resolve("tsls"); err == nil {
		t.Fatalf("resolved %+v", got)
	}
}

func TestSymlinkFollowed(t *testing.T) {
	f, home := fake(t, "linux", t.TempDir(), nil)
	target := put(t, t.TempDir(), "real", "#!/bin/sh\n", 0o755)
	dir := filepath.Join(home, ".bun/bin")
	os.MkdirAll(dir, 0o755)
	if err := os.Symlink(target, filepath.Join(dir, "tsls")); err != nil {
		t.Fatal(err)
	}
	if got, err := f.resolve("tsls"); err != nil || got.Dir() != dir {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestDarwinOnlyDirs(t *testing.T) {
	for _, tc := range []struct {
		goos string
		want bool
	}{{"linux", false}, {"darwin", true}} {
		f, home := fake(t, tc.goos, t.TempDir(), nil)
		f.noSystem = false // only lists dirs, never resolves
		lib := filepath.Join(home, "Library/pnpm")
		has := false
		for _, d := range f.dirs(false) {
			has = has || d == lib || d == "/opt/homebrew/bin"
		}
		if has != tc.want {
			t.Errorf("%s: darwin dirs present = %v, want %v", tc.goos, has, tc.want)
		}
	}
}

func TestDirOrderAndGoPath(t *testing.T) {
	gp := t.TempDir()
	f, home := fake(t, "linux", "", map[string]string{"GOPATH": gp + ":/other", "GOBIN": "/gobin", "PNPM_HOME": "/pn"})
	f.noSystem = false // only lists dirs, never resolves
	got := f.dirs(true)
	want := []string{"/pn", filepath.Join(home, ".local/share/pnpm"), npmMarker,
		filepath.Join(home, ".npm-global/bin"), filepath.Join(home, ".volta/bin"), filepath.Join(home, ".bun/bin"),
		"/gobin", filepath.Join(gp, "bin"), filepath.Join(home, ".cargo/bin"), filepath.Join(home, ".local/bin"), "/usr/local/bin"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	f, home = fake(t, "linux", "", nil)
	found := false
	for _, d := range f.dirs(false) {
		found = found || d == filepath.Join(home, "go/bin")
	}
	if !found {
		t.Error("default GOPATH dir ~/go/bin missing")
	}
}

func TestNpmPrefixOnceAndLazy(t *testing.T) {
	f, home := fake(t, "linux", t.TempDir(), nil)
	npmDir := t.TempDir()
	put(t, filepath.Join(home, ".local/bin"), "npm", "#!/bin/sh\n", 0o755)
	prefix := t.TempDir()
	calls := 0
	f.npmPrefix = func(npm string, _ []string) string { calls++; return prefix }
	_ = npmDir

	// Found before the npm entry: npm is never asked.
	put(t, filepath.Join(home, ".local/share/pnpm"), "early", "#!/bin/sh\n", 0o755)
	if _, err := f.resolve("early"); err != nil || calls != 0 {
		t.Fatalf("err %v calls %d", err, calls)
	}
	want := put(t, filepath.Join(prefix, "bin"), "late", "#!/bin/sh\n", 0o755)
	for i := 0; i < 3; i++ {
		got, err := f.resolve("late")
		if err != nil || got.Path != want {
			t.Fatalf("got %+v, %v", got, err)
		}
	}
	if _, err := f.resolve("nope"); err == nil {
		t.Fatal("resolved nope")
	}
	if calls != 1 {
		t.Fatalf("npm prefix ran %d times, want 1", calls)
	}
}

func TestNoNpmNoCall(t *testing.T) {
	f, _ := fake(t, "linux", t.TempDir(), nil)
	f.npmPrefix = func(string, []string) string { t.Fatal("called without npm"); return "" }
	if _, err := f.resolve("nope"); err == nil {
		t.Fatal("resolved")
	}
}

func TestShebangEnvNode(t *testing.T) {
	f, home := fake(t, "linux", t.TempDir(), nil)
	dir := filepath.Join(home, ".local/share/pnpm")
	put(t, dir, "node", "#!/bin/sh\n", 0o755)
	put(t, dir, "tsls", "#!/usr/bin/env node\n", 0o755)
	put(t, dir, "shim", "#!/bin/sh\nexec node x\n", 0o755)
	put(t, dir, "bin", "\x7fELF", 0o755)
	for _, name := range []string{"tsls", "shim"} {
		got, err := f.resolve(name)
		if err != nil || !got.Fallback {
			t.Fatalf("%s: %+v, %v", name, got, err)
		}
		var p string
		for _, e := range got.Env {
			if v, ok := strings.CutPrefix(e, "PATH="); ok {
				p = v
			}
		}
		if !strings.HasPrefix(p, dir+string(os.PathListSeparator)) {
			t.Errorf("%s: PATH %q does not lead with %s", name, p, dir)
		}
	}
	if got, _ := f.resolve("bin"); got.Env != nil {
		t.Errorf("binary got env %v", got.Env)
	}
}

func TestShebangNodeOnPathNeedsNoEnv(t *testing.T) {
	pathDir := t.TempDir()
	f, home := fake(t, "linux", pathDir, nil)
	put(t, pathDir, "node", "#!/bin/sh\n", 0o755)
	put(t, filepath.Join(home, ".local/share/pnpm"), "tsls", "#!/usr/bin/env node\n", 0o755)
	if got, err := f.resolve("tsls"); err != nil || got.Env != nil {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestSlashCommandAsIs(t *testing.T) {
	f, home := fake(t, "linux", t.TempDir(), nil)
	put(t, filepath.Join(home, ".cargo/bin"), "x", "#!/bin/sh\n", 0o755)
	if _, err := f.resolve("./x"); err == nil {
		t.Fatal("slash command searched fallback dirs")
	}
	p := put(t, t.TempDir(), "x", "#!/bin/sh\n", 0o755)
	if got, err := f.resolve(p); err != nil || got.Path != p || got.Fallback {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestPrependPath(t *testing.T) {
	got := PrependPath([]string{"A=1", "PATH=/x:/y"}, "/d")
	if strings.Join(got, " ") != "A=1 PATH=/d:/x:/y" {
		t.Fatal(got)
	}
}
