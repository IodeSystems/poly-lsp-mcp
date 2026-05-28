package server

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/tslsmcp/internal/config"
	"github.com/iodesystems/tslsmcp/internal/jsonrpc"
)

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
	srv := New(reg)

	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()

	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()

	return &lspSession{
		t:       t,
		srvIn:   cOut, // closing this signals EOF to the server
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
		if sym.Name != "UserID" {
			t.Errorf("query=UserID returned name %q (substring is allowed but exact preferred here)", sym.Name)
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
