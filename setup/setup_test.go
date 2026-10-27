package setup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
)

func TestHintsReportsFallbackDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PNPM_HOME", "")
	t.Setenv("PATH", t.TempDir())
	dir := filepath.Join(home, ".local/share/pnpm")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "typescript-language-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range Hints(reg, map[string]int{"typescript": 3}) {
		if l.LSP == nil || l.LSP.Name != "typescript-language-server" {
			continue
		}
		if !l.LSP.Found || l.LSP.Path != bin || l.LSP.Via != "fallback" {
			t.Fatalf("got %+v", *l.LSP)
		}
		return
	}
	t.Fatal("no typescript hint")
}
