package mcp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// TestProactiveOpenDefaultsOff: a Server nobody configured sends no
// didOpen at connect. Opt-in is --proactive-open / SetProactiveOpen(true).
func TestProactiveOpenDefaultsOff(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil) // no SetProactiveOpen call
	if srv.proactiveOpen {
		t.Fatal("proactiveOpen must default to false")
	}
	srv.SetManager(multiplex.NewManager(reg))

	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	sess := &mcpSession{
		t: t, srv: srv,
		srvIn: cOut, clientR: json.NewDecoder(cIn),
		clientW: cOut, done: done,
	}
	defer sess.close()
	sess.request("initialize", map[string]any{})
	sess.notify("notifications/initialized", map[string]any{})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.WaitForProactiveOpen(ctx); err != nil {
		t.Fatalf("no walk should be pending: %v", err)
	}
	srv.openDocsMu.Lock()
	n := len(srv.openDocs)
	srv.openDocsMu.Unlock()
	if n != 0 {
		t.Errorf("default sent didOpen for %d file(s) at connect", n)
	}
}
