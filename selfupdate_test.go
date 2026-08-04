package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeAt creates a file with a chosen mtime, so staleness can be tested
// without sleeping.
func writeAt(t *testing.T, dir, rel string, mod time.Time) string {
	t.Helper()
	abs := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(abs, mod, mod); err != nil {
		t.Fatal(err)
	}
	return abs
}

// Only what the compiler reads counts. poly-lsp-mcp.yaml is the language
// registry, READ at runtime — treating it as a build input would rebuild on
// every config tweak and change nothing about the binary. Nothing in this
// repo uses go:embed, so no asset type belongs here either.
func TestBuildInputIsGoSourceAndModuleFilesOnly(t *testing.T) {
	for _, name := range []string{"main.go", "server_test.go", "A.GO", "go.mod", "go.sum"} {
		if !buildInput(name) {
			t.Errorf("%q should be a build input", name)
		}
	}
	for _, name := range []string{
		"poly-lsp-mcp.yaml", "config.yml", "queries.scm",
		"README.md", "Makefile", "poly-lsp-mcp", "notes.txt",
	} {
		if buildInput(name) {
			t.Errorf("%q should NOT be a build input", name)
		}
	}
}

// The whole point: a source edit after the binary was built is detected.
func TestSourceNewerThanSpotsAnEditInAnyPackage(t *testing.T) {
	dir := t.TempDir()
	built := time.Now()
	old, fresh := built.Add(-time.Hour), built.Add(time.Hour)

	writeAt(t, dir, "main.go", old)
	writeAt(t, dir, "go.mod", old)
	writeAt(t, dir, "mcp/modern.go", old)
	if sourceNewerThan(dir, built) {
		t.Fatal("an all-old tree is not newer than the binary")
	}

	// A nested package counts — the walk must not stop at the module root.
	writeAt(t, dir, "symbols/filesymbols.go", fresh)
	if !sourceNewerThan(dir, built) {
		t.Error("an edit under symbols/ should mark the tree newer")
	}
}

// A missed directory means a missed rebuild — silent staleness, the exact
// failure this file exists to kill. So the prune list stays short, and these
// two halves are pinned against each other: pruned trees cannot trigger, every
// real dependency can.
func TestSourceNewerThanPruneList(t *testing.T) {
	built := time.Now()
	fresh := built.Add(time.Hour)

	// Build output, separate modules, fixtures and non-Go material: a change
	// here cannot affect `go build .`.
	for _, rel := range []string{
		"bin/poly-lsp-mcp.go",
		"testdata/fixtures/polyglot/main.go",
		"bench/probes/p/_fixture/tools.go",
		"plan/notes.go",
		"scripts/helper.go",
		".git/hooks/x.go",
		"node_modules/pkg/index.go",
	} {
		dir := t.TempDir()
		writeAt(t, dir, "main.go", built.Add(-time.Hour))
		writeAt(t, dir, rel, fresh)
		if sourceNewerThan(dir, built) {
			t.Errorf("%s is pruned and must not trigger a rebuild", rel)
		}
	}

	// Every package main actually depends on, plus examples/ (in the module
	// but unreachable from main — a wasted build is a cheap false positive,
	// a skipped one is the bug).
	for _, rel := range []string{
		"config/c.go", "daemon/d.go", "internal/git/g.go", "mcp/m.go",
		"multiplex/x.go", "server/s.go", "symbols/y.go", "examples/embed/e.go",
	} {
		dir := t.TempDir()
		writeAt(t, dir, "main.go", built.Add(-time.Hour))
		writeAt(t, dir, rel, fresh)
		if !sourceNewerThan(dir, built) {
			t.Errorf("%s is a build input and must trigger a rebuild", rel)
		}
	}
}

// An unstamped binary never self-updates — that is what makes `go install .`
// a release build. selfUpdate must return before it can stat, build or exec.
func TestSelfUpdateIsANoOpWithoutASourceStamp(t *testing.T) {
	saved := srcDir
	t.Cleanup(func() { srcDir = saved })

	srcDir = ""
	selfUpdate() // must not panic, build, or replace this test process

	// The env guards are the other half: with a stamp but a guard set, still
	// nothing. Pointing srcDir at an empty temp dir makes a stray rebuild
	// attempt fail loudly rather than silently succeeding against the repo.
	srcDir = t.TempDir()
	t.Setenv("POLY_LSP_NO_AUTOBUILD", "1")
	selfUpdate()
	t.Setenv("POLY_LSP_NO_AUTOBUILD", "")
	t.Setenv("POLY_LSP_AUTOBUILD_DONE", "1")
	selfUpdate()
}

// rebuildSelf must never leave a half-written binary at the destination: it
// builds to a pid-unique temp and renames. A failing build leaves the old
// binary untouched and no debris.
func TestRebuildSelfLeavesTheBinaryIntactOnFailure(t *testing.T) {
	src := t.TempDir()
	// A module that does not compile.
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module broken\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\nfunc main() { this is not go }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(t.TempDir(), "poly-lsp-mcp")
	if err := os.WriteFile(exe, []byte("OLD BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := rebuildSelf(src, exe); err == nil {
		t.Fatal("a broken tree should fail the rebuild")
	}
	got, err := os.ReadFile(exe)
	if err != nil || string(got) != "OLD BINARY" {
		t.Errorf("the existing binary must survive a failed rebuild; got %q err=%v", got, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(exe))
	if len(entries) != 1 {
		t.Errorf("a failed rebuild must not leave temp debris; dir has %d entries", len(entries))
	}
}
