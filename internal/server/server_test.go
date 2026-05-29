package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/tslsmcp/internal/config"
	"github.com/iodesystems/tslsmcp/internal/jsonrpc"
	"github.com/iodesystems/tslsmcp/internal/multiplex"
)

// tslsmcpBinary is the path of the tslsmcp binary built during TestMain.
// Used by tests that need a real child LSP — the binary speaks the
// protocol so we can exercise the server's forwarding paths against
// itself without depending on gopls/tsserver/pylsp being installed.
var tslsmcpBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tslsmcp-srv-test-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	tslsmcpBinary = filepath.Join(dir, "tslsmcp")
	out, err := exec.Command("go", "build", "-o", tslsmcpBinary, "github.com/iodesystems/tslsmcp").CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("build tslsmcp for tests: %v\n%s", err, out))
	}
	os.Exit(m.Run())
}

// lspSession drives a live Server through in-process io.Pipe pairs. It
// gives tests the same shape as a real client without spawning a binary.
type lspSession struct {
	t       *testing.T
	srvIn   *io.PipeWriter // close to end the session
	clientR *bufio.Reader
	clientW *io.PipeWriter
	done    chan error
	nextID  int64
}

func startSession(t *testing.T) *lspSession {
	t.Helper()
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	return startSessionWith(t, reg, nil)
}

func startSessionWith(t *testing.T, reg *config.Registry, mgr *multiplex.Manager) *lspSession {
	return startSessionFull(t, reg, mgr, nil, nil)
}

func startSessionFull(t *testing.T, reg *config.Registry, mgr *multiplex.Manager, declared []config.Binding, schemas []config.Schema) *lspSession {
	t.Helper()
	srv := New(reg, mgr, declared, schemas)

	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()

	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()

	return &lspSession{
		t:       t,
		srvIn:   cOut,
		clientR: bufio.NewReader(cIn),
		clientW: cOut,
		done:    done,
	}
}

func (s *lspSession) request(method string, params any) *jsonrpc.Message {
	s.t.Helper()
	id := atomic.AddInt64(&s.nextID, 1)
	rawID, _ := json.Marshal(id)
	rawParams, _ := json.Marshal(params)
	if err := jsonrpc.Write(s.clientW, &jsonrpc.Message{
		JSONRPC: "2.0",
		ID:      rawID,
		Method:  method,
		Params:  rawParams,
	}); err != nil {
		s.t.Fatalf("write %s: %v", method, err)
	}
	resp, err := jsonrpc.Read(s.clientR)
	if err != nil {
		s.t.Fatalf("read %s response: %v", method, err)
	}
	if string(resp.ID) != string(rawID) {
		s.t.Fatalf("id mismatch on %s: sent %s, got %s", method, rawID, resp.ID)
	}
	if resp.Error != nil {
		s.t.Fatalf("%s error: %s", method, resp.Error.Message)
	}
	return resp
}

func (s *lspSession) notify(method string, params any) {
	s.t.Helper()
	rawParams, _ := json.Marshal(params)
	if err := jsonrpc.Write(s.clientW, &jsonrpc.Message{
		JSONRPC: "2.0",
		Method:  method,
		Params:  rawParams,
	}); err != nil {
		s.t.Fatalf("notify %s: %v", method, err)
	}
}

func (s *lspSession) close() {
	s.t.Helper()
	s.request("shutdown", nil)
	s.notify("exit", nil)
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		s.srvIn.Close()
		s.t.Fatal("server did not shut down within 2s")
	}
}

// fixtureURI returns file://<abs path of the named fixture>.
func fixtureURI(t *testing.T, name string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	abs, err := filepath.Abs(filepath.Join(filepath.Dir(here), "..", "..", "testdata", "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	return "file://" + abs
}

func TestInitializeAdvertisesCapabilities(t *testing.T) {
	s := startSession(t)
	defer s.close()

	resp := s.request("initialize", map[string]any{
		"rootUri":      fixtureURI(t, "polyglot"),
		"capabilities": map[string]any{},
	})
	var got struct {
		Capabilities map[string]any `json:"capabilities"`
		ServerInfo   struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatal(err)
	}
	if got.ServerInfo.Name != "tslsmcp" {
		t.Errorf("serverInfo.name = %q, want tslsmcp", got.ServerInfo.Name)
	}
	if got.Capabilities["workspaceSymbolProvider"] != true {
		t.Errorf("workspaceSymbolProvider not advertised: %+v", got.Capabilities)
	}
	if got.Capabilities["referencesProvider"] != true {
		t.Errorf("referencesProvider not advertised: %+v", got.Capabilities)
	}
	s.notify("initialized", map[string]any{})
}

func TestWorkspaceSymbolFindsUserIDAcrossLanguages(t *testing.T) {
	s := startSession(t)
	defer s.close()

	s.request("initialize", map[string]any{"rootUri": fixtureURI(t, "polyglot")})
	s.notify("initialized", map[string]any{})

	resp := s.request("workspace/symbol", map[string]any{"query": "UserID"})
	var syms []symbolInformation
	if err := json.Unmarshal(resp.Result, &syms); err != nil {
		t.Fatalf("unmarshal symbols: %v", err)
	}
	if len(syms) < 6 {
		t.Errorf("got %d UserID symbols, want >= 6", len(syms))
	}
	files := map[string]bool{}
	for _, sym := range syms {
		if !strings.Contains(strings.ToLower(sym.Name), "userid") {
			t.Errorf("substring match: %q does not contain UserID", sym.Name)
		}
		files[filepath.Base(strings.TrimPrefix(sym.Location.URI, "file://"))] = true
	}
	for _, want := range []string{"main.go", "client.ts", "worker.py", "README.md", "config.yaml"} {
		if !files[want] {
			t.Errorf("UserID not surfaced from %s", want)
		}
	}
}

func TestWorkspaceSymbolEmptyQueryReturnsEverything(t *testing.T) {
	s := startSession(t)
	defer s.close()

	s.request("initialize", map[string]any{"rootUri": fixtureURI(t, "lsp-only")})
	s.notify("initialized", map[string]any{})

	resp := s.request("workspace/symbol", map[string]any{"query": ""})
	var syms []symbolInformation
	if err := json.Unmarshal(resp.Result, &syms); err != nil {
		t.Fatal(err)
	}
	if len(syms) == 0 {
		t.Fatal("empty query returned zero symbols")
	}
	// Must include something from each language.
	langs := map[string]int{} // ext seen
	for _, sym := range syms {
		path := strings.TrimPrefix(sym.Location.URI, "file://")
		ext := filepath.Ext(path)
		langs[ext]++
	}
	for _, want := range []string{".go", ".ts", ".py"} {
		if langs[want] == 0 {
			t.Errorf("no symbols from %s files", want)
		}
	}
}

func TestReferencesUsesPositionToFindWord(t *testing.T) {
	s := startSession(t)
	defer s.close()

	uri := fixtureURI(t, "polyglot")
	s.request("initialize", map[string]any{"rootUri": uri})
	s.notify("initialized", map[string]any{})

	// polyglot/main.go line 6 (0-based 5): `type UserID int64`
	// "UserID" starts at column 5 (after "type ").
	mainGo := strings.TrimPrefix(uri, "file://") + "/main.go"
	resp := s.request("textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": "file://" + mainGo},
		"position":     map[string]any{"line": 5, "character": 6},
		"context":      map[string]any{"includeDeclaration": true},
	})
	var locs []location
	if err := json.Unmarshal(resp.Result, &locs); err != nil {
		t.Fatalf("unmarshal references: %v", err)
	}
	if len(locs) < 6 {
		t.Errorf("got %d references, want >= 6", len(locs))
	}
	// Verify ranges are well-formed.
	for _, l := range locs {
		if l.Range.End.Character-l.Range.Start.Character != len("UserID") {
			t.Errorf("range width = %d, want %d for UserID: %+v",
				l.Range.End.Character-l.Range.Start.Character, len("UserID"), l)
		}
	}
}

func TestReferencesEmptyOnNonWordPosition(t *testing.T) {
	s := startSession(t)
	defer s.close()

	uri := fixtureURI(t, "polyglot")
	s.request("initialize", map[string]any{"rootUri": uri})
	s.notify("initialized", map[string]any{})

	mainGo := strings.TrimPrefix(uri, "file://") + "/main.go"
	// Line 1 is the blank line between `package main` and `import "fmt"`.
	// Cursor on character 0 of an empty line: no word in either direction.
	resp := s.request("textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": "file://" + mainGo},
		"position":     map[string]any{"line": 1, "character": 0},
	})
	var locs []location
	if err := json.Unmarshal(resp.Result, &locs); err != nil {
		t.Fatal(err)
	}
	if len(locs) != 0 {
		t.Errorf("expected zero refs on blank line, got %d", len(locs))
	}
}

func TestDidSaveRefreshesIndex(t *testing.T) {
	// Materialize a temp workspace we can safely mutate.
	dir := t.TempDir()
	original := []byte("package main\n\nfunc Alpha() {}\n")
	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := startSession(t)
	defer s.close()

	s.request("initialize", map[string]any{"rootUri": "file://" + dir})
	s.notify("initialized", map[string]any{})

	// Sanity: Alpha is indexed.
	resp := s.request("workspace/symbol", map[string]any{"query": "Alpha"})
	var syms []symbolInformation
	json.Unmarshal(resp.Result, &syms)
	if len(syms) == 0 {
		t.Fatal("Alpha missing from initial index")
	}

	// Replace Alpha → Beta on disk, then notify didSave.
	updated := []byte("package main\n\nfunc Beta() {}\n")
	if err := os.WriteFile(mainPath, updated, 0o644); err != nil {
		t.Fatal(err)
	}
	s.notify("textDocument/didSave", map[string]any{
		"textDocument": map[string]any{"uri": "file://" + mainPath},
	})
	// Drain: notifications don't have responses, but the server processes
	// in-order, so the next request only returns after didSave completed.
	resp = s.request("workspace/symbol", map[string]any{"query": "Alpha"})
	syms = nil
	json.Unmarshal(resp.Result, &syms)
	if len(syms) != 0 {
		t.Errorf("after didSave: Alpha still indexed: %+v", syms)
	}
	resp = s.request("workspace/symbol", map[string]any{"query": "Beta"})
	syms = nil
	json.Unmarshal(resp.Result, &syms)
	if len(syms) == 0 {
		t.Error("after didSave: Beta not picked up")
	}
}

func TestMergeCapabilitiesUnionsAndOverrides(t *testing.T) {
	childA := json.RawMessage(`{"hoverProvider":true,"definitionProvider":true}`)
	childB := json.RawMessage(`{"hoverProvider":false,"completionProvider":{"triggerCharacters":["."]}}`)
	own := map[string]any{
		"workspaceSymbolProvider": true,
		"hoverProvider":           "override-wins",
	}
	merged := mergeCapabilities(map[string]json.RawMessage{"a": childA, "b": childB}, own)

	if merged["workspaceSymbolProvider"] != true {
		t.Errorf("own cap missing: %+v", merged)
	}
	if merged["hoverProvider"] != "override-wins" {
		t.Errorf("our hoverProvider should override child; got %v", merged["hoverProvider"])
	}
	if merged["definitionProvider"] != true {
		t.Errorf("childA definitionProvider missing: %v", merged["definitionProvider"])
	}
	if _, ok := merged["completionProvider"]; !ok {
		t.Errorf("childB completionProvider missing: %+v", merged)
	}
}

func TestMergeCapabilitiesHandlesNilAndBadInputs(t *testing.T) {
	merged := mergeCapabilities(nil, map[string]any{"workspaceSymbolProvider": true})
	if merged["workspaceSymbolProvider"] != true {
		t.Errorf("own cap missing with nil child caps: %+v", merged)
	}

	bad := map[string]json.RawMessage{"junk": json.RawMessage(`{not json`)}
	merged = mergeCapabilities(bad, map[string]any{"x": 1})
	if merged["x"] != 1 {
		t.Errorf("malformed child JSON should be skipped: %+v", merged)
	}
}

// makeGoOverrideRegistry returns a registry where "go" points at the
// tslsmcp test binary so tests can spin up a child LSP without a real
// language server installed.
func makeGoOverrideRegistry(t *testing.T) *config.Registry {
	t.Helper()
	cfg := &config.Config{Languages: []config.Language{
		{Name: "go", Extensions: []string{"go", "mod"}, LSP: &config.LSP{Cmd: tslsmcpBinary}, TreeSitter: "go"},
	}}
	reg, err := cfg.Build()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// makeGoWorkspace materializes a temp dir with a single main.go containing
// the identifier wantSymbol so tests can verify documentSymbol routing.
func makeGoWorkspace(t *testing.T, wantSymbol string) string {
	t.Helper()
	dir := t.TempDir()
	mainGo := fmt.Sprintf("package main\n\nfunc %s() {}\n", wantSymbol)
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainGo), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDocumentSymbolWithManagerReturnsContent(t *testing.T) {
	reg := makeGoOverrideRegistry(t)
	mgr := multiplex.NewManager(reg)
	dir := makeGoWorkspace(t, "MarkerFunc")

	s := startSessionWith(t, reg, mgr)
	defer s.close()

	s.request("initialize", map[string]any{"rootUri": "file://" + dir})
	s.notify("initialized", map[string]any{})

	resp := s.request("textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": "file://" + filepath.Join(dir, "main.go")},
	})
	var syms []symbolInformation
	if err := json.Unmarshal(resp.Result, &syms); err != nil {
		t.Fatal(err)
	}
	if len(syms) == 0 {
		t.Fatal("documentSymbol returned empty")
	}
	found := false
	for _, sym := range syms {
		if sym.Name == "MarkerFunc" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("MarkerFunc missing from documentSymbol: %+v", syms)
	}
}

func TestForwardsUnknownTextDocumentMethodToChild(t *testing.T) {
	reg := makeGoOverrideRegistry(t)
	mgr := multiplex.NewManager(reg)
	dir := makeGoWorkspace(t, "Foo")

	s := startSessionWith(t, reg, mgr)
	defer s.close()

	s.request("initialize", map[string]any{"rootUri": "file://" + dir})
	s.notify("initialized", map[string]any{})

	// textDocument/hover is implemented by neither side. If we forward to
	// the child, the child returns -32601 method-not-found and our
	// forward wrapper sends it back as a -32603 internal error whose
	// message names the method. If we had NOT forwarded (e.g., no manager
	// or no child for this URI) the parent would have returned a null
	// result, not an error — so an error here proves the request reached
	// the child.
	id := atomic.AddInt64(&s.nextID, 1)
	rawID, _ := json.Marshal(id)
	params, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": "file://" + filepath.Join(dir, "main.go")},
		"position":     map[string]any{"line": 1, "character": 5},
	})
	if err := jsonrpc.Write(s.clientW, &jsonrpc.Message{
		JSONRPC: "2.0",
		ID:      rawID,
		Method:  "textDocument/hover",
		Params:  params,
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := jsonrpc.Read(s.clientR)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil {
		t.Fatalf("expected forwarded error from hover, got result=%s", resp.Result)
	}
	if resp.Error.Code != -32603 {
		t.Errorf("error code = %d, want -32603 (forward wrapper)", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "textDocument/hover") {
		t.Errorf("error message %q should name the forwarded method", resp.Error.Message)
	}
}

func TestForwardsTextDocumentWithNilManagerReplyNull(t *testing.T) {
	// No manager -> forward path returns null result for any
	// textDocument/* method we don't intercept. Sanity check that the
	// fallback exists and doesn't error.
	s := startSession(t)
	defer s.close()

	s.request("initialize", map[string]any{})
	s.notify("initialized", map[string]any{})

	id := atomic.AddInt64(&s.nextID, 1)
	rawID, _ := json.Marshal(id)
	params, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": "file:///tmp/whatever.go"},
		"position":     map[string]any{"line": 0, "character": 0},
	})
	if err := jsonrpc.Write(s.clientW, &jsonrpc.Message{
		JSONRPC: "2.0", ID: rawID, Method: "textDocument/hover", Params: params,
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := jsonrpc.Read(s.clientR)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Errorf("expected null result, got error %+v", resp.Error)
	}
	if string(resp.Result) != "null" {
		t.Errorf("expected null, got %s", resp.Result)
	}
}

func TestBindingsSurfaceAsSynonymInWorkspaceSymbol(t *testing.T) {
	// Build a tiny workspace with two .go files containing "UserID",
	// then declare a binding that aliases "UserType" to UserID's sites.
	// workspace/symbol "UserType" should return those sites — the
	// synonym semantics promised by Tier 2.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\ntype UserID int\n\nfunc f(id UserID) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	declared := []config.Binding{{
		Name:  "UserType",
		Sites: []config.BindingSite{{File: "main.go", Symbol: "UserID"}},
	}}

	s := startSessionFull(t, reg, nil, declared, nil)
	defer s.close()

	s.request("initialize", map[string]any{"rootUri": "file://" + dir})
	s.notify("initialized", map[string]any{})

	resp := s.request("workspace/symbol", map[string]any{"query": "UserType"})
	var syms []symbolInformation
	if err := json.Unmarshal(resp.Result, &syms); err != nil {
		t.Fatal(err)
	}
	if len(syms) == 0 {
		t.Fatal("workspace/symbol UserType returned no results — declared binding not surfacing")
	}
	for _, sym := range syms {
		if sym.Name != "UserType" {
			t.Errorf("symbol name = %q, want UserType (binding name should win)", sym.Name)
		}
		if filepath.Base(strings.TrimPrefix(sym.Location.URI, "file://")) != "main.go" {
			t.Errorf("URI = %q, want main.go", sym.Location.URI)
		}
	}
}

func TestBindingsJSONPathSurfacesYAMLValueAsSynonym(t *testing.T) {
	// The polyglot fixture's config.yaml has `user_id_type: UserID` —
	// declare a binding that pins the UserID value at that jsonpath
	// as "user identifier". workspace/symbol "user identifier" must
	// then return the YAML value's position.
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	declared := []config.Binding{{
		Name: "user identifier",
		Sites: []config.BindingSite{
			{File: "config.yaml", JSONPath: "$.user_id_type"},
		},
	}}

	s := startSessionFull(t, reg, nil, declared, nil)
	defer s.close()

	s.request("initialize", map[string]any{"rootUri": fixtureURI(t, "polyglot")})
	s.notify("initialized", map[string]any{})

	resp := s.request("workspace/symbol", map[string]any{"query": "user identifier"})
	var syms []symbolInformation
	if err := json.Unmarshal(resp.Result, &syms); err != nil {
		t.Fatal(err)
	}
	if len(syms) == 0 {
		t.Fatal("workspace/symbol 'user identifier' returned nothing — jsonpath binding not surfacing")
	}
	for _, sym := range syms {
		if sym.Name != "user identifier" {
			t.Errorf("name = %q, want 'user identifier'", sym.Name)
		}
		if !strings.HasSuffix(sym.Location.URI, "config.yaml") {
			t.Errorf("URI %q should end with config.yaml", sym.Location.URI)
		}
	}
}

func TestRenameAdvertisesProviderCapability(t *testing.T) {
	s := startSession(t)
	defer s.close()

	resp := s.request("initialize", map[string]any{
		"rootUri": fixtureURI(t, "polyglot"),
	})
	var got struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	json.Unmarshal(resp.Result, &got)
	if got.Capabilities["renameProvider"] != true {
		t.Errorf("renameProvider not advertised: %+v", got.Capabilities)
	}
	s.notify("initialized", map[string]any{})
}

func TestRenameLexicalFallbackBuildsWorkspaceEdit(t *testing.T) {
	// No declared bindings → rename uses lexical hits. Two Go files
	// share UserID; renaming at one position should produce edits in
	// both.
	dir := t.TempDir()
	file1 := filepath.Join(dir, "main.go")
	file2 := filepath.Join(dir, "helper.go")
	if err := os.WriteFile(file1,
		[]byte("package main\n\ntype UserID int\n\nfunc f(id UserID) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file2,
		[]byte("package main\n\nfunc g(x UserID) UserID { return x }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := startSession(t)
	defer s.close()
	s.request("initialize", map[string]any{"rootUri": "file://" + dir})
	s.notify("initialized", map[string]any{})

	// Cursor on UserID in main.go line 2 col 6 (0-based: line=2, char=5).
	resp := s.request("textDocument/rename", map[string]any{
		"textDocument": map[string]any{"uri": "file://" + file1},
		"position":     map[string]any{"line": 2, "character": 5},
		"newName":      "UserIdentifier",
	})
	var edit workspaceEdit
	if err := json.Unmarshal(resp.Result, &edit); err != nil {
		t.Fatalf("decode rename result: %v (raw=%s)", err, resp.Result)
	}
	if len(edit.Changes) != 2 {
		t.Errorf("expected edits in 2 files, got %d: %+v", len(edit.Changes), edit.Changes)
	}
	totalEdits := 0
	for uri, edits := range edit.Changes {
		for _, e := range edits {
			if e.NewText != "UserIdentifier" {
				t.Errorf("newText = %q in %s, want UserIdentifier", e.NewText, uri)
			}
			// Range width must equal len("UserID") = 6.
			if e.Range.End.Character-e.Range.Start.Character != 6 {
				t.Errorf("range width = %d, want 6: %+v in %s",
					e.Range.End.Character-e.Range.Start.Character, e.Range, uri)
			}
		}
		totalEdits += len(edits)
	}
	if totalEdits < 3 {
		t.Errorf("expected >= 3 edits (1 decl + 1 use in main, 2 uses in helper), got %d", totalEdits)
	}
}

func TestRenameWithDeclaredBindingRestrictsToDeclared(t *testing.T) {
	// Two Go files have UserID. A binding declares UserID in main.go
	// only. Rename of UserID should ONLY touch main.go — declared sites
	// are preferred when present (the user opted into precision).
	dir := t.TempDir()
	mainGo := filepath.Join(dir, "main.go")
	helperGo := filepath.Join(dir, "helper.go")
	os.WriteFile(mainGo,
		[]byte("package main\n\ntype UserID int\n\nfunc f(id UserID) {}\n"), 0o644)
	os.WriteFile(helperGo,
		[]byte("package main\n\nfunc g(x UserID) UserID { return x }\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\ngo 1.26\n"), 0o644)

	reg, _ := config.Default().Build()
	declared := []config.Binding{{
		Name:  "UserID",
		Sites: []config.BindingSite{{File: "main.go", Symbol: "UserID"}},
	}}

	s := startSessionFull(t, reg, nil, declared, nil)
	defer s.close()
	s.request("initialize", map[string]any{"rootUri": "file://" + dir})
	s.notify("initialized", map[string]any{})

	resp := s.request("textDocument/rename", map[string]any{
		"textDocument": map[string]any{"uri": "file://" + mainGo},
		"position":     map[string]any{"line": 2, "character": 5},
		"newName":      "UserIdentifier",
	})
	var edit workspaceEdit
	if err := json.Unmarshal(resp.Result, &edit); err != nil {
		t.Fatal(err)
	}
	if _, ok := edit.Changes["file://"+helperGo]; ok {
		t.Errorf("helper.go received edits even though declared binding scoped to main.go: %+v", edit.Changes)
	}
	if _, ok := edit.Changes["file://"+mainGo]; !ok {
		t.Errorf("main.go did not receive edits: %+v", edit.Changes)
	}
}

func TestRenameSkipsAliasingBindingSites(t *testing.T) {
	// Aliasing binding: name=UserType, sites=[symbol: UserID]. The
	// resolver registers declared sites under name "UserType" pointing
	// at UserID positions. If a user happens to rename "UserType" at
	// some cursor position, the declared sites' on-disk text is
	// "UserID", not "UserType" — those edits would mangle the file.
	// The aliasing-protection check must skip them.
	dir := t.TempDir()
	mainGo := filepath.Join(dir, "main.go")
	useTypeGo := filepath.Join(dir, "use_type.go")
	os.WriteFile(mainGo,
		[]byte("package main\n\ntype UserID int\n"), 0o644)
	os.WriteFile(useTypeGo,
		[]byte("package main\n\nvar UserType int\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\ngo 1.26\n"), 0o644)

	reg, _ := config.Default().Build()
	declared := []config.Binding{{
		Name:  "UserType",
		Sites: []config.BindingSite{{File: "main.go", Symbol: "UserID"}}, // aliasing
	}}

	s := startSessionFull(t, reg, nil, declared, nil)
	defer s.close()
	s.request("initialize", map[string]any{"rootUri": "file://" + dir})
	s.notify("initialized", map[string]any{})

	// Cursor on UserType in use_type.go line 2 col 5 (0-based: 2, 4).
	resp := s.request("textDocument/rename", map[string]any{
		"textDocument": map[string]any{"uri": "file://" + useTypeGo},
		"position":     map[string]any{"line": 2, "character": 4},
		"newName":      "Renamed",
	})
	if string(resp.Result) == "null" {
		// If null, the declared sites all got filtered out by the
		// aliasing-protection check, which is the right outcome here.
		return
	}
	var edit workspaceEdit
	if err := json.Unmarshal(resp.Result, &edit); err != nil {
		t.Fatal(err)
	}
	// main.go must NOT appear — its declared-binding text is "UserID",
	// not "UserType", so the aliasing check should have rejected it.
	if _, ok := edit.Changes["file://"+mainGo]; ok {
		t.Errorf("main.go received edits despite aliasing-mismatch: %+v", edit.Changes)
	}
}

func TestRenameNullForUnknownPosition(t *testing.T) {
	s := startSession(t)
	defer s.close()
	s.request("initialize", map[string]any{"rootUri": fixtureURI(t, "polyglot")})
	s.notify("initialized", map[string]any{})

	// Blank line in main.go: no word at cursor.
	resp := s.request("textDocument/rename", map[string]any{
		"textDocument": map[string]any{"uri": fixtureURI(t, "polyglot") + "/main.go"},
		"position":     map[string]any{"line": 1, "character": 0},
		"newName":      "Whatever",
	})
	if string(resp.Result) != "null" {
		t.Errorf("expected null result for rename on blank line, got %s", resp.Result)
	}
}

func TestRenameRejectsEmptyNewName(t *testing.T) {
	s := startSession(t)
	defer s.close()
	s.request("initialize", map[string]any{"rootUri": fixtureURI(t, "polyglot")})
	s.notify("initialized", map[string]any{})

	// request() t.Fatals on error, so use raw send + recv.
	rawID := s.nextRawID()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(rawID),
		"method":  "textDocument/rename",
		"params": map[string]any{
			"textDocument": map[string]any{"uri": fixtureURI(t, "polyglot") + "/main.go"},
			"position":     map[string]any{"line": 5, "character": 6},
			"newName":      "",
		},
	})
	s.sendRaw(body)
	resp := s.recvMessage()
	if resp.Error == nil {
		t.Fatalf("empty newName accepted: %s", resp.Result)
	}
	if resp.Error.Code != errInvalidParams {
		t.Errorf("error code = %d, want %d", resp.Error.Code, errInvalidParams)
	}
}

func TestSchemaAnchoredBindingsPromoteWorkspaceHits(t *testing.T) {
	// Tier 3: declaring api.proto (which already exists in the polyglot
	// fixture and contains `message UserID`) must auto-bind every
	// workspace position for UserID across go/ts/py/yaml/md as
	// declared. Rename through any of those positions should then
	// touch every member including the proto declaration.
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	schemas := []config.Schema{{File: "api.proto", Dialect: "proto"}}

	s := startSessionFull(t, reg, nil, nil, schemas)
	defer s.close()

	s.request("initialize", map[string]any{"rootUri": fixtureURI(t, "polyglot")})
	s.notify("initialized", map[string]any{})

	// Rename UserID from the Go declaration. The proto file must be
	// part of the resulting WorkspaceEdit because Tier 3 auto-bound it.
	resp := s.request("textDocument/rename", map[string]any{
		"textDocument": map[string]any{
			"uri": fixtureURI(t, "polyglot") + "/main.go",
		},
		"position": map[string]any{"line": 5, "character": 6},
		"newName":  "PersonID",
	})
	var edit workspaceEdit
	if err := json.Unmarshal(resp.Result, &edit); err != nil {
		t.Fatal(err)
	}

	files := map[string]int{}
	for uri, edits := range edit.Changes {
		base := filepath.Base(strings.TrimPrefix(uri, "file://"))
		files[base] += len(edits)
	}

	// api.proto must be in the edit set — that's the whole point of
	// schema-anchored bindings.
	if files["api.proto"] == 0 {
		t.Errorf("api.proto missing from rename edit (Tier 3 not pulling its weight): %+v", files)
	}
	// And it should still cross go/ts/py via the lexical→declared
	// promotion path.
	for _, want := range []string{"main.go", "client.ts", "worker.py"} {
		if files[want] == 0 {
			t.Errorf("%s missing from rename edit: %+v", want, files)
		}
	}
}

func TestSchemaAnchoredOpenAPIPromotesUserIDAcrossWorkspace(t *testing.T) {
	// polyglot fixture has openapi.yaml declaring `UserID` under
	// components.schemas and `GetUser` under paths.../operationId.
	// Declaring it as a schema must surface both names with declared
	// confidence and pull openapi.yaml into a UserID rename.
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	schemas := []config.Schema{{File: "openapi.yaml", Dialect: "openapi"}}

	s := startSessionFull(t, reg, nil, nil, schemas)
	defer s.close()

	s.request("initialize", map[string]any{"rootUri": fixtureURI(t, "polyglot")})
	s.notify("initialized", map[string]any{})

	resp := s.request("textDocument/rename", map[string]any{
		"textDocument": map[string]any{
			"uri": fixtureURI(t, "polyglot") + "/main.go",
		},
		"position": map[string]any{"line": 5, "character": 6},
		"newName":  "PersonID",
	})
	var edit workspaceEdit
	if err := json.Unmarshal(resp.Result, &edit); err != nil {
		t.Fatal(err)
	}
	hit := false
	for uri := range edit.Changes {
		if strings.HasSuffix(uri, "openapi.yaml") {
			hit = true
		}
	}
	if !hit {
		t.Errorf("openapi.yaml missing from rename edit; Tier 3 openapi not pulling its weight: %+v", edit.Changes)
	}
}

func TestUnknownMethodReturnsMethodNotFound(t *testing.T) {
	s := startSession(t)
	defer s.close()

	s.request("initialize", map[string]any{})
	s.notify("initialized", map[string]any{})

	id := atomic.AddInt64(&s.nextID, 1)
	rawID, _ := json.Marshal(id)
	if err := jsonrpc.Write(s.clientW, &jsonrpc.Message{
		JSONRPC: "2.0",
		ID:      rawID,
		Method:  "no/such/thing",
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := jsonrpc.Read(s.clientR)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Errorf("want -32601 method not found, got error=%+v", resp.Error)
	}
}
