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

// TestProactiveOpenPopulatesDiagnosticsResource proves the resource
// is useful immediately, before any edit happens:
//
//  1. Spawn an MCP server against a tempdir with an intentionally
//     broken Go file.
//  2. After initialize + WaitForProactiveOpen returns, read
//     poly-lsp-mcp://diagnostics.
//  3. The response contains gopls's complaint about the broken file.
//
// Without proactive open, the resource would be empty until the
// agent edited something.
func TestProactiveOpenPopulatesDiagnosticsResource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}

	dir := t.TempDir()
	// Broken on purpose: references an undefined identifier so
	// gopls publishes an error on first didOpen.
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {\n\tdoesNotExist()\n}\n"), 0o644); err != nil {
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
	srv := New(reg, dir, nil, nil)
	srv.SetLegacyTools(true) // uses the poly-lsp-mcp://diagnostics resource
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetDiagnosticWait(8 * time.Second)

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

	// Wait for the walk to finish dispatching didOpens. gopls still
	// processes asynchronously, so we also need to give it a moment
	// to publish — done below by re-reading the resource if the
	// first read shows nothing.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.WaitForProactiveOpen(ctx); err != nil {
		t.Fatalf("WaitForProactiveOpen: %v", err)
	}

	// gopls publishes asynchronously; poll the resource for up to
	// 8s waiting for the broken-file diagnostic to land.
	var payload struct {
		DiagnosticsAvailable bool `json:"diagnosticsAvailable"`
		Diagnostics          []struct {
			File     string `json:"file"`
			Severity string `json:"severity"`
			Message  string `json:"message"`
		} `json:"diagnostics"`
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		resp := sess.request("resources/read", map[string]any{
			"uri": "poly-lsp-mcp://diagnostics",
		})
		var got struct {
			Contents []resourceContent `json:"contents"`
		}
		json.Unmarshal(resp.Result, &got)
		if len(got.Contents) == 0 {
			t.Fatal("empty resources/read response")
		}
		json.Unmarshal([]byte(got.Contents[0].Text), &payload)
		if !payload.DiagnosticsAvailable {
			t.Fatalf("diagnosticsAvailable=false (no LSP for Go?); payload=%+v", payload)
		}
		var saw bool
		for _, d := range payload.Diagnostics {
			if d.Severity == "error" && filepath.Base(d.File) == "main.go" {
				saw = true
				break
			}
		}
		if saw {
			return // success
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("no error-severity diagnostic for main.go after 8s; payload=%+v", payload)
}

// TestProactiveOpenDisabled verifies the opt-out path: with
// SetProactiveOpen(false), no workspace walk runs and no didOpen is
// sent from our side. (Note: gopls still walks its packages on
// workspace load and publishes diagnostics anyway — that's gopls's
// behavior, not ours; disabling proactive open is an optimization,
// not a way to keep the diagnostic store empty.)
func TestProactiveOpenDisabled(t *testing.T) {
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
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetProactiveOpen(false)

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

	// With proactive open disabled, WaitForProactiveOpen returns
	// immediately (no channel installed) and openDocs stays empty.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := srv.WaitForProactiveOpen(ctx); err != nil {
		t.Errorf("WaitForProactiveOpen should return immediately when disabled; got %v", err)
	}
	srv.openDocsMu.Lock()
	n := len(srv.openDocs)
	srv.openDocsMu.Unlock()
	if n != 0 {
		t.Errorf("openDocs should be empty with proactive open disabled; got %d entries", n)
	}
}

// TestWokenChildGetsOpenDocsReplayed: after a lifecycle stop, the next
// RouteByURI starts a fresh gopls, which knows no documents. Without the
// OnStart replay, openDocs still says main.go is open, so the next edit
// goes out as didChange for a file the new gopls never saw — gopls drops
// it and the stale error stays. With replay, the fix clears the error.
func TestWokenChildGetsOpenDocsReplayed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped under -short")
	}
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}

	dir := t.TempDir()
	mainGo := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainGo,
		[]byte("package main\n\nfunc main() {\n\tdoesNotExist()\n}\n"), 0o644); err != nil {
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
	mgr := multiplex.NewManager(reg)
	mgr.Lifecycle = multiplex.Lifecycle{IdleStop: 500 * time.Millisecond, Interval: 100 * time.Millisecond}
	srv := New(reg, dir, nil, nil)
	srv.SetManager(mgr)

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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.WaitForProactiveOpen(ctx); err != nil {
		t.Fatal(err)
	}

	stopped := func() bool {
		for _, st := range mgr.Status() {
			if st.Language == "go" {
				return st.State == "stopped"
			}
		}
		return false
	}
	for !stopped() {
		if ctx.Err() != nil {
			t.Fatalf("go child never idle-stopped: %+v", mgr.Status())
		}
		time.Sleep(50 * time.Millisecond)
	}

	uri := pathToURI(mainGo)
	child := mgr.RouteByURI(uri) // wake; OnStart replays didOpen
	if child == nil {
		t.Fatal("wake failed")
	}
	store := mgr.Diagnostics()
	// The woken gopls reports the error again (from the replayed open).
	for hasError(store.Get(uri)) == false {
		gen := store.Gen(uri)
		if store.WaitAfter(ctx, uri, gen); ctx.Err() != nil {
			t.Fatal("woken gopls never reported the error")
		}
	}

	fixed := []byte("package main\n\nfunc main() {}\n")
	if err := os.WriteFile(mainGo, fixed, 0o644); err != nil {
		t.Fatal(err)
	}
	srv.notifyChildOfEdit(child, uri, fixed)
	for hasError(store.Get(uri)) {
		gen := store.Gen(uri)
		if store.WaitAfter(ctx, uri, gen); ctx.Err() != nil {
			t.Fatalf("edit after wake did not clear the error (didChange to an unopened doc?): %+v", store.Get(uri))
		}
	}
}

func hasError(diags []multiplex.Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == 1 {
			return true
		}
	}
	return false
}
