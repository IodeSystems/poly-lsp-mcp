package mcp

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// lspOffSession is fakeDiagSessionTrust (versioned, trusted) with the
// client's switch prefs set before the bring-up.
func lspOffSession(t *testing.T, prefs LSPPrefs, offByDefault bool) (*mcpSession, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	trust := true
	cfg := config.Default()
	for i := range cfg.Languages {
		if cfg.Languages[i].Name == "go" {
			cfg.Languages[i].LSP = &config.LSP{Cmd: exe, Args: []string{fakeDiagLSPArg}, Env: []string{"FAKE_DIAG_MODE=versioned"}, TrustCleanDiagnostics: &trust}
			cfg.Languages[i].OffByDefault = offByDefault
		}
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetValidateEdits(true)
	srv.SetDiagnosticWait(5 * time.Second)
	srv.SetLSPPrefs(prefs)
	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	s := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
	t.Cleanup(s.close)
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	return s, path
}

// A language off for the client: its child is not started, an edit the
// child would reject (BROKEN) lands with validationSkipped: lsp-off, and
// an edit that does not parse is reverted by the parse check.
func TestLSPOffEditIsParseCheckedOnly(t *testing.T) {
	s, path := lspOffSession(t, LSPPrefs{Off: []string{"go"}}, false)
	m := editMain(t, s, path, brokenMain)
	if m["rejected"] == true || m["validated"] != false || m["validationSkipped"] != "lsp-off" || m["diagnosticsAvailable"] != false {
		t.Fatalf("want kept, validationSkipped lsp-off, no diagnostics: %v", m)
	}
	if note, _ := m["validationNote"].(string); !strings.Contains(note, "parse") {
		t.Errorf("validationNote = %q", note)
	}
	if st := s.srv.ChildStates()["go"]; st == "running" {
		t.Errorf("the child of a language every client turned off was started: %s", st)
	}
	before, _ := os.ReadFile(path)
	m = editMain(t, s, path, "package main\n\nfunc main() {\n")
	if m["rejected"] != true || m["reverted"] != true || !strings.Contains(m["note"].(string), "REJECTED") {
		t.Fatalf("a parse-breaking edit was not reverted: %v", m)
	}
	if got, _ := os.ReadFile(path); string(got) != string(before) {
		t.Fatalf("file not restored: %q", got)
	}
}

// off_by_default leaves the child off unless the client turns it on; on,
// the child validates as usual.
func TestLSPOnTurnsOnAnOffByDefaultLanguage(t *testing.T) {
	off, offPath := lspOffSession(t, LSPPrefs{}, true)
	if m := editMain(t, off, offPath, brokenMain); m["validationSkipped"] != "lsp-off" {
		t.Fatalf("off by default, no lsp_on: %v", m)
	}
	on, onPath := lspOffSession(t, LSPPrefs{On: []string{"go"}}, true)
	time.Sleep(300 * time.Millisecond) // the open's publish lands
	if m := editMain(t, on, onPath, brokenMain); m["rejected"] != true {
		t.Fatalf("lsp_on go: the child's error did not revert the edit: %v", m)
	}
	if st := on.srv.ChildStates()["go"]; st != "running" {
		t.Errorf("lsp_on go: child state %q", st)
	}
}

// Two sessions on one server (a daemon root): the child is shared, the
// switch filters per session, and the child starts when the session that
// wants it first needs it.
func TestLSPOffIsPerSession(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	trust := true
	cfg := config.Default()
	for i := range cfg.Languages {
		if cfg.Languages[i].Name == "go" {
			cfg.Languages[i].LSP = &config.LSP{Cmd: exe, Args: []string{fakeDiagLSPArg}, Env: []string{"FAKE_DIAG_MODE=versioned"}, TrustCleanDiagnostics: &trust}
		}
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetValidateEdits(true)
	srv.SetDiagnosticWait(5 * time.Second)
	srv.SetQuiet(true)
	srv.SetSessionLSP("off", LSPPrefs{Off: []string{"go"}})
	if err := srv.BringUp(true); err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	if st := srv.ChildStates()["go"]; st == "running" {
		t.Fatalf("only an lsp_off client, child state %q", st)
	}
	edit := func(sess, text string) map[string]any {
		t.Helper()
		cur, _ := os.ReadFile(path)
		args, _ := json.Marshal(map[string]any{"node": "main.go", "oldText": string(cur), "newText": text})
		content, _, err := srv.CallTool(sess, "node_edit", args, CallOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(content[0].Text), &m); err != nil {
			t.Fatalf("decode: %v (%s)", err, content[0].Text)
		}
		return m
	}
	if m := edit("off", brokenMain); m["validationSkipped"] != "lsp-off" {
		t.Fatalf("off session: %v", m)
	}
	if st := srv.ChildStates()["go"]; st == "running" {
		t.Fatalf("an lsp_off session's edit started the child")
	}
	// A second client that wants go: its edit starts the child and is
	// validated by it.
	srv.SetSessionLSP("on", LSPPrefs{})
	if m := edit("on", "package main\n\nfunc main() {}\n"); m["validated"] != true {
		t.Fatalf("on session's clean edit: %v", m)
	}
	if st := srv.ChildStates()["go"]; st != "running" {
		t.Fatalf("on session: child state %q", st)
	}
	time.Sleep(time.Second) // the fake's late publish for that version
	if m := edit("on", brokenMain); m["rejected"] != true {
		t.Fatalf("on session's broken edit not reverted: %v", m)
	}
	time.Sleep(time.Second)
	// The child is running now; the off session still gets none of it.
	if m := edit("off", brokenMain); m["validationSkipped"] != "lsp-off" || m["rejected"] == true {
		t.Fatalf("off session with the child running: %v", m)
	}
}

// node_query for a client with the language off: the edge is not settled
// through the child (which would pick store.Save); both candidates stay.
func TestLSPOffQueryStaysLexical(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module collide\ngo 1.21\n")
	write("store/store.go", "package store\n\nfunc Save(v string) string {\n\treturn \"store:\" + v\n}\n")
	write("cache/cache.go", "package cache\n\nfunc Save(v string) string {\n\treturn \"cache:\" + v\n}\n")
	write("main.go", "package main\n\nimport \"collide/store\"\n\nfunc Run() string {\n\treturn store.Save(\"x\")\n}\n")
	for _, off := range []bool{false, true} {
		cfg := config.Default()
		for i := range cfg.Languages {
			if cfg.Languages[i].Name == "go" {
				cfg.Languages[i].LSP = &config.LSP{Cmd: exe, Args: []string{fakeDefLSPArg}, Env: []string{
					"FAKE_DEF_URI=file://" + filepath.Join(root, "store/store.go"), "FAKE_DEF_LINE=2",
				}}
			}
		}
		reg, err := cfg.Build()
		if err != nil {
			t.Fatal(err)
		}
		srv := New(reg, root, nil, nil)
		srv.SetManager(multiplex.NewManager(reg))
		if off {
			srv.SetLSPPrefs(LSPPrefs{Off: []string{"go"}})
		}
		sIn, cOut := io.Pipe()
		cIn, sOut := io.Pipe()
		done := make(chan error, 1)
		go func() { done <- srv.Serve(sIn, sOut) }()
		s := &mcpSession{t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn), clientW: cOut, done: done}
		s.request("initialize", map[string]any{})
		s.notify("notifications/initialized", map[string]any{})
		q := query(t, s, map[string]any{"selector": `#'main.go#Run'::out.call`, "limit": 20})
		s.close()
		var conf string
		for _, m := range q.Matches {
			if strings.Contains(strings.Join(m.To, ","), "store/store.go#Save") {
				conf = m.Conf
			}
		}
		switch {
		case !off && conf != refLSP:
			t.Errorf("lsp on: conf=%q, want %q (%+v)", conf, refLSP, q.Matches)
		case off && (conf == refLSP || conf == ""):
			t.Errorf("lsp off: conf=%q, want lexical/unsettled (%+v)", conf, q.Matches)
		}
	}
}
