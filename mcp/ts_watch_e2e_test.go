package mcp

import (
	"context"
	"encoding/json"
	"fmt"
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

// tslsClientWatchEnv names a typescript-language-server binary for
// TestTSServerUsesThisWatcher, when the one on PATH is older than 6.1.0.
// It must resolve a TypeScript of its own (installed beside it).
const tslsClientWatchEnv = "POLY_LSP_TEST_TSLS"

// tslsForClientWatch returns a typescript-language-server of 6.1.0 or
// later: $POLY_LSP_TEST_TSLS, else the one on PATH. "" when neither is.
func tslsForClientWatch(t *testing.T) string {
	t.Helper()
	cands := []string{os.Getenv(tslsClientWatchEnv)}
	if p, err := exec.LookPath("typescript-language-server"); err == nil {
		cands = append(cands, p)
	}
	for _, c := range cands {
		if c == "" {
			continue
		}
		out, err := exec.Command(c, "--version").Output()
		if err != nil {
			continue
		}
		var a, b, p int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d.%d.%d", &a, &b, &p); err != nil {
			continue
		}
		if a > 6 || (a == 6 && b >= 1) {
			return c
		}
	}
	return ""
}

// With typescript-language-server 6.1.0 or later, tsserver takes its file
// events from this server and holds no inotify watches of its own. And a
// package install, whose writes inside node_modules nobody watches, still
// reaches it: the manifest events and the node_modules event the server
// adds for them make tsserver resolve the new package.
func TestTSServerUsesThisWatcher(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live typescript-language-server e2e in -short")
	}
	if runtime.GOOS != "linux" {
		t.Skip("inotify watch count: Linux only")
	}
	tsls := tslsForClientWatch(t)
	if tsls == "" {
		t.Skip("no typescript-language-server >= 6.1.0 (on PATH or $" + tslsClientWatchEnv + ")")
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
	// node_modules exists before the install: the case where the manifest
	// events alone were measured not to be enough.
	write("node_modules/other/package.json", `{"name":"other","version":"1.0.0","types":"index.d.ts"}`+"\n")
	write("node_modules/other/index.d.ts", "export declare const o: number;\n")
	for i := range 20 {
		write(fmt.Sprintf("src/m%02d/x.ts", i), fmt.Sprintf("export const x%d = %d;\n", i, i))
	}
	write("src/main.ts", "import { pad } from \"foo\";\nexport const x: string = pad(\"a\");\n")
	mainPath := filepath.Join(dir, "src/main.ts")
	mainURI := pathToURI(mainPath)

	cfg := config.Default()
	for i := range cfg.Languages {
		if cfg.Languages[i].Name == "typescript" {
			cfg.Languages[i].LSP.Cmd = tsls
		}
	}
	reg, err := cfg.Build()
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
	// TS2307: cannot find module "foo".
	waitFor("TS2307 for the missing package", func() bool { return hasCode(2307) })

	child := srv.manager.Running(mainURI)
	if child == nil {
		t.Fatal("no running typescript child")
	}
	// The project has loaded (it published); let tsserver settle.
	time.Sleep(3 * time.Second)
	wc, err := multiplex.TreeWatches(child.Pid())
	if err != nil {
		t.Fatalf("count watches: %v", err)
	}
	t.Logf("tsserver tree inotify watches: %d (%d watchers registered)", wc.Total, child.FileWatchers())
	if wc.Total > 2 {
		t.Fatalf("typescript-language-server tree holds %d inotify watches (top pid %d: %d), want ~0", wc.Total, wc.TopPid, wc.TopCount)
	}
	if n := child.FileWatchers(); n == 0 {
		t.Fatal("typescript-language-server registered no file watchers")
	}

	// "npm install foo": node_modules is not watched, so only the
	// manifest writes reach this server.
	write("node_modules/foo/package.json", `{"name":"foo","version":"1.0.0","types":"index.d.ts"}`+"\n")
	write("node_modules/foo/index.d.ts", "export declare function pad(s: string): string;\n")
	write("package.json", `{"name":"p","version":"1.0.0","type":"module","dependencies":{"other":"^1.0.0","foo":"^1.0.0"}}`+"\n")
	write("package-lock.json", `{"name":"p","lockfileVersion":3,"packages":{"node_modules/other":{"version":"1.0.0"},"node_modules/foo":{"version":"1.0.0"}}}`+"\n")
	waitFor("TS2307 cleared after the install", func() bool { return !hasCode(2307) })
}
