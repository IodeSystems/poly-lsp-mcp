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
//   - blind: replays Xcode's sourcekit-lsp on macos-15-intel (CI trace,
//     2026-10-27): as loading-quiet, but every publish is [] whatever the
//     text, after the progress ends too. Its errors never arrive.
//   - xcode14: replays Xcode's sourcekit-lsp on macos-14 (CI trace of
//     9b7385f, 2026-10-27). Publishes carry a version, but one does not
//     prove the text was analysed. Nothing is published for 1500ms after
//     the open (the package loads); then the open's version [], and, if a
//     change came meanwhile, its version [] and 20ms later its version
//     with the real result. A later change is answered after 200ms
//     without another change: twice with the result last published
//     (stale) and the latest change's version, then with that version and
//     its real result. The CI trace cut those two publishes off before
//     any version (multiplex trace limit): with the version is the reading
//     that reproduces the failure (the stale error reported for a valid
//     edit validated by the next publish).
//   - xcode14-unversioned: as xcode14, the two stale publishes without a
//     version.
//   - silent: never publishes.
//   - debounce: replays gopls coalescing changes (versioned). The open
//     is published at once; a change 300ms later, unless another change
//     came meanwhile: then only the later one is published.
//   - skip-open-text: versioned, each publish at once, except a change
//     back to the open's text, which gets none (a revert the server never
//     answers).
//
// In debounce and skip-open-text a document has one error per BROKEN.
const fakeDiagLSPArg = "--fake-diag-lsp"

func runFakeDiagLSP() {
	mode := os.Getenv("FAKE_DIAG_MODE")
	var mu sync.Mutex
	texts := map[string]string{}
	loaded := map[string]bool{}
	lastBroken := map[string]bool{} // xcode14: the result last published
	lastChange := map[string]int{}  // xcode14: the latest change's version
	changeSeq := map[string]int{}   // xcode14: debounces changes
	write := func(m *jsonrpc.Message) {
		mu.Lock()
		defer mu.Unlock()
		_ = jsonrpc.Write(os.Stdout, m)
	}
	openText := map[string]string{} // skip-open-text
	publishN := func(uri string, version, errors int) {
		p := map[string]any{"uri": uri, "diagnostics": []any{}}
		if version > 0 {
			p["version"] = version
		}
		diags := []any{}
		for i := 0; i < errors; i++ {
			diags = append(diags, map[string]any{
				"range":    map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 1}},
				"severity": 1, "message": "broken here",
			})
		}
		p["diagnostics"] = diags
		raw, _ := json.Marshal(p)
		mu.Lock()
		lastBroken[uri] = errors > 0
		mu.Unlock()
		write(&jsonrpc.Message{JSONRPC: "2.0", Method: "textDocument/publishDiagnostics", Params: raw})
	}
	publish := func(uri string, version int, broken bool) {
		n := 0
		if broken {
			n = 1
		}
		publishN(uri, version, n)
	}
	count := func(uri string) int {
		mu.Lock()
		defer mu.Unlock()
		return strings.Count(texts[uri], "BROKEN")
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
			case "debounce", "skip-open-text":
				mu.Lock()
				openText[uri] = p.TextDocument.Text
				mu.Unlock()
				publishN(uri, version, count(uri))
			case "xcode14", "xcode14-unversioned":
				go func() {
					time.Sleep(1500 * time.Millisecond)
					mu.Lock()
					loaded[uri] = true
					changed := lastChange[uri]
					mu.Unlock()
					publish(uri, version, false)
					if changed > version {
						publish(uri, changed, false)
						time.Sleep(20 * time.Millisecond)
						publish(uri, changed, current(uri))
					}
				}()
			case "loading", "loading-quiet", "stuck", "blind":
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
			lastChange[uri] = version
			changeSeq[uri]++
			seq := changeSeq[uri]
			mu.Unlock()
			switch mode {
			case "xcode14", "xcode14-unversioned":
				if isLoaded {
					go func() {
						time.Sleep(200 * time.Millisecond)
						mu.Lock()
						latest, stale := changeSeq[uri] == seq, lastBroken[uri]
						mu.Unlock()
						if !latest {
							return // a later change answers
						}
						staleVersion := version
						if mode == "xcode14-unversioned" {
							staleVersion = 0
						}
						publish(uri, staleVersion, stale)
						time.Sleep(5 * time.Millisecond)
						publish(uri, staleVersion, stale)
						time.Sleep(5 * time.Millisecond)
						publish(uri, version, current(uri))
					}()
				}
			case "sourcekit":
				if isLoaded {
					go func() {
						time.Sleep(100 * time.Millisecond)
						publish(uri, 0, current(uri))
					}()
				}
			case "loading", "loading-quiet", "stuck", "blind":
				go func() {
					time.Sleep(100 * time.Millisecond)
					publish(uri, 0, (isLoaded || mode == "stuck") && mode != "blind" && current(uri))
				}()
			case "versioned":
				publish(uri, version-1, false)
				go func() {
					time.Sleep(800 * time.Millisecond)
					publish(uri, version, current(uri))
				}()
			case "debounce":
				go func() {
					time.Sleep(300 * time.Millisecond)
					mu.Lock()
					latest := changeSeq[uri] == seq
					mu.Unlock()
					if latest {
						publishN(uri, version, count(uri))
					}
				}()
			case "skip-open-text":
				mu.Lock()
				reverted := texts[uri] == openText[uri]
				mu.Unlock()
				if !reverted {
					publishN(uri, version, count(uri))
				}
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
	return fakeDiagSessionTrust(t, mode, wait, text, false)
}

// fakeDiagSessionTrust is fakeDiagSessionWith with the child's
// trust_clean_diagnostics set to trust (unset, the fake is not trusted:
// its command is not a server trusted by default).
func fakeDiagSessionTrust(t *testing.T, mode string, wait time.Duration, text string, trust bool) (*mcpSession, string) {
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
			cfg.Languages[i].LSP = &config.LSP{Cmd: exe, Args: []string{fakeDiagLSPArg}, Env: []string{"FAKE_DIAG_MODE=" + mode}, TrustCleanDiagnostics: &trust}
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
// 1500ms, with no version on either: the broken edit is reverted. A
// clean result with no version never validates an edit.
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
	// Loaded now: a clean edit is answered at +100ms and lands, not
	// validated.
	good := "package main\n\nfunc main() { println(1) }\n"
	wantUnconfirmed(t, editMain(t, s, path, good))
	if got, _ := os.ReadFile(path); string(got) != good {
		t.Fatalf("clean edit not kept: %q", got)
	}
}

// wantUnconfirmed checks an edit answered by a clean result from a child
// not trusted for one: kept, not validated, validationSkipped:
// lsp-unconfirmed.
func wantUnconfirmed(t *testing.T, m map[string]any) {
	t.Helper()
	if m["validated"] != false || m["rejected"] == true || m["validationSkipped"] != "lsp-unconfirmed" || m["diagnosticsTimedOut"] == true {
		t.Fatalf("want validated:false validationSkipped:lsp-unconfirmed (not timed out), got %v", m)
	}
	if note, _ := m["validationNote"].(string); !strings.Contains(note, "trust_clean_diagnostics") || !strings.Contains(note, "not validated") {
		t.Errorf("validationNote = %q", note)
	}
}

// A child that sends a stale publish for the previous version before the
// edited one: the version tells them apart, and the broken edit reverts.
// Trusted (as gopls is), a clean result for the edited version validates.
func TestEditWaitsForEditedVersion(t *testing.T) {
	s, path := fakeDiagSessionTrust(t, "versioned", 5*time.Second, "package main\n\nfunc main() {}\n", true)
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
	// A clean result for the edited version validates. Let the revert's
	// own result (+800ms) land first: the fake would publish it after the
	// next edit's.
	time.Sleep(time.Second)
	good := "package main\n\nfunc main() { println(1) }\n"
	if m := editMain(t, s, path, good); m["validated"] != true {
		t.Fatalf("clean edit with a versioned result not validated: %v", m)
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
	// Loaded now: a clean edit is answered at +100ms, not validated.
	good := "package main\n\nfunc main() { println(1) }\n"
	wantUnconfirmed(t, editMain(t, s, path, good))
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
	wantUnconfirmed(t, m)
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
// taken, so the text is sent again once it is idle, and the answer to
// that is the edit's result (without the resend the edit times out).
func TestEditResendsTextWhenChildGoesIdle(t *testing.T) {
	s, path := fakeDiagSession(t, "loading-quiet", 5*time.Second)
	waitFirstPublish(t, s, path)
	good := "package main\n\nfunc main() { println(1) }\n"
	wantUnconfirmed(t, editMain(t, s, path, good))
}

// The macos-15-intel failure of 87d9429: Xcode's sourcekit-lsp answered a
// broken edit with [] while its package loaded, sent nothing when the
// load ended, and answered the resent text with [] again. No error ever
// arrived. The edit is kept and must not be reported validated.
func TestEditNotValidatedOnBlindChild(t *testing.T) {
	s, path := fakeDiagSession(t, "blind", 5*time.Second)
	waitFirstPublish(t, s, path)
	m := editMain(t, s, path, brokenMain)
	wantUnconfirmed(t, m)
	if got, _ := os.ReadFile(path); string(got) != brokenMain {
		t.Fatalf("edit not kept: %q", got)
	}
}

// A batch (commit:false, then commit) whose staged file was answered only
// by an unversioned clean publish commits, kept and not validated: the
// union's check is no stronger than the evidence for its files.
func TestBatchCommitNotValidatedOnUnconfirmedResult(t *testing.T) {
	s, path := fakeDiagSession(t, "blind", 5*time.Second)
	waitFirstPublish(t, s, path)
	cur, _ := os.ReadFile(path)
	good := "package main\n\nfunc main() { println(1) }\n"
	r := s.callTool("node_edit", map[string]any{"node": "main.go", "oldText": string(cur), "newText": good, "commit": false})
	if m := txnDecode(t, r); m["staged"] != true {
		t.Fatalf("stage: %v", m)
	}
	m := txnDecode(t, s.callTool("node_edit", map[string]any{"commit": true}))
	if m["committed"] != true || m["validated"] != false || m["validationSkipped"] != "lsp-unconfirmed" {
		t.Fatalf("want committed, validated:false, validationSkipped:lsp-unconfirmed; got %v", m)
	}
	if got, _ := os.ReadFile(path); string(got) != good {
		t.Fatalf("committed edit not kept: %q", got)
	}
}

// trust_clean_diagnostics: a clean result validates the edit, under the
// same rules that pick it (an older version is skipped; unversioned, the
// first publish after the open is skipped, a clean one while the child
// loads is skipped). Off, the same result is lsp-unconfirmed, versioned
// or not, and a broken edit still reverts.
func TestEditTrustCleanDiagnostics(t *testing.T) {
	good := "package main\n\nfunc main() { println(1) }\n"
	for _, mode := range []string{"sourcekit", "loading", "versioned"} {
		for _, trust := range []bool{true, false} {
			name := mode + "/untrusted"
			if trust {
				name = mode + "/trusted"
			}
			t.Run(name, func(t *testing.T) {
				s, path := fakeDiagSessionTrust(t, mode, 5*time.Second, "package main\n\nfunc main() {}\n", trust)
				switch mode {
				case "loading":
					waitFirstPublish(t, s, path)
				case "versioned":
					time.Sleep(300 * time.Millisecond) // the edit is a didChange
				}
				orig, _ := os.ReadFile(path)
				m := editMain(t, s, path, brokenMain)
				if m["rejected"] != true || m["reverted"] != true {
					t.Fatalf("broken edit not reverted: %v", m)
				}
				if got, _ := os.ReadFile(path); string(got) != string(orig) {
					t.Fatalf("file not restored: %q", got)
				}
				if mode == "versioned" {
					time.Sleep(time.Second) // the revert's own result first
				}
				m = editMain(t, s, path, good)
				if !trust {
					wantUnconfirmed(t, m)
					return
				}
				if m["validated"] != true || m["rejected"] == true || m["validationSkipped"] != nil {
					t.Fatalf("trusted clean result: want validated:true, got %v", m)
				}
			})
		}
	}
}

// The macos-14 failure of 9b7385f: Xcode's sourcekit-lsp versions its
// publishes, and a version does not prove the text was analysed. A broken
// edit made while the package loads gets [] for its version, then the
// error for the same version 20ms later: reverted. The valid edit after
// the revert gets two unversioned publishes with the reverted text's
// error (stale), then [] for its version. sourcekit-lsp is not trusted:
// the valid edit is kept, lsp-unconfirmed; trusted, it validates. Either
// way its response lists no stale diagnostic.
func TestEditXcode14Replay(t *testing.T) {
	good := "package main\n\nfunc main() { println(1) }\n"
	for _, c := range []struct {
		mode  string
		trust bool
	}{{"xcode14", false}, {"xcode14", true}, {"xcode14-unversioned", false}, {"xcode14-unversioned", true}} {
		mode, trust := c.mode, c.trust
		name := mode + "/untrusted"
		if trust {
			name = mode + "/trusted"
		}
		t.Run(name, func(t *testing.T) {
			s, path := fakeDiagSessionTrust(t, mode, 5*time.Second, "package main\n\nfunc main() {}\n", trust)
			// The open's publish waits for the load: the edit is a didChange.
			time.Sleep(200 * time.Millisecond)
			orig, _ := os.ReadFile(path)
			m := editMain(t, s, path, brokenMain)
			if m["rejected"] != true || m["reverted"] != true {
				t.Fatalf("broken edit not reverted: %v", m)
			}
			if got, _ := os.ReadFile(path); string(got) != string(orig) {
				t.Fatalf("file not restored: %q", got)
			}
			m = editMain(t, s, path, good)
			if trust {
				if m["validated"] != true || m["rejected"] == true || m["validationSkipped"] != nil {
					t.Fatalf("trusted: want validated:true, got %v", m)
				}
			} else {
				wantUnconfirmed(t, m)
			}
			if d, _ := m["diagnostics"].([]any); len(d) != 0 {
				t.Fatalf("valid edit's response lists diagnostics not of its text: %v", d)
			}
			if got, _ := os.ReadFile(path); string(got) != good {
				t.Fatalf("valid edit not kept: %q", got)
			}
		})
	}
}

// A broken edit sent again right after its revert is reverted again. The
// fake, like gopls, publishes a change only if no later change follows
// within 300ms: without a wait for the revert's own result, the retry's
// baseline held the reverted error, and the retry was validated.
func TestEditRepeatAfterRevert(t *testing.T) {
	s, path := fakeDiagSessionTrust(t, "debounce", 5*time.Second, "package main\n\nfunc main() {}\n", true)
	waitFirstPublish(t, s, path)
	orig, _ := os.ReadFile(path)
	for i := 0; i < 3; i++ {
		m := editMain(t, s, path, brokenMain)
		if m["rejected"] != true || m["reverted"] != true {
			t.Fatalf("broken edit %d not reverted: %v", i+1, m)
		}
		if got, _ := os.ReadFile(path); string(got) != string(orig) {
			t.Fatalf("broken edit %d: file not restored: %q", i+1, got)
		}
	}
	good := "package main\n\nfunc main() { println(1) }\n"
	if m := editMain(t, s, path, good); m["validated"] != true {
		t.Fatalf("valid edit not validated: %v", m)
	}
}

// The same after a batch commit's revert, and after a rollback.
func TestBatchRepeatAfterRevert(t *testing.T) {
	s, path := fakeDiagSessionTrust(t, "debounce", 5*time.Second, "package main\n\nfunc main() {}\n", true)
	waitFirstPublish(t, s, path)
	orig, _ := os.ReadFile(path)
	stage := func() {
		t.Helper()
		cur, _ := os.ReadFile(path)
		r := s.callTool("node_edit", map[string]any{"node": "main.go", "oldText": string(cur), "newText": brokenMain, "commit": false})
		if m := txnDecode(t, r); m["staged"] != true {
			t.Fatalf("stage: %v", m)
		}
	}
	stage()
	if m := txnDecode(t, s.callTool("node_edit", map[string]any{"commit": true})); m["committed"] != false || m["reverted"] != true {
		t.Fatalf("broken batch not reverted: %v", m)
	}
	if m := editMain(t, s, path, brokenMain); m["rejected"] != true || m["reverted"] != true {
		t.Fatalf("broken edit after a batch revert not reverted: %v", m)
	}
	stage()
	if m := txnDecode(t, s.callTool("node_edit", map[string]any{"rollback": true})); m["rolledBack"] != true {
		t.Fatalf("rollback: %v", m)
	}
	if m := editMain(t, s, path, brokenMain); m["rejected"] != true || m["reverted"] != true {
		t.Fatalf("broken edit after a rollback not reverted: %v", m)
	}
	if got, _ := os.ReadFile(path); string(got) != string(orig) {
		t.Fatalf("file not restored: %q", got)
	}
}

// A revert the child never answers: the wait times out and the next
// edit's baseline is unknown. An error the reverted edit added counts as
// new again (the retry is reverted); one there before it (main.go starts
// with one BROKEN) does not (a valid edit that keeps it validates).
func TestEditRepeatAfterUnansweredRevert(t *testing.T) {
	for _, c := range []struct{ name, orig, broken, good string }{
		{"clean", "package main\n\nfunc main() {}\n", brokenMain, "package main\n\nfunc main() { println(1) }\n"},
		{"pre-existing", brokenMain, "package main\n\n// BROKEN BROKEN\nfunc main() {}\n", "package main\n\n// BROKEN\nfunc main() { println(1) }\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, path := fakeDiagSessionTrust(t, "skip-open-text", 600*time.Millisecond, c.orig, true)
			waitFirstPublish(t, s, path)
			for i := 0; i < 2; i++ {
				start := time.Now()
				m := editMain(t, s, path, c.broken)
				if m["rejected"] != true || m["reverted"] != true {
					t.Fatalf("broken edit %d not reverted: %v", i+1, m)
				}
				if el := time.Since(start); el < 600*time.Millisecond {
					t.Errorf("broken edit %d returned in %s: the revert's wait did not run to the cap", i+1, el)
				}
				if got, _ := os.ReadFile(path); string(got) != c.orig {
					t.Fatalf("broken edit %d: file not restored: %q", i+1, got)
				}
			}
			if m := editMain(t, s, path, c.good); m["validated"] != true {
				t.Fatalf("valid edit not validated: %v", m)
			}
		})
	}
}
