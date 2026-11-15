package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/lspbin"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// ts7Env names a node_modules dir holding typescript 7 or later and its
// platform package (@typescript/typescript-<os>-<arch>), for
// TestTS7NativeUsesThisWatcher. Unset, the global npm root is used when
// its typescript is 7 or later.
const ts7Env = "POLY_LSP_TEST_TS7"

// ts7NodeModules returns a node_modules dir with typescript >= 7, or "".
func ts7NodeModules() string {
	for _, nm := range []string{os.Getenv(ts7Env), lspbin.NpmGlobalRoot()} {
		if tsMajorAt(nm) >= 7 {
			return nm
		}
	}
	return ""
}

// tsMajorAt is the major version of nm/typescript; 0 when there is none.
func tsMajorAt(nm string) int {
	if nm == "" {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(nm, "typescript", "package.json"))
	if err != nil {
		return 0
	}
	var pkg struct {
		Version string `json:"version"`
	}
	var major int
	if json.Unmarshal(data, &pkg) != nil {
		return 0
	}
	if _, err := fmt.Sscanf(pkg.Version, "%d.", &major); err != nil {
		return 0
	}
	return major
}

// A project that resolves typescript 7 runs TS 7's native LSP. It
// publishes diagnostics, takes its file events from this server and holds
// no inotify watches of its own, and a package install (whose writes in
// node_modules nobody watches) still reaches it through the manifest
// events and the node_modules event the server adds for them.
func TestTS7NativeUsesThisWatcher(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live TypeScript 7 e2e in -short")
	}
	if runtime.GOOS != "linux" {
		t.Skip("inotify watch count: Linux only")
	}
	ts7 := ts7NodeModules()
	if ts7 == "" {
		t.Skip("no typescript >= 7 ($" + ts7Env + " or the global npm root)")
	}

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
	write("tsconfig.json", `{"compilerOptions":{"strict":true,"noEmit":true,"module":"nodenext","moduleResolution":"nodenext"},"include":["src"]}`+"\n")
	write("package.json", `{"name":"p","version":"1.0.0","type":"module","dependencies":{"other":"^1.0.0"}}`+"\n")
	write("package-lock.json", `{"name":"p","lockfileVersion":3,"packages":{"node_modules/other":{"version":"1.0.0"}}}`+"\n")
	write("node_modules/other/package.json", `{"name":"other","version":"1.0.0","types":"index.d.ts"}`+"\n")
	write("node_modules/other/index.d.ts", "export declare const o: number;\n")
	// The project's own typescript: a link to the TS 7 install.
	if err := os.Symlink(filepath.Join(ts7, "typescript"), filepath.Join(dir, "node_modules", "typescript")); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		write(fmt.Sprintf("src/m%02d/x.ts", i), fmt.Sprintf("export const x%d = %d;\n", i, i))
	}
	write("src/main.ts", "import { pad } from \"foo\";\nexport const x: string = pad(\"a\");\nexport const n: number = \"s\";\n")
	mainPath := filepath.Join(dir, "src/main.ts")
	mainURI := pathToURI(mainPath)

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetProactiveOpen(true)
	srv.SetManager(multiplex.NewManager(reg))

	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	sess := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
	defer sess.close()
	sess.request("initialize", map[string]any{})
	sess.notify("notifications/initialized", map[string]any{})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := srv.WaitForProactiveOpen(ctx); err != nil {
		t.Fatalf("proactive open: %v", err)
	}

	store := srv.manager.Diagnostics()
	hasCode := func(code float64) bool {
		for _, d := range store.Get(mainURI) {
			if c, ok := d.Code.(float64); ok && c == code {
				return true
			}
		}
		return false
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not within 60s; diagnostics %+v", what, store.Get(mainURI))
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	// TS2307: cannot find module "foo". TS2322: string to number.
	waitFor("TS2307 for the missing package", func() bool { return hasCode(2307) })
	waitFor("TS2322 for the bad assignment", func() bool { return hasCode(2322) })

	child := srv.manager.Running(mainURI)
	if child == nil {
		t.Fatal("no running typescript child")
	}
	cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", child.Pid()))
	if args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00"); len(args) != 3 || filepath.Base(args[0]) != "tsc" || args[1] != "--lsp" {
		t.Fatalf("typescript child is %q, want TS 7's tsc --lsp --stdio", args)
	}
	time.Sleep(3 * time.Second)
	wc, err := multiplex.TreeWatches(child.Pid())
	if err != nil {
		t.Fatalf("count watches: %v", err)
	}
	t.Logf("TS 7 tree inotify watches: %d (%d watchers registered)", wc.Total, child.FileWatchers())
	if wc.Total > 2 {
		t.Fatalf("TS 7 tree holds %d inotify watches (top pid %d: %d), want ~0", wc.Total, wc.TopPid, wc.TopCount)
	}
	if n := child.FileWatchers(); n == 0 {
		t.Fatal("TS 7 registered no file watchers")
	}

	// "npm install foo".
	write("node_modules/foo/package.json", `{"name":"foo","version":"1.0.0","types":"index.d.ts"}`+"\n")
	write("node_modules/foo/index.d.ts", "export declare function pad(s: string): string;\n")
	write("package.json", `{"name":"p","version":"1.0.0","type":"module","dependencies":{"other":"^1.0.0","foo":"^1.0.0"}}`+"\n")
	write("package-lock.json", `{"name":"p","lockfileVersion":3,"packages":{"node_modules/other":{"version":"1.0.0"},"node_modules/foo":{"version":"1.0.0"}}}`+"\n")
	waitFor("TS2307 cleared after the install", func() bool { return !hasCode(2307) })

	// An edit's diagnostics come from a pull after its didChange: one
	// that breaks a sibling reports the sibling's error.
	r := sess.callTool("node_edit", map[string]any{
		"node":    "src/m00/x.ts#x0",
		"oldText": "export const x0 = 0;",
		"newText": "export const x0 = \"zero\";\nexport const y0: number = x0;",
	})
	if r.IsError {
		t.Fatalf("node_edit errored: %+v", r.Content)
	}
	var payload struct {
		DiagnosticsAvailable bool `json:"diagnosticsAvailable"`
		Diagnostics          []struct {
			Message string `json:"message"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal([]byte(r.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode: %v (%s)", err, r.Content[0].Text)
	}
	found := false
	for _, d := range payload.Diagnostics {
		found = found || strings.Contains(d.Message, "not assignable to type 'number'")
	}
	if !payload.DiagnosticsAvailable || !found {
		t.Fatalf("node_edit: want the TS2322 it introduced; got %s", r.Content[0].Text)
	}
}
