package mcp

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// findSourceKitLSP returns sourcekit-lsp's path: PATH first (a swift.org
// toolchain on Linux, the /usr/bin shim Xcode installs on macOS), then
// `xcrun --find` on macOS. Skips when there is none.
func findSourceKitLSP(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping live sourcekit-lsp e2e in -short")
	}
	if p, err := exec.LookPath("sourcekit-lsp"); err == nil {
		return p
	}
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("xcrun", "--find", "sourcekit-lsp").Output(); err == nil {
			p := strings.TrimSpace(string(out))
			// The default config spawns `sourcekit-lsp` off PATH. A Mac
			// where only xcrun finds it would get no child LSP, so say so
			// rather than quietly passing with the full path.
			t.Errorf("sourcekit-lsp is not on PATH but xcrun finds %s: the default config (LSP cmd \"sourcekit-lsp\") would not start it here", p)
			return p
		}
	}
	t.Skip("sourcekit-lsp not found (PATH, or xcrun on macOS)")
	return ""
}

// With edit validation on, an edit that introduces a NEW sourcekit-lsp
// error is never reported validated, and is reverted when sourcekit-lsp
// reports the error within the wait; a valid one lands. The child is
// spawned for .swift files, opened, and its diagnostics reach the edit
// path. A SwiftPM package, which sourcekit-lsp reads without a build
// server.
//
// sourcekit-lsp is not trusted for a clean result
// (trust_clean_diagnostics off by default), so no result of it validates
// an edit (validationSkipped: lsp-unconfirmed): it publishes [] for a
// broken file until its build settings load, unversioned (swift.org
// 6.4.1, macos-15-intel) or with the edit's version (macos-14). On Linux
// (swift.org 6.4.1, 2026-10-27) the broken edit is reverted; on
// macos-15-intel (Xcode) the error had not arrived 16 s after the edit
// (CI trace), and the edit is kept, not validated.
func TestSwiftEditValidateRevertsWithSourceKitLSP(t *testing.T) {
	lsp := findSourceKitLSP(t)
	// A failure then shows the publish/progress sequence the edit saw.
	t.Setenv(multiplex.TraceEnv, "1")

	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Package.swift", "// swift-tools-version:5.7\nimport PackageDescription\n\n"+
		"let package = Package(name: \"Demo\", targets: [.executableTarget(name: \"Demo\")])\n")
	orig := "func greet() -> String {\n    return \"hi\"\n}\n\nprint(greet())\n"
	write("Sources/Demo/main.swift", orig)
	mainPath := filepath.Join(dir, "Sources/Demo/main.swift")

	cfg := config.Default()
	for i := range cfg.Languages {
		if cfg.Languages[i].Name == "swift" {
			cfg.Languages[i].LSP = &config.LSP{Cmd: lsp}
		}
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetLegacyTools(true)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetValidateEdits(true)
	// sourcekit-lsp loads the package before its first complete
	// diagnostics, 2-3 s after the open: past the 2 s default.
	srv.SetDiagnosticWait(60 * time.Second)

	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	sess := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
	defer sess.close()
	sess.request("initialize", map[string]any{})
	sess.notify("notifications/initialized", map[string]any{})

	decode := func(r toolResp) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(r.Content[0].Text), &m); err != nil {
			t.Fatalf("decode edit response: %v", err)
		}
		return m
	}
	// No settle wait: the edit goes in right after bring-up, while
	// sourcekit-lsp's first publish for the file (unversioned, computed
	// before its build settings load) is still due. The edit must not take
	// that publish as its own result (waitForEditedText). On a slow
	// machine (macos-15-intel) the edit can land after it, while the
	// package still loads: sourcekit-lsp then answers the edit with [] under
	// an open work-done progress, which must not validate it either.
	lines := strings.Count(orig, "\n") + 1

	broken := "func greet() -> String {\n    return doesNotExist()\n}\n\nprint(greet())\n"
	r := sess.callTool("node_edit", map[string]any{
		"file": "Sources/Demo/main.swift", "startLine": 1, "startCol": 1, "endLine": lines, "endCol": 1,
		"newText": broken,
	})
	m := decode(r)
	if m["validated"] == true {
		t.Fatalf("an edit calling an undeclared function was reported validated; response=%+v", m)
	}
	got, _ := os.ReadFile(mainPath)
	switch {
	case m["rejected"] == true && m["reverted"] == true:
		if string(got) != orig {
			t.Fatalf("file not restored after revert:\n got: %q\nwant: %q", got, orig)
		}
	case m["validationSkipped"] == "lsp-unconfirmed" || m["validationSkipped"] == "lsp-timeout":
		// sourcekit-lsp reported no error within the wait.
		t.Logf("broken edit kept, not validated (%v): %v", m["validationSkipped"], m["validationNote"])
		if string(got) != broken {
			t.Fatalf("kept edit not on disk:\n got: %q\nwant: %q", got, broken)
		}
	default:
		t.Fatalf("broken edit neither reverted nor reported unvalidated; response=%+v", m)
	}

	good := "func greet() -> String {\n    return \"hello\"\n}\n\nprint(greet())\n"
	r = sess.callTool("node_edit", map[string]any{
		"file": "Sources/Demo/main.swift", "startLine": 1, "startCol": 1, "endLine": lines, "endCol": 1,
		"newText": good,
	})
	// Kept, not validated: unconfirmed, or timed out when sourcekit-lsp
	// is still loading at the end of the wait.
	m = decode(r)
	if m["rejected"] == true || m["validated"] == true {
		t.Fatalf("valid edit rejected, or validated though sourcekit-lsp is not trusted; response=%+v", m)
	}
	if s := m["validationSkipped"]; s != "lsp-unconfirmed" && s != "lsp-timeout" {
		t.Fatalf("valid edit: want validationSkipped lsp-unconfirmed or lsp-timeout; response=%+v", m)
	}
	t.Logf("valid edit kept, not validated (%v)", m["validationSkipped"])
	if got, _ := os.ReadFile(mainPath); string(got) != good {
		t.Fatalf("valid edit did not land:\n got: %q\nwant: %q", got, good)
	}
}

// The precision pass against a real sourcekit-lsp: two types each declare
// save(), and the caller uses exactly one. Lexical scope offers both; the
// child must narrow Run's outgoing call to Store.save, across files.
func TestSwiftPrecisionWithSourceKitLSP(t *testing.T) {
	lsp := findSourceKitLSP(t)
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Package.swift", "// swift-tools-version:5.7\nimport PackageDescription\n\n"+
		"let package = Package(name: \"Demo\", targets: [.target(name: \"Demo\")])\n")
	write("Sources/Demo/Store.swift", "struct Store {\n    func save(_ v: String) -> String { \"store:\" + v }\n}\n")
	write("Sources/Demo/Cache.swift", "struct Cache {\n    func save(_ v: String) -> String { \"cache:\" + v }\n}\n")
	write("Sources/Demo/Run.swift", "func run() -> String {\n    return Store().save(\"x\")\n}\n")

	cfg := config.Default()
	for i := range cfg.Languages {
		if cfg.Languages[i].Name == "swift" {
			cfg.Languages[i].LSP = &config.LSP{Cmd: lsp}
		}
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	s := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	waitPublished(t, srv, filepath.Join(dir, "Sources/Demo/Run.swift"))
	q := query(t, s, map[string]any{"selector": `#'Sources/Demo/Run.swift#run'::out.call`, "limit": 20})
	var saw bool
	for _, m := range q.Matches {
		joined := strings.Join(m.To, ",")
		if !strings.Contains(joined, "Store.swift#Store.save") {
			continue
		}
		saw = true
		if strings.Contains(joined, "Cache.swift#Cache.save") {
			t.Errorf("sourcekit-lsp resolves Store().save; the edge still lists Cache.save: %v (conf=%q)", m.To, m.Conf)
		}
		if m.Conf != refLSP {
			t.Errorf("conf=%q, want %q", m.Conf, refLSP)
		}
	}
	if !saw {
		t.Fatalf("no outgoing call edge to Store.save; got %+v", q.Matches)
	}
}

// waitPublished waits for the child's diagnostics for path to settle: a
// first publish, then 3 s with no other. sourcekit-lsp publishes a file
// more than once as its build settings and index arrive (measured: two
// publishes 1.2 s apart for one version), and none carries a document
// version, so one landing during an edit's wait reads as that edit's.
func waitPublished(t *testing.T, srv *Server, path string) {
	t.Helper()
	uri := "file://" + path
	store := srv.manager.Diagnostics()
	deadline := time.Now().Add(60 * time.Second)
	last, quietSince := uint64(0), time.Now()
	for {
		g := store.Gen(uri)
		if g != last {
			last, quietSince = g, time.Now()
		}
		if g > 0 && time.Since(quietSince) >= 3*time.Second {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("sourcekit-lsp diagnostics for %s did not settle within 60s (gen %d)", path, g)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
