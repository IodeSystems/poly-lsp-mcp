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

// Dev self-update — a source-stamped binary rebuilds itself in place when the
// tree changed, then re-execs. Ported from dun's cmd/dun/selfupdate.go for the
// same reason it exists there, which this repo demonstrated the hard way:
// poly-lsp-mcp is SPAWNED, never launched by hand (dun runs `poly-lsp-mcp mcp
// --root <ws>` off PATH; editors spawn the LSP), so nothing in the loop ever
// notices the binary is old. A dogfooding session ran for hours against a
// build that predated the fix it was hunting, and one live server was still
// executing a binary that had already been DELETED from disk by a later
// `go install`.
//
// Guards:
//
//   - srcDir is stamped ONLY by `make build|install` (-ldflags -X main.srcDir).
//     A plain `go install .` / `go install …@version` leaves it empty →
//     self-update is a no-op. That is the release build.
//   - POLY_LSP_AUTOBUILD_DONE guards the one re-exec after a rebuild (no loop).
//     The detached daemon inherits it, so `mcp --daemon` cannot ping-pong.
//   - POLY_LSP_NO_AUTOBUILD=1 disables it entirely.
//   - A build failure (a dirty tree that doesn't compile) is NON-FATAL: warn
//     and run the current binary. A code server that refuses to start because
//     the tree is mid-edit is worse than a slightly stale one.
//
// Everything it prints goes to STDERR. stdout is the MCP/LSP JSON-RPC channel
// and a single stray byte there desynchronizes the client.

// srcDir is the module directory, stamped at build time (see Makefile). Empty
// for released/plain builds → self-update disabled.
var srcDir = ""

func selfUpdate() {
	if srcDir == "" ||
		os.Getenv("POLY_LSP_AUTOBUILD_DONE") != "" ||
		os.Getenv("POLY_LSP_NO_AUTOBUILD") == "1" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	st, err := os.Stat(exe)
	if err != nil || !sourceNewerThan(srcDir, st.ModTime()) {
		return // up to date (or can't tell) — proceed normally
	}

	fmt.Fprintln(os.Stderr, "poly-lsp-mcp: source changed — rebuilding…")
	if err := rebuildSelf(srcDir, exe); err != nil {
		fmt.Fprintf(os.Stderr, "poly-lsp-mcp: rebuild failed (%v) — running the current binary\n", err)
		return
	}
	// Re-exec the freshly built binary with the same args. syscall.Exec keeps
	// the file descriptors, so an MCP client that already opened the stdio pipe
	// stays connected across the swap; the guard stops a loop.
	env := append(os.Environ(), "POLY_LSP_AUTOBUILD_DONE=1")
	if err := syscall.Exec(exe, os.Args, env); err != nil {
		fmt.Fprintf(os.Stderr, "poly-lsp-mcp: re-exec failed (%v) — running the current binary\n", err)
	}
}

// rebuildSelf rebuilds the binary at exe from srcDir, keeping the source stamp
// so the NEXT run can self-update too.
//
// Builds to a pid-unique temp and renames, rather than writing exe directly:
// several MCP clients can start at once (dun's harness, an editor, a shell),
// each seeing the same stale tree, and concurrent `go build -o <same path>`
// runs would interleave into a corrupt binary. Rename is atomic, so a loser of
// the race replaces a complete binary with another complete binary. It also
// dodges ETXTBSY when a copy of exe is already running.
func rebuildSelf(srcDir, exe string) error {
	tmp := fmt.Sprintf("%s.new.%d", exe, os.Getpid())
	build := exec.Command("go", "build", "-o", tmp, "-ldflags", "-X main.srcDir="+srcDir, ".")
	build.Dir = srcDir
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
