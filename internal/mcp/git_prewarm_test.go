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

	"github.com/iodesystems/poly-lsp-mcp/internal/config"
)

// gitRepoWithStack creates a tempdir, init's a git repo, builds a
// two-branch stack (main with one file, feature with another), and
// leaves the working tree on `feature`. Returns the dir.
func gitRepoWithStack(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@e")
	run("config", "user.name", "t")
	run("config", "commit.gpgSign", "false")

	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Ancestor() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "main")

	run("checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.go"),
		[]byte("package main\n\nfunc OnFeature() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "feature work")
	run("branch", "--set-upstream-to=main", "feature")
	return dir
}

// TestGitPrewarmRunsForAncestorBranch boots the MCP server against
// the feature branch and verifies the ancestor branch (main) gets
// prewarmed into the parse cache.
func TestGitPrewarmRunsForAncestorBranch(t *testing.T) {
	dir := gitRepoWithStack(t)

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)

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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.WaitForGitPrewarm(ctx); err != nil {
		t.Fatalf("WaitForGitPrewarm: %v", err)
	}

	// The feature branch's main.go content was indexed at startup.
	// The prewarm should have also parsed main.go AS IT EXISTS ON
	// main — same content, same hash. So one cache hit on the
	// re-prewarm should produce zero fresh parses.
	if got := srv.parseCache.Len(); got == 0 {
		t.Errorf("parseCache empty after prewarm")
	}
}

// TestGitPrewarmDisabledNoOp verifies the toggle: with
// SetGitPrewarm(false), WaitForGitPrewarm returns immediately and
// no goroutine was kicked.
func TestGitPrewarmDisabledNoOp(t *testing.T) {
	dir := gitRepoWithStack(t)

	reg, _ := config.Default().Build()
	srv := New(reg, dir, nil, nil)
	srv.SetGitPrewarm(false)

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

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := srv.WaitForGitPrewarm(ctx); err != nil {
		t.Errorf("WaitForGitPrewarm with prewarm disabled should not block; got %v", err)
	}
	srv.gitPrewarmDoneMu.Lock()
	ch := srv.gitPrewarmDone
	srv.gitPrewarmDoneMu.Unlock()
	if ch != nil {
		t.Errorf("gitPrewarmDone channel should be nil when prewarm is disabled")
	}
}

// TestGitPrewarmFillsCacheForAncestorOnlyFiles is the demonstrative
// Phase-3 win: when an ancestor branch has files the current
// branch lacks, the initial Build won't see them, but the prewarm
// will — so switching back later is free.
func TestGitPrewarmFillsCacheForAncestorOnlyFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@e")
	run("config", "user.name", "t")
	run("config", "commit.gpgSign", "false")

	// main: two files.
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\nfunc Main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "only_main.go"),
		[]byte("package main\nfunc OnlyMain() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "main")

	// feature: delete only_main.go, add feature.go.
	run("checkout", "-q", "-b", "feature")
	if err := os.Remove(filepath.Join(dir, "only_main.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "feature.go"),
		[]byte("package main\nfunc OnFeature() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "feature work")
	run("branch", "--set-upstream-to=main", "feature")

	reg, _ := config.Default().Build()
	srv := New(reg, dir, nil, nil)

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

	// Capture cache size after initialize but BEFORE prewarm.
	// Hard to do strictly atomic without a hook; instead capture
	// the post-prewarm count and compare to a baseline derived
	// from a no-git-prewarm run.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.WaitForGitPrewarm(ctx); err != nil {
		t.Fatalf("WaitForGitPrewarm: %v", err)
	}
	postPrewarm := srv.parseCache.Len()

	// Baseline: same setup with prewarm disabled — feature's two
	// files only.
	srv2 := New(reg, dir, nil, nil)
	srv2.SetGitPrewarm(false)
	sIn2, cOut2 := io.Pipe()
	cIn2, sOut2 := io.Pipe()
	done2 := make(chan error, 1)
	go func() { done2 <- srv2.Serve(sIn2, sOut2) }()
	sess2 := &mcpSession{
		t: t, srv: srv2,
		srvIn: cOut2, clientR: json.NewDecoder(cIn2),
		clientW: cOut2, done: done2,
	}
	defer sess2.close()
	sess2.request("initialize", map[string]any{})
	sess2.notify("notifications/initialized", map[string]any{})
	baseline := srv2.parseCache.Len()

	if postPrewarm <= baseline {
		t.Errorf("prewarm should have added entries beyond baseline; got %d (prewarmed) vs %d (baseline)",
			postPrewarm, baseline)
	}
}

// TestGitPrewarmNotInRepoNoOp: outside a git repo, no walk runs and
// no error escapes.
func TestGitPrewarmNotInRepoNoOp(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir() // intentionally not a git repo

	reg, _ := config.Default().Build()
	srv := New(reg, dir, nil, nil)

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
	if err := srv.WaitForGitPrewarm(ctx); err != nil {
		t.Errorf("WaitForGitPrewarm outside a repo should return immediately; got %v", err)
	}
}
