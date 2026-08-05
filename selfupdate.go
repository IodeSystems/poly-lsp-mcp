package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Dev self-update — the whole mechanism, in one file. A binary told where its
// own source lives rebuilds itself in place when that tree changed, then
// re-execs.
//
// It exists because poly-lsp-mcp is SPAWNED, never launched by hand (dun runs
// `poly-lsp-mcp mcp --root <ws>` off PATH; editors spawn the LSP), so nothing
// in the loop ever notices the binary is old. This repo demonstrated the cost:
// a dogfooding session ran for hours against a build that predated the fix it
// was hunting, and one live server was still executing a binary that had
// already been DELETED from disk by a later `go install`.
//
// The trigger is an ENVIRONMENT variable, deliberately, and not a build-time
// stamp. A stamp travels inside the binary: copy that binary to another
// machine and it carries a path it will rebuild from, and whoever controls
// that path controls the binary. LSP_REBUILD_PATH cannot travel — a
// distributed build contains no path at all and nothing to disable, so the
// safe configuration is the DEFAULT rather than something a release build has
// to remember to turn off.
//
//	export LSP_REBUILD_PATH=~/local/src/iodesystems/poly-lsp-mcp
//
// Set it in the profile that starts your agent/editor, since the spawned
// server inherits its environment. Unset it to turn this off; there is no
// second switch.
//
// The path is not trusted just because it was named. Before anything is built
// it must be an absolute directory, OWNED by the user running this process and
// not world-writable (else another account could swap the source out from
// under the rebuild), and its go.mod must declare THIS module (else a mistyped
// path silently rebuilds the binary from some other program). A named path
// that fails any of those says so on stderr — a switch you set and that then
// does nothing silently is worse than one that refuses out loud.
//
// LSP_REBUILD_DONE guards the one re-exec after a rebuild (no loop). The
// detached daemon inherits it, so `mcp --daemon` cannot ping-pong.
//
// A build failure (a dirty tree that doesn't compile) is NON-FATAL: warn and
// run the current binary. A code server that refuses to start because the tree
// is mid-edit is worse than a slightly stale one.
//
// Everything it prints goes to STDERR. stdout is the MCP/LSP JSON-RPC channel
// and a single stray byte there desynchronizes the client.
const (
	rebuildPathEnv = "LSP_REBUILD_PATH"
	rebuildDoneEnv = "LSP_REBUILD_DONE"

	// The module the source tree must declare. A rebuild replaces the running
	// binary, so "some Go module lives here" is not enough — it has to be THIS
	// one.
	rebuildModule = "github.com/iodesystems/poly-lsp-mcp"
)

func selfUpdate() {
	dir, ok := rebuildDir()
	if !ok {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	st, err := os.Stat(exe)
	if err != nil || !sourceNewerThan(dir, st.ModTime()) {
		return // up to date (or can't tell) — proceed normally
	}

	fmt.Fprintln(os.Stderr, "poly-lsp-mcp: source changed — rebuilding…")
	if err := rebuildSelf(dir, exe); err != nil {
		fmt.Fprintf(os.Stderr, "poly-lsp-mcp: rebuild failed (%v) — running the current binary\n", err)
		return
	}
	// Re-exec the freshly built binary with the same args. syscall.Exec keeps
	// the file descriptors, so an MCP client that already opened the stdio pipe
	// stays connected across the swap; the guard stops a loop.
	env := append(os.Environ(), rebuildDoneEnv+"=1")
	if err := syscall.Exec(exe, os.Args, env); err != nil {
		fmt.Fprintf(os.Stderr, "poly-lsp-mcp: re-exec failed (%v) — running the current binary\n", err)
	}
}

// rebuildDir resolves LSP_REBUILD_PATH and vets it, reporting whether a
// rebuild from it is allowed. Every rejection of a NAMED path is announced;
// the unset case is the ordinary default and stays silent.
func rebuildDir() (string, bool) {
	raw := strings.TrimSpace(os.Getenv(rebuildPathEnv))
	if raw == "" || os.Getenv(rebuildDoneEnv) != "" {
		return "", false
	}
	// A quoted assignment (LSP_REBUILD_PATH='~/src/…') reaches us with the
	// tilde unexpanded, which is a config that looks right and cannot work.
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			raw = filepath.Join(home, strings.TrimPrefix(raw, "~"))
		}
	}
	if !filepath.IsAbs(raw) {
		return refuse(raw, "not an absolute path")
	}
	fi, err := os.Stat(raw)
	if err != nil {
		return refuse(raw, fmt.Sprintf("cannot stat it (%v)", err))
	}
	if !fi.IsDir() {
		return refuse(raw, "not a directory")
	}
	if why := unsafeOwner(fi); why != "" {
		return refuse(raw, why)
	}
	if !declaresModule(raw, rebuildModule) {
		return refuse(raw, "its go.mod does not declare "+rebuildModule)
	}
	return raw, true
}

func refuse(path, why string) (string, bool) {
	fmt.Fprintf(os.Stderr, "poly-lsp-mcp: %s=%s ignored — %s\n", rebuildPathEnv, path, why)
	return "", false
}

// unsafeOwner reports why a source directory must not be built from, or "".
//
// The rebuild runs `go build` in this directory and then REPLACES the running
// binary with the result, so write access to it is write access to the binary.
// Anything another ACCOUNT can modify is disqualified.
//
// World-writable only, deliberately — not group. The user-private-group
// convention makes an ordinary checkout group-writable and gives that group
// exactly one member: measured on this box, umask 002 and `git clone` produce
// a 0775 directory owned by nthalk:nthalk. Refusing that would refuse every
// normal checkout, i.e. disable the feature while appearing to guard it. Where
// a group really is shared, the group is an administrative choice about who is
// already trusted; o+w is nobody's choice.
func unsafeOwner(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "cannot determine its owner"
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Sprintf("owned by uid %d, not by the user running this process (%d)", st.Uid, os.Getuid())
	}
	if perm := fi.Mode().Perm(); perm&0o002 != 0 {
		return fmt.Sprintf("world-writable (%#o) — anyone who can write it can replace this binary", perm)
	}
	return ""
}

// declaresModule reports whether dir/go.mod declares exactly want.
//
// A hand scan rather than golang.org/x/mod: one directive, at the top level,
// and a parser dependency for it would be the larger risk.
func declaresModule(dir, want string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	for line := range strings.Lines(string(b)) {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "module")
		if !ok || rest == strings.TrimLeft(rest, " \t") {
			continue // "module" must be followed by space — not "modulefoo"
		}
		return strings.Trim(strings.TrimSpace(rest), `"`) == want
	}
	return false
}

// rebuildSelf rebuilds the binary at exe from dir.
//
// Builds to a pid-unique temp and renames, rather than writing exe directly:
// several MCP clients can start at once (dun's harness, an editor, a shell),
// each seeing the same stale tree, and concurrent `go build -o <same path>`
// runs would interleave into a corrupt binary. Rename is atomic, so a loser of
// the race replaces a complete binary with another complete binary. It also
// dodges ETXTBSY when a copy of exe is already running.
func rebuildSelf(dir, exe string) error {
	tmp := fmt.Sprintf("%s.new.%d", exe, os.Getpid())
	build := exec.Command("go", "build", "-o", tmp, ".")
	build.Dir = dir
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		os.Remove(tmp)
		return err
	}
	if st, err := os.Stat(exe); err == nil {
		_ = os.Chmod(tmp, st.Mode().Perm())
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// sourceNewerThan reports whether any file that affects the build is newer
// than t.
//
// The prune list is deliberately SHORT. A missed directory means a missed
// rebuild — silent staleness, the exact failure this file exists to kill — so
// only trees that cannot contribute to `go build .` are skipped: build output
// (bin), separate modules and fixtures (testdata, bench), and non-Go material
// (plan, scripts, node_modules). Everything else is walked even if it is not
// actually a dependency of main; `examples/` is in the module but unreachable
// from main, so touching it triggers one needless rebuild. A wasted build is a
// cheap false positive; a skipped one is the bug.
func sourceNewerThan(dir string, t time.Time) bool {
	newer := false
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || newer {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "bin", "testdata", "bench", "plan", "scripts":
				return filepath.SkipDir
			}
			return nil
		}
		if !buildInput(d.Name()) {
			return nil
		}
		if info, e := d.Info(); e == nil && info.ModTime().After(t) {
			newer = true
		}
		return nil
	})
	return newer
}

// buildInput reports whether a filename affects the compiled binary.
//
// Go source and the module files, and nothing else — this repo has no
// `go:embed` anywhere, so no asset type belongs here. In particular NOT
// poly-lsp-mcp.yaml: the language registry is READ at runtime, so editing it
// changes behaviour without changing the binary, and treating it as a build
// input would rebuild on every config tweak for no effect.
//
// _test.go files count. They don't change the binary, but skipping them would
// let a test-only edit leave the tree "unchanged", and the mtime comparison is
// against the BINARY — so the next real edit would land in the same window and
// look like it had already been built.
func buildInput(name string) bool {
	switch name {
	case "go.mod", "go.sum":
		return true
	}
	return strings.EqualFold(filepath.Ext(name), ".go")
}
