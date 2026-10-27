package mcp

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/jsonrpc"
	"github.com/iodesystems/poly-lsp-mcp/multiplex"
)

// fakeDiagLSPArg makes the test binary a child LSP that publishes
// diagnostics on a schedule set by $FAKE_DIAG_MODE. A document whose text
// contains BROKEN has one error. Modes:
//
//   - sourcekit: no version on any publish. A didOpen is published twice:
//     [] at +800ms (as sourcekit-lsp does before its build settings load),
//     then the real result at +1500ms. After that, a change is published
//     at +100ms. A change before that gets no publish of its own.
//   - versioned: every publish carries a version. A change is first
//     answered at once with the PREVIOUS version's (clean) result, then
//     with its own at +800ms (past the validation settle window).
//   - loading: replays sourcekit-lsp 6.4.1 loading a package under a CPU
//     quota (traced 2026-10-27). A didOpen begins a work-done progress
//     ("SourceKit-LSP: Reloading Package"), and until it ends 1500ms
//     later every publish, unversioned, is [] whatever the text: the
//     open's at +100ms and one per change at +100ms. At the end the real
//     result follows at +100ms; later changes are answered at +100ms.
//   - loading-quiet: as loading, but nothing is published when the
//     progress ends (sourcekit-lsp's "Indexing" ends right after a
//     publish, with none after it).
//   - stuck: as loading, but the progress never ends and every publish
//     shows the real result.
//   - silent: never publishes.
const fakeDiagLSPArg = "--fake-diag-lsp"

func runFakeDiagLSP() {
	mode := os.Getenv("FAKE_DIAG_MODE")
	var mu sync.Mutex
	texts := map[string]string{}
	loaded := map[string]bool{}
	write := func(m *jsonrpc.Message) {
		mu.Lock()
		defer mu.Unlock()
		_ = jsonrpc.Write(os.Stdout, m)
	}
	publish := func(uri string, version int, broken bool) {
		p := map[string]any{"uri": uri, "diagnostics": []any{}}
		if version > 0 {
			p["version"] = version
		}
		if broken {
			p["diagnostics"] = []any{map[string]any{
				"range":    map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 1}},
				"severity": 1, "message": "broken here",
			}}
		}
		raw, _ := json.Marshal(p)
		write(&jsonrpc.Message{JSONRPC: "2.0", Method: "textDocument/publishDiagnostics", Params: raw})
	}
	current := func(uri string) bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(texts[uri], "BROKEN")
	}
	progress := func(kind string) {
		raw, _ := json.Marshal(map[string]any{"token": "load", "value": map[string]any{"kind": kind, "title": "SourceKit-LSP: Reloading Package"}})
		write(&jsonrpc.Message{JSONRPC: "2.0", Method: "$/progress", Params: raw})
	}
	r := bufio.NewReader(os.Stdin)
	for {
		msg, err := jsonrpc.Read(r)
		if err != nil || msg.Method == "exit" {
			return
		}
		if msg.IsResponse() {
			continue // to window/workDoneProgress/create
		}
		var p struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Version int    `json:"version"`
				Text    string `json:"text"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		uri, version := p.TextDocument.URI, p.TextDocument.Version
		switch msg.Method {
		case "textDocument/didOpen":
			mu.Lock()
			texts[uri] = p.TextDocument.Text
			mu.Unlock()
			switch mode {
			case "sourcekit":
				go func() {
					time.Sleep(800 * time.Millisecond)
					publish(uri, 0, false)
					time.Sleep(700 * time.Millisecond)
					publish(uri, 0, current(uri))
					mu.Lock()
					loaded[uri] = true
					mu.Unlock()
				}()
			case "versioned":
				publish(uri, version, current(uri))
			case "loading", "loading-quiet", "stuck":
				write(&jsonrpc.Message{JSONRPC: "2.0", ID: json.RawMessage(`"create-load"`), Method: "window/workDoneProgress/create", Params: json.RawMessage(`{"token":"load"}`)})
				progress("begin")
				go func() {
					time.Sleep(100 * time.Millisecond)
					publish(uri, 0, mode == "stuck" && current(uri))
					if mode == "stuck" {
						return
					}
					time.Sleep(1400 * time.Millisecond)
					mu.Lock()
					loaded[uri] = true
					mu.Unlock()
					progress("end")
					if mode == "loading" {
						time.Sleep(100 * time.Millisecond)
						publish(uri, 0, current(uri))
					}
				}()
			}
			continue
		case "textDocument/didChange":
			mu.Lock()
			if len(p.ContentChanges) > 0 {
				texts[uri] = p.ContentChanges[0].Text
			}
			isLoaded := loaded[uri]
			mu.Unlock()
			switch mode {
			case "sourcekit":
				if isLoaded {
					go func() {
						time.Sleep(100 * time.Millisecond)
						publish(uri, 0, current(uri))
					}()
				}
			case "loading", "loading-quiet", "stuck":
				go func() {
					time.Sleep(100 * time.Millisecond)
					publish(uri, 0, (isLoaded || mode == "stuck") && current(uri))
				}()
			case "versioned":
				publish(uri, version-1, false)
				go func() {
					time.Sleep(800 * time.Millisecond)
					publish(uri, version, current(uri))
				}()
			}
			continue
		}
		if msg.IsNotification() {
			continue
		}
		resp := &jsonrpc.Message{JSONRPC: "2.0", ID: msg.ID}
		switch msg.Method {
		case "initialize":
			resp.Result = json.RawMessage(`{"capabilities":{"textDocumentSync":1}}`)
		case "shutdown":
			resp.Result = json.RawMessage(`null`)
		default:
			resp.Error = &jsonrpc.Error{Code: -32601, Message: "method not found: " + msg.Method}
		}
		write(resp)
	}
}

// fakeDiagSession starts a server on a one-file Go workspace whose child
// LSP is the fake above, with validation on.
func fakeDiagSession(t *testing.T, mode string, wait time.Duration) (*mcpSession, string) {
	t.Helper()
	return fakeDiagSessionWith(t, mode, wait, "package main\n\nfunc main() {}\n")
}

// fakeDiagSessionWith is fakeDiagSession with main.go holding text.
func fakeDiagSessionWith(t *testing.T, mode string, wait time.Duration, text string) (*mcpSession, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	for i := range cfg.Languages {
		if cfg.Languages[i].Name == "go" {
			cfg.Languages[i].LSP = &config.LSP{Cmd: exe, Args: []string{fakeDiagLSPArg}, Env: []string{"FAKE_DIAG_MODE=" + mode}}
		}
	}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, dir, nil, nil)
	srv.SetManager(multiplex.NewManager(reg))
	srv.SetValidateEdits(true)
	srv.SetDiagnosticWait(wait)
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

func editMain(t *testing.T, s *mcpSession, path, text string) map[string]any {
	t.Helper()
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r := s.callTool("node_edit", map[string]any{"node": "main.go", "oldText": string(cur), "newText": text})
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Content[0].Text), &m); err != nil {
		t.Fatalf("decode: %v (%s)", err, r.Content[0].Text)
	}
	return m
}

const brokenMain = "package main\n\n// BROKEN\nfunc main() {}\n"

// An edit right after bring-up must not take the child's first,
// incomplete publish for the opened file as its own result. The fake
// publishes [] 800ms after the open and the real (broken) result at
// 1500ms, with no version on either: the broken edit is reverted.
func TestEditSkipsFirstPublishAfterOpen(t *testing.T) {
	s, path := fakeDiagSession(t, "sourcekit", 5*time.Second)
	orig, _ := os.ReadFile(path)
	m := editMain(t, s, path, brokenMain)
	if m["rejected"] != true || m["reverted"] != true {
		t.Fatalf("a broken edit right after open was not reverted: %v", m)
	}
	if got, _ := os.ReadFile(path); string(got) != string(orig) {
		t.Fatalf("file not restored: %q", got)
	}
	// Loaded now: a clean edit is answered at +100ms and lands.
	good := "package main\n\nfunc main() { println(1) }\n"
	if m := editMain(t, s, path, good); m["validated"] != true {
		t.Fatalf("clean edit after load not validated: %v", m)
	}
}

// A child that sends a stale publish for the previous version before the
// edited one: the version tells them apart, and the broken edit reverts.
func TestEditWaitsForEditedVersion(t *testing.T) {
	s, path := fakeDiagSession(t, "versioned", 5*time.Second)
	// Let the open's publish land so the edit is a didChange.
	time.Sleep(300 * time.Millisecond)
	orig, _ := os.ReadFile(path)
	m := editMain(t, s, path, brokenMain)
	if m["rejected"] != true || m["reverted"] != true {
		t.Fatalf("broken edit kept on a stale publish for the previous version: %v", m)
	}
	if got, _ := os.ReadFile(path); string(got) != string(orig) {
		t.Fatalf("file not restored: %q", got)
	}
}

// A child that never publishes: the edit waits the cap, is kept, and says
// it was not validated rather than passing on old diagnostics.
func TestEditValidationTimesOut(t *testing.T) {
	s, path := fakeDiagSession(t, "silent", 400*time.Millisecond)
	start := time.Now()
	m := editMain(t, s, path, brokenMain)
	if el := time.Since(start); el < 400*time.Millisecond {
		t.Errorf("edit returned in %s, before the 400ms wait", el)
	}
	if m["validated"] != false || m["validationSkipped"] != "lsp-timeout" || m["diagnosticsTimedOut"] != true {
		t.Fatalf("want validated:false validationSkipped:lsp-timeout diagnosticsTimedOut:true, got %v", m)
	}
	if note, _ := m["validationNote"].(string); !strings.Contains(note, "timed out after 400ms") || !strings.Contains(note, "not validated") {
		t.Errorf("validationNote = %q", note)
	}
	if got, _ := os.ReadFile(path); string(got) != brokenMain {
		t.Fatalf("timed-out edit not kept: %q", got)
	}
}

// A daemon session's --validate-wait (CallOptions.ValidateWait) governs
// that session's edits on the shared server, not the server's own.
func TestCallToolValidateWaitIsPerSession(t *testing.T) {
	s, path := fakeDiagSession(t, "silent", 30*time.Second)
	cur, _ := os.ReadFile(path)
	args, _ := json.Marshal(map[string]any{"node": "main.go", "oldText": string(cur), "newText": brokenMain})
	start := time.Now()
	content, _, err := s.srv.CallTool("sess-a", "node_edit", args, CallOptions{ValidateWait: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("edit took %s: the session's 300ms wait was not used", el)
	}
	if !strings.Contains(content[0].Text, "timed out after 300ms") {
		t.Fatalf("want a 300ms timeout note, got %s", content[0].Text)
	}
}

// waitFirstPublish blocks until the child has published path once.
func waitFirstPublish(t *testing.T, s *mcpSession, path string) {
	t.Helper()
	uri := pathToURI(path)
	deadline := time.Now().Add(5 * time.Second)
	for s.srv.manager.Diagnostics().Gen(uri) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no publish for the opened file within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The macos-15-intel failure of the v0.3.0 release: an edit made after a
// loading sourcekit-lsp's first publish, while its package still loads.
// The child answers the edit with [] (no semantic errors yet) under an
// open work-done progress; that must not validate it. The real result
// after the progress ends reverts it.
func TestEditDistrustsCleanPublishWhileChildLoads(t *testing.T) {
	s, path := fakeDiagSession(t, "loading", 5*time.Second)
	waitFirstPublish(t, s, path)
	orig, _ := os.ReadFile(path)
	m := editMain(t, s, path, brokenMain)
	if m["rejected"] != true || m["reverted"] != true {
		t.Fatalf("a broken edit was validated on a publish made while the child was loading: %v", m)
	}
	if got, _ := os.ReadFile(path); string(got) != string(orig) {
		t.Fatalf("file not restored: %q", got)
	}
	// Loaded now: a clean edit is answered at +100ms and validated.
	good := "package main\n\nfunc main() { println(1) }\n"
	if m := editMain(t, s, path, good); m["validated"] != true {
		t.Fatalf("clean edit after load not validated: %v", m)
	}
}

// A child whose progress never ends. Its clean result is never trusted:
// the edit times out, kept and not validated. A result with errors is
// taken, but one with no NEW errors (the file was already broken) does
// not validate the edit either.
func TestEditNotValidatedWhileChildBusy(t *testing.T) {
	s, path := fakeDiagSessionWith(t, "stuck", 600*time.Millisecond, brokenMain)
	waitFirstPublish(t, s, path)
	stillBroken := "package main\n\n// BROKEN\nfunc main() { println(1) }\n"
	m := editMain(t, s, path, stillBroken)
	if m["validated"] != false || m["rejected"] == true || m["validationSkipped"] != "lsp-timeout" {
		t.Fatalf("an edit with no new errors from a busy child: want validated:false validationSkipped:lsp-timeout, got %v", m)
	}
	if note, _ := m["validationNote"].(string); !strings.Contains(note, "still loading") {
		t.Errorf("validationNote = %q", note)
	}
	clean := "package main\n\nfunc main() {}\n"
	m = editMain(t, s, path, clean)
	if m["validated"] != false || m["validationSkipped"] != "lsp-timeout" || m["diagnosticsTimedOut"] != true {
		t.Fatalf("a clean result from a busy child was trusted: %v", m)
	}
	if got, _ := os.ReadFile(path); string(got) != clean {
		t.Fatalf("timed-out edit not kept: %q", got)
	}
}

// A child that, like sourcekit-lsp ending "Indexing", publishes nothing
// when its progress ends: the edit's clean publish while it loaded is not
// trusted, so the text is sent again once it is idle, and the clean
// answer to that validates the edit.
func TestEditResendsTextWhenChildGoesIdle(t *testing.T) {
	s, path := fakeDiagSession(t, "loading-quiet", 5*time.Second)
	waitFirstPublish(t, s, path)
	good := "package main\n\nfunc main() { println(1) }\n"
	if m := editMain(t, s, path, good); m["validated"] != true {
		t.Fatalf("clean edit made while the child loaded not validated after it went idle: %v", m)
	}
}
