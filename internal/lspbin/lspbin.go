// Package lspbin finds a child LSP's executable.
//
// PATH comes first. A client launched without the shell's rc PATH (a GUI
// app, a launcher, a daemon auto-started by one: the daemon keeps the
// starting proxy's environment) cannot see LSPs installed with pnpm -g,
// npm -g, volta, bun, go install or cargo, so a fixed list of well-known
// user bin dirs is searched after it. The list is not configurable. A
// fallback never wins over a PATH hit.
package lspbin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Found is where a command was found.
type Found struct {
	Path     string // absolute (or as given, when the command had a slash)
	Fallback bool   // not on PATH; found in a well-known dir
	// Env is nil, or the environment a child needs to run Path: the caller's
	// plus a PATH that leads with the dir of an interpreter PATH lacks.
	Env []string
}

// Dir is the directory Path lives in.
func (f Found) Dir() string { return filepath.Dir(f.Path) }

// Resolve finds cmd: PATH first, then the fallback dirs. A cmd with a path
// separator is used as is. err is the PATH lookup's, when nothing is found.
func Resolve(cmd string) (Found, error) { return system.resolve(cmd) }

// finder holds the inputs of a lookup, so tests can replace them.
type finder struct {
	goos      string
	getenv    func(string) string
	home      string // "" reads the user home at lookup time
	lookPath  func(string) (string, error)
	npmPrefix func(npm string, env []string) string // `npm prefix -g`, "" on failure

	npmOnce sync.Once
	npmBin  string
}

var system = &finder{
	goos:      runtime.GOOS,
	getenv:    os.Getenv,
	lookPath:  exec.LookPath,
	npmPrefix: runNpmPrefix,
}

func (f *finder) resolve(cmd string) (Found, error) {
	p, err := f.lookPath(cmd)
	if err == nil {
		return Found{Path: p}, nil
	}
	if strings.ContainsRune(cmd, '/') || strings.ContainsRune(cmd, filepath.Separator) {
		return Found{}, err
	}
	for _, d := range f.dirs(true) {
		if d == npmMarker {
			if d = f.npmGlobalBin(); d == "" {
				continue
			}
		}
		if c := filepath.Join(d, cmd); executable(c) {
			return Found{Path: c, Fallback: true, Env: f.interpreterEnv(c)}, nil
		}
	}
	return Found{}, err
}

// npmMarker stands in the list for `npm prefix -g`/bin, which costs a
// process, so the caller resolves it only when it reaches it.
const npmMarker = "\x00npm"

// dirs is the fallback list, in order. withNpm includes npmMarker.
func (f *finder) dirs(withNpm bool) []string {
	var l []string
	add := func(d string) {
		if d == npmMarker || d != "" && filepath.IsAbs(d) {
			l = append(l, d)
		}
	}
	h := f.home
	if h == "" {
		h, _ = os.UserHomeDir()
	}
	home := func(rel ...string) string {
		if h == "" {
			return ""
		}
		return filepath.Join(append([]string{h}, rel...)...)
	}
	add(f.getenv("PNPM_HOME"))
	add(home(".local", "share", "pnpm"))
	if f.goos == "darwin" {
		add(home("Library", "pnpm"))
	}
	if withNpm {
		add(npmMarker)
	}
	add(home(".npm-global", "bin"))
	add(home(".volta", "bin"))
	add(home(".bun", "bin"))
	add(f.getenv("GOBIN"))
	gopath := ""
	if p := filepath.SplitList(f.getenv("GOPATH")); len(p) > 0 {
		gopath = p[0]
	}
	if gopath != "" {
		add(filepath.Join(gopath, "bin"))
	} else {
		add(home("go", "bin"))
	}
	add(home(".cargo", "bin"))
	add(home(".local", "bin"))
	if f.goos == "darwin" {
		add("/opt/homebrew/bin")
	}
	add("/usr/local/bin")

	seen := map[string]bool{}
	out := l[:0]
	for _, d := range l {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// npmGlobalBin is `npm prefix -g`/bin, asked of npm at most once per
// process. npm is taken from PATH, else from the other fallback dirs. ""
// when there is no npm or it fails.
func (f *finder) npmGlobalBin() string {
	f.npmOnce.Do(func() {
		npm, err := f.lookPath("npm")
		env := []string(nil)
		if err != nil {
			for _, d := range f.dirs(false) {
				if c := filepath.Join(d, "npm"); executable(c) {
					npm, env = c, f.interpreterEnv(c)
					err = nil
					break
				}
			}
		}
		if err != nil {
			return
		}
		if p := f.npmPrefix(npm, env); p != "" {
			f.npmBin = filepath.Join(p, "bin")
		}
	})
	return f.npmBin
}

// interpreterEnv returns the environment for running the script at path,
// when its shebang names an interpreter PATH lacks but a fallback dir has
// (`#!/usr/bin/env node`; pnpm's sh shims call node too). nil when PATH
// suffices or the file is not a script.
func (f *finder) interpreterEnv(path string) []string {
	want := shebangInterpreter(path)
	if want == "" {
		return nil
	}
	if _, err := f.lookPath(want); err == nil {
		return nil
	}
	for _, d := range append([]string{filepath.Dir(path)}, f.dirs(false)...) {
		if executable(filepath.Join(d, want)) {
			return PrependPath(os.Environ(), d)
		}
	}
	return nil
}

// shebangInterpreter returns the program a script needs found by name:
// X in `#!/usr/bin/env X`, and node for any other shebang script that sits
// in a dir holding node (a pnpm shim is `#!/bin/sh` and execs node).
func shebangInterpreter(path string) string {
	fh, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer fh.Close()
	buf := make([]byte, 256)
	n, _ := fh.Read(buf)
	line, _, _ := strings.Cut(string(buf[:n]), "\n")
	if !strings.HasPrefix(line, "#!") {
		return ""
	}
	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	if len(fields) >= 2 && filepath.Base(fields[0]) == "env" {
		for _, a := range fields[1:] {
			if !strings.HasPrefix(a, "-") {
				return a
			}
		}
	}
	return "node"
}

// PrependPath returns env with dir at the front of its PATH.
func PrependPath(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	old := ""
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			old = v
			continue
		}
		out = append(out, e)
	}
	if old != "" {
		dir += string(os.PathListSeparator) + old
	}
	return append(out, "PATH="+dir)
}

// executable is true for a regular file (symlinks followed) with an
// execute bit.
func executable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}

func runNpmPrefix(npm string, env []string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, npm, "prefix", "-g")
	c.Env = env
	out, err := c.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
