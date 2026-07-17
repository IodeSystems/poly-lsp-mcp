package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The --root flag is this server's entire containment contract, and it was not
// being honored. resolveFileArg returned absolute paths VERBATIM, and used
// filepath.Join for relative ones — which CLEANS but does not CONFINE, so
// "../.." walked straight out. Since node_edit creates a file at whatever path
// resolves, that was an arbitrary write anywhere the process could reach.
//
// Found in a live benchmark: the model called node_edit{node:"/tmp"} on its
// first task. That one was refused only because /tmp happens to be a directory
// ("node_edit doesn't recurse into directories") — /tmp/anything.txt would have
// been created. Probed directly, it wrote outside the root and reported
// {"created":true}.
const jailSrc = "package main\n\nfunc Keep() {}\n"

// escapingPaths are paths that must be refused by every tool that takes one:
// absolute (wherever it points) and relative-but-climbing both land outside.
func escapingPaths(t *testing.T) []string {
	t.Helper()
	return []string{
		"/etc/passwd",
		"/tmp/polylsp-jail-probe.txt",
		"../escape.txt",
		"../../escape.txt",
		"sub/../../escape.txt", // climbs out only after cleaning
	}
}

func TestNodeEditRefusesToWriteOutsideRoot(t *testing.T) {
	dir := goWorkspace(t, jailSrc)
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// A real, unambiguous escape target: a FILE (not a directory, so it can't
	// be refused for the wrong reason) outside the root, that does not exist.
	outside := filepath.Join(t.TempDir(), "pwned.txt")

	r := s.callTool("node_edit", map[string]any{
		"node":    outside,
		"newText": "ESCAPED\n",
	})
	if !r.IsError {
		t.Errorf("node_edit wrote to %s, outside the root; want an error", outside)
	}
	if _, err := os.Stat(outside); err == nil {
		b, _ := os.ReadFile(outside)
		t.Fatalf("SANDBOX ESCAPE: %s was created outside the root with %q", outside, b)
	}
	if got := r.Content[0].Text; !strings.Contains(got, "escapes the workspace root") {
		t.Errorf("error should name the jail, got: %s", got)
	}
}

func TestPathToolsRefuseEscapingPaths(t *testing.T) {
	dir := goWorkspace(t, jailSrc)
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	for _, p := range escapingPaths(t) {
		// node_read must not exfiltrate; node_edit must not write.
		if r := s.callTool("node_read", map[string]any{"node": p}); !r.IsError {
			t.Errorf("node_read(%q) escaped the root; want an error", p)
		}
		if r := s.callTool("node_edit", map[string]any{"node": p, "newText": "x\n"}); !r.IsError {
			t.Errorf("node_edit(%q) escaped the root; want an error", p)
		}
	}
	// Nothing may have been created on the way out.
	if _, err := os.Stat("/tmp/polylsp-jail-probe.txt"); err == nil {
		os.Remove("/tmp/polylsp-jail-probe.txt")
		t.Fatal("SANDBOX ESCAPE: /tmp/polylsp-jail-probe.txt was created")
	}
}

// node_rename_file writes to `to` as much as it reads `from`, so an unjailed
// destination would move a workspace file OUT of the workspace.
func TestRenameFileRefusesEscapingDestination(t *testing.T) {
	dir := goWorkspace(t, jailSrc)
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.srv.SetLegacyTools(true) // node_rename_file is a legacy-surface tool
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	outside := filepath.Join(t.TempDir(), "stolen.go")
	r := s.callTool("node_rename_file", map[string]any{"from": "main.go", "to": outside})
	if !r.IsError {
		t.Errorf("node_rename_file moved a file to %s, outside the root; want an error", outside)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatalf("SANDBOX ESCAPE: %s created outside the root", outside)
	}
	// And the source must survive a refused rename.
	if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
		t.Errorf("refused rename destroyed the source file: %v", err)
	}
}

// The jail confines; it does not ban absolute paths. Clients legitimately echo
// back absolute paths this server itself emitted, so an absolute path INSIDE
// the root must keep working — that distinction is the whole reason
// resolveFileArg checks containment rather than filepath.IsAbs.
func TestAbsolutePathInsideRootStillWorks(t *testing.T) {
	dir := goWorkspace(t, jailSrc)
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	inside := filepath.Join(dir, "main.go")
	r := s.callTool("node_read", map[string]any{"node": inside})
	if r.IsError {
		t.Fatalf("absolute path inside the root must be allowed, got: %s", r.Content[0].Text)
	}
	if got := r.Content[0].Text; !strings.Contains(got, "Keep") {
		t.Errorf("read returned the wrong file: %s", got)
	}
}
