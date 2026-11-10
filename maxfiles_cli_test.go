package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/daemon"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// `index` over --max-files is refused with the remedy; --index-anyway does
// not lift it, --max-files 0 does.
func TestIndexMaxFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(daemon.HomeEnv, t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir := t.TempDir()
	for i := range 12 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.go", i)), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) error {
		root := newRoot()
		root.SetArgs(args)
		stdout := os.Stdout
		os.Stdout, _ = os.Open(os.DevNull)
		defer func() { os.Stdout = stdout }()
		return root.Execute()
	}
	for _, args := range [][]string{
		{"index", "--root", dir, "--max-files", "10"},
		{"index", "--root", dir, "--max-files", "10", "--index-anyway"},
	} {
		var tm *symbols.TooManyFilesError
		if err := run(args...); !errors.As(err, &tm) || !strings.Contains(tm.Detail(), "--max-files") {
			t.Errorf("%v: want TooManyFilesError, got %v", args, err)
		}
	}
	if err := run("index", "--root", dir, "--max-files", "0"); err != nil {
		t.Errorf("--max-files 0: %v", err)
	}
}
