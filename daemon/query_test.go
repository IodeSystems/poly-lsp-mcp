package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/mcp"
)

// TestQueryAnswersThroughTheRunningDaemonAndFallsBackWithTheSameMatches:
// with a daemon running for the workspace, a query answered through it
// does not rebuild the index in the CALLING process (the daemon's warm
// server serves it), and when no daemon answers the same query answers
// standalone with the SAME matches. In-process handler on a temp socket,
// no gopls, no network — the fixture is lexical, so both answers are the
// lexical matches and are directly comparable.
func TestQueryAnswersThroughTheRunningDaemonAndFallsBackWithTheSameMatches(t *testing.T) {
	// A fixture with one call edge: Caller calls Save in a.go.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module q\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package q\n\nfunc Save() {}\n\nfunc Caller() {\n\tSave()\n}\n"
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, _ := resolvePath(root)
	const selector = `#'a.go#Save'::in.call`

	// 1) Through the running daemon: the daemon's warm server builds the
	// index, not this process.
	c, stop := startTestDaemon(t, resolved)
	defer stop()

	// A server standing in for what the CLI process would build, so the
	// "zero index builds here" claim is measured against a real counter.
	cfg, _, err := config.LoadOrDefault("nonexistent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := mcp.New(reg, resolved, cfg.Bindings, cfg.Schemas)
	srv.SetQuiet(true)
	before := srv.IndexBuilds()

	out, err := c.Query(QueryParams{Root: resolved, Selector: selector})
	if err != nil {
		t.Fatalf("query through daemon: %v", err)
	}
	if got := srv.IndexBuilds(); got != before {
		t.Fatalf("answering through the daemon built the index %d time(s) in the calling process; want 0", got-before)
	}
	if !strings.Contains(out, "a.go#Caller") {
		t.Errorf("daemon answer does not name the caller:\n%s", out)
	}
	if !strings.Contains(out, "conf:") {
		t.Errorf("daemon answer does not label the edge conf:\n%s", out)
	}

	// JSON form through the daemon decodes as one document with the same
	// match.
	jsOut, err := c.Query(QueryParams{Root: resolved, Selector: selector, JSON: true})
	if err != nil {
		t.Fatalf("query (json) through daemon: %v", err)
	}
	if !strings.Contains(jsOut, `"a.go#Caller"`) {
		t.Errorf("daemon JSON answer does not name the caller:\n%s", jsOut)
	}

	// 2) Standalone (no daemon answering): the same query, answered in this
	// process, builds the index HERE — and lists the same matches.
	srv2 := mcp.New(reg, resolved, cfg.Bindings, cfg.Schemas)
	srv2.SetQuiet(true)
	var standalone bytes.Buffer
	if err := srv2.QueryText(selector, 0, 0, "", &standalone); err != nil {
		t.Fatalf("standalone query: %v", err)
	}
	if srv2.IndexBuilds() == 0 {
		t.Fatal("standalone query did not build the index in-process; the counter is dead")
	}
	if !strings.Contains(standalone.String(), "a.go#Caller") {
		t.Errorf("standalone answer does not name the caller:\n%s", standalone.String())
	}

	// The two answers are the SAME matches: the daemon's answer and the
	// standalone's agree on every match line (the call edge into Save). The
	// daemon's warm server carries a child-LSP manager, so it labels the edge
	// conf; the standalone one here has none — the conf label is the only
	// difference, so strip it before comparing the matches themselves.
	daemonLines := stripConf(matchLines(out))
	standLines := stripConf(matchLines(standalone.String()))
	if len(daemonLines) != len(standLines) {
		t.Fatalf("daemon and standalone answers differ in match count: daemon %d (%q) vs standalone %d (%q)",
			len(daemonLines), daemonLines, len(standLines), standLines)
	}
	for i := range daemonLines {
		if daemonLines[i] != standLines[i] {
			t.Fatalf("daemon and standalone answers differ at match %d:\n  daemon:    %q\n  standalone: %q", i, daemonLines[i], standLines[i])
		}
	}
}

// stripConf drops the "[conf: ...]" label from a match line so an
// LSP-resolved answer and a standalone (no-manager) answer compare as the
// same matches. Whitespace runs collapse to one space so the gap left by
// the removed label doesn't break the comparison.
func stripConf(lines []string) []string {
	for i, ln := range lines {
		s := strings.ReplaceAll(ln, "[conf: lsp]", "")
		s = strings.ReplaceAll(s, "[conf: lexical]", "")
		s = strings.ReplaceAll(s, "[conf: unsettled]", "")
		for strings.Contains(s, "\t") {
			s = strings.ReplaceAll(s, "\t", " ")
		}
		for strings.Contains(s, "  ") {
			s = strings.ReplaceAll(s, "  ", " ")
		}
		lines[i] = strings.TrimRight(s, " ")
	}
	return lines
}

// matchLines returns the lines of an answer that name a match (here: the
// call-site node line and its far-end line), stripping the caveat footer,
// so two answers for the same workspace compare as the same matches.
func matchLines(answer string) []string {
	var out []string
	for _, ln := range strings.Split(answer, "\n") {
		if strings.HasPrefix(ln, "note:") {
			break // the caveat footer is not a match
		}
		if strings.TrimSpace(ln) == "" {
			continue
		}
		out = append(out, ln)
	}
	return out
}
