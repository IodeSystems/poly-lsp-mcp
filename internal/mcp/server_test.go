package mcp

import (
	"encoding/json"
	"errors"
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

// mcpSession drives a live MCP server through io.Pipe pairs using
// newline-delimited JSON-RPC framing. Mirrors the LSP test session in
// shape but uses json.Encoder/Decoder instead of the LSP framer.
type mcpSession struct {
	t       *testing.T
	srv     *Server // exposed for tests that need to peek at internals
	srvIn   *io.PipeWriter
	clientR *json.Decoder
	clientW *io.PipeWriter
	done    chan error
	nextID  int64
}

func startSession(t *testing.T, root string) *mcpSession {
	return startSessionFull(t, root, nil, nil)
}

func startSessionFull(t *testing.T, root string, bs []config.Binding, ss []config.Schema) *mcpSession {
	t.Helper()
	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	srv := New(reg, root, bs, ss)

	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()

	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()

	return &mcpSession{
		t:       t,
		srv:     srv,
		srvIn:   cOut,
		clientR: json.NewDecoder(cIn),
		clientW: cOut,
		done:    done,
	}
}

func (s *mcpSession) sendMessage(msg *jsonrpc.Message) {
	s.t.Helper()
	enc := json.NewEncoder(s.clientW)
	if err := enc.Encode(msg); err != nil {
		s.t.Fatalf("encode: %v", err)
	}
}

func (s *mcpSession) request(method string, params any) *jsonrpc.Message {
	s.t.Helper()
	id := atomic.AddInt64(&s.nextID, 1)
	rawID, _ := json.Marshal(id)
	var rawParams json.RawMessage
	if params != nil {
		rawParams, _ = json.Marshal(params)
	}
	s.sendMessage(&jsonrpc.Message{
		JSONRPC: "2.0",
		ID:      rawID,
		Method:  method,
		Params:  rawParams,
	})
	var resp jsonrpc.Message
	if err := s.clientR.Decode(&resp); err != nil {
		s.t.Fatalf("decode response for %s: %v", method, err)
	}
	if string(resp.ID) != string(rawID) {
		s.t.Fatalf("id mismatch on %s: sent %s, got %s", method, rawID, resp.ID)
	}
	return &resp
}

func (s *mcpSession) notify(method string, params any) {
	s.t.Helper()
	var rawParams json.RawMessage
	if params != nil {
		rawParams, _ = json.Marshal(params)
	}
	s.sendMessage(&jsonrpc.Message{
		JSONRPC: "2.0",
		Method:  method,
		Params:  rawParams,
	})
}

func (s *mcpSession) close() {
	s.t.Helper()
	s.request("shutdown", nil)
	s.clientW.Close()
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		s.srvIn.Close()
		s.t.Fatal("MCP server did not exit within 2s")
	}
}

// polyglotFixture returns the absolute path of the polyglot test
// workspace so MCP tools have something to index.
func polyglotFixture(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	abs, err := filepath.Abs(filepath.Join(filepath.Dir(here), "..", "..", "testdata", "fixtures", "polyglot"))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// ---- protocol lifecycle ----

func TestInitializeReturnsProtocolVersionAndServerInfo(t *testing.T) {
	s := startSession(t, "")
	defer s.close()

	resp := s.request("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0.0"},
	})
	if resp.Error != nil {
		t.Fatalf("initialize errored: %+v", resp.Error)
	}
	var got struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools any `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatal(err)
	}
	if got.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocolVersion = %q, want 2024-11-05", got.ProtocolVersion)
	}
	if got.Capabilities.Tools == nil {
		t.Error("tools capability missing")
	}
	if got.ServerInfo.Name != "tslsmcp" {
		t.Errorf("serverInfo.name = %q, want tslsmcp", got.ServerInfo.Name)
	}
}

func TestPreInitMethodsAreRejected(t *testing.T) {
	s := startSession(t, "")
	defer func() {
		s.clientW.Close()
		<-s.done
	}()

	// Don't request initialize. Call tools/list first.
	resp := s.request("tools/list", nil)
	if resp.Error == nil || resp.Error.Code != -32002 {
		t.Errorf("expected -32002 ServerNotInitialized, got %+v", resp.Error)
	}
}

func TestDoubleInitializeRejected(t *testing.T) {
	s := startSession(t, "")
	defer s.close()

	s.request("initialize", map[string]any{})
	resp := s.request("initialize", map[string]any{})
	if resp.Error == nil || resp.Error.Code != -32600 {
		t.Errorf("expected -32600 InvalidRequest on second init, got %+v", resp.Error)
	}
}

func TestEOFWithoutShutdownReturnsSentinel(t *testing.T) {
	s := startSession(t, "")
	s.clientW.Close()
	select {
	case got := <-s.done:
		if !errors.Is(got, ErrExitWithoutShutdown) {
			t.Errorf("Serve returned %v, want ErrExitWithoutShutdown", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return")
	}
}

// ---- tools/list ----

func TestToolsListReturnsRegisteredTools(t *testing.T) {
	s := startSession(t, "")
	defer s.close()

	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/list", nil)
	if resp.Error != nil {
		t.Fatal(resp.Error.Message)
	}
	var got struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range got.Tools {
		names[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %q has empty description", tool.Name)
		}
		if len(tool.InputSchema) == 0 || string(tool.InputSchema) == "null" {
			t.Errorf("tool %q has empty inputSchema", tool.Name)
		}
	}
	for _, want := range []string{"find_symbol", "find_references", "rename", "list_bindings", "document_symbols", "refresh", "apply_rename"} {
		if !names[want] {
			t.Errorf("tool %q missing from list", want)
		}
	}
}

// ---- tools/call ----

func TestFindSymbolToolReturnsCrossLanguageHits(t *testing.T) {
	root := polyglotFixture(t)
	s := startSessionFull(t, root, nil, nil)
	defer s.close()

	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/call", map[string]any{
		"name":      "find_symbol",
		"arguments": map[string]any{"query": "UserID"},
	})
	if resp.Error != nil {
		t.Fatal(resp.Error.Message)
	}
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatal(err)
	}
	if r.IsError {
		t.Fatalf("tool reported error: %+v", r.Content)
	}
	if len(r.Content) == 0 {
		t.Fatal("empty tool content")
	}

	var hits []siteJSON
	if err := json.Unmarshal([]byte(r.Content[0].Text), &hits); err != nil {
		t.Fatalf("tool content not JSON-shaped: %v\npayload: %s", err, r.Content[0].Text)
	}
	if len(hits) < 5 {
		t.Errorf("got %d UserID hits, want >= 5", len(hits))
	}
	langs := map[string]bool{}
	for _, h := range hits {
		langs[h.Language] = true
		// Files should be workspace-relative, not absolute.
		if strings.HasPrefix(h.File, "/") {
			t.Errorf("expected workspace-relative file path, got %q", h.File)
		}
	}
	for _, want := range []string{"go", "typescript", "python"} {
		if !langs[want] {
			t.Errorf("missing language %q in hits: %+v", want, langs)
		}
	}
}

func TestFindReferencesToolEchoesNameInResults(t *testing.T) {
	root := polyglotFixture(t)
	s := startSessionFull(t, root, nil, nil)
	defer s.close()

	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/call", map[string]any{
		"name":      "find_references",
		"arguments": map[string]any{"name": "UserID"},
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	if r.IsError {
		t.Fatalf("tool errored: %+v", r.Content)
	}
	var hits []siteJSON
	json.Unmarshal([]byte(r.Content[0].Text), &hits)
	if len(hits) == 0 {
		t.Fatal("no references for UserID")
	}
	for _, h := range hits {
		if h.Name != "UserID" {
			t.Errorf("hit name = %q, want UserID", h.Name)
		}
	}
}

func TestRenameToolProducesCrossLanguageEdits(t *testing.T) {
	root := polyglotFixture(t)
	schemas := []config.Schema{
		{File: "api.proto", Dialect: "proto"},
	}
	s := startSessionFull(t, root, nil, schemas)
	defer s.close()

	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/call", map[string]any{
		"name":      "rename",
		"arguments": map[string]any{"name": "UserID", "newName": "PersonID"},
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	if r.IsError {
		t.Fatalf("rename tool errored: %+v", r.Content)
	}
	var payload struct {
		Name    string       `json:"name"`
		NewName string       `json:"newName"`
		Edits   []renameEdit `json:"edits"`
	}
	if err := json.Unmarshal([]byte(r.Content[0].Text), &payload); err != nil {
		t.Fatalf("rename payload not JSON-shaped: %v\n%s", err, r.Content[0].Text)
	}
	if payload.Name != "UserID" || payload.NewName != "PersonID" {
		t.Errorf("payload header mismatch: %+v", payload)
	}
	if len(payload.Edits) == 0 {
		t.Fatal("rename produced no edits")
	}

	files := map[string]int{}
	for _, e := range payload.Edits {
		if e.OldText != "UserID" {
			t.Errorf("edit oldText = %q, want UserID", e.OldText)
		}
		if e.NewText != "PersonID" {
			t.Errorf("edit newText = %q, want PersonID", e.NewText)
		}
		files[e.File]++
	}
	// With proto schema declared, api.proto must be in the edit set.
	if files["api.proto"] == 0 {
		t.Errorf("api.proto missing from rename edits: %+v", files)
	}
	// And the language fan-out should still hit Go/TS/Python.
	for _, want := range []string{"main.go", "client.ts", "worker.py"} {
		if files[want] == 0 {
			t.Errorf("%s missing from rename edits: %+v", want, files)
		}
	}
}

func TestRenameToolNoMatchesReturnsEmptyEdits(t *testing.T) {
	root := polyglotFixture(t)
	s := startSessionFull(t, root, nil, nil)
	defer s.close()

	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/call", map[string]any{
		"name":      "rename",
		"arguments": map[string]any{"name": "DefinitelyNotInWorkspace", "newName": "Whatever"},
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	if r.IsError {
		t.Fatalf("missing-name should not be a tool error: %+v", r.Content)
	}
	var payload struct {
		Edits []renameEdit `json:"edits"`
	}
	json.Unmarshal([]byte(r.Content[0].Text), &payload)
	if len(payload.Edits) != 0 {
		t.Errorf("expected zero edits, got %+v", payload.Edits)
	}
}

func TestRenameToolMissingArgumentsIsToolError(t *testing.T) {
	s := startSession(t, polyglotFixture(t))
	defer s.close()

	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/call", map[string]any{
		"name":      "rename",
		"arguments": map[string]any{"name": "UserID"}, // no newName
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	if !r.IsError {
		t.Errorf("expected isError=true for missing newName, got %+v", r)
	}
}

func TestListBindingsToolWithNoBindingsReturnsEmpty(t *testing.T) {
	root := polyglotFixture(t)
	s := startSessionFull(t, root, nil, nil)
	defer s.close()

	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/call", map[string]any{
		"name":      "list_bindings",
		"arguments": map[string]any{},
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	if r.IsError {
		t.Fatalf("list_bindings errored: %+v", r.Content)
	}
	var got []bindingSummary
	json.Unmarshal([]byte(r.Content[0].Text), &got)
	if len(got) != 0 {
		t.Errorf("no Tier-2 or Tier-3 bindings declared, got %d catalog entries: %+v", len(got), got)
	}
}

func TestListBindingsToolReturnsCatalogWithLanguagesAndSites(t *testing.T) {
	root := polyglotFixture(t)
	schemas := []config.Schema{
		{File: "api.proto", Dialect: "proto"},
	}
	s := startSessionFull(t, root, nil, schemas)
	defer s.close()

	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/call", map[string]any{
		"name":      "list_bindings",
		"arguments": map[string]any{},
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	if r.IsError {
		t.Fatalf("list_bindings errored: %+v", r.Content)
	}
	var got []bindingSummary
	if err := json.Unmarshal([]byte(r.Content[0].Text), &got); err != nil {
		t.Fatalf("payload not catalog-shaped: %v\n%s", err, r.Content[0].Text)
	}
	if len(got) == 0 {
		t.Fatal("schema declared but no bindings surfaced")
	}
	// UserID from api.proto should be in the catalog.
	var userIDEntry *bindingSummary
	for i, e := range got {
		if e.Name == "UserID" {
			userIDEntry = &got[i]
			break
		}
	}
	if userIDEntry == nil {
		t.Fatalf("UserID not in binding catalog: %+v", got)
	}
	if userIDEntry.SiteCount == 0 {
		t.Errorf("UserID has zero sites: %+v", userIDEntry)
	}
	langSet := map[string]bool{}
	for _, l := range userIDEntry.Languages {
		langSet[l] = true
	}
	if !langSet["proto"] {
		t.Errorf("UserID missing proto language tag: %+v", userIDEntry.Languages)
	}
	for _, want := range []string{"go", "typescript", "python"} {
		if !langSet[want] {
			t.Errorf("UserID missing %s language tag (schema should promote workspace hits): %+v",
				want, userIDEntry.Languages)
		}
	}
	for _, site := range userIDEntry.Sites {
		if site.Confidence != "declared" {
			t.Errorf("catalog returned non-declared site: %+v", site)
		}
	}
}

func TestDocumentSymbolsToolReturnsFileSortedHits(t *testing.T) {
	root := polyglotFixture(t)
	s := startSessionFull(t, root, nil, nil)
	defer s.close()

	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/call", map[string]any{
		"name":      "document_symbols",
		"arguments": map[string]any{"file": "main.go"}, // workspace-relative
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	if r.IsError {
		t.Fatalf("document_symbols errored: %+v", r.Content)
	}
	var hits []siteJSON
	if err := json.Unmarshal([]byte(r.Content[0].Text), &hits); err != nil {
		t.Fatalf("payload not sites-shaped: %v\n%s", err, r.Content[0].Text)
	}
	if len(hits) == 0 {
		t.Fatal("document_symbols(main.go) returned nothing")
	}
	for _, h := range hits {
		if h.File != "main.go" {
			t.Errorf("hit file = %q, want main.go", h.File)
		}
	}
	// Sorted by (line, col)?
	for i := 1; i < len(hits); i++ {
		prev, cur := hits[i-1], hits[i]
		if prev.Line > cur.Line || (prev.Line == cur.Line && prev.Col > cur.Col) {
			t.Errorf("hits not sorted at index %d: %+v then %+v", i, prev, cur)
		}
	}
	// Names should include the Go identifiers we expect.
	names := map[string]bool{}
	for _, h := range hits {
		names[h.Name] = true
	}
	for _, want := range []string{"UserID", "GreetUser", "main"} {
		if !names[want] {
			t.Errorf("expected %q in main.go symbols, got: %+v", want, names)
		}
	}
}

func TestDocumentSymbolsToolAcceptsAbsolutePath(t *testing.T) {
	root := polyglotFixture(t)
	s := startSessionFull(t, root, nil, nil)
	defer s.close()

	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/call", map[string]any{
		"name":      "document_symbols",
		"arguments": map[string]any{"file": filepath.Join(root, "client.ts")},
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	if r.IsError {
		t.Fatalf("errored: %+v", r.Content)
	}
	var hits []siteJSON
	json.Unmarshal([]byte(r.Content[0].Text), &hits)
	if len(hits) == 0 {
		t.Fatal("document_symbols on absolute client.ts returned nothing")
	}
	for _, h := range hits {
		// Output should still be workspace-relative regardless of input.
		if filepath.IsAbs(h.File) {
			t.Errorf("expected workspace-relative output, got %q", h.File)
		}
	}
}

func TestDocumentSymbolsToolMissingFileArgIsError(t *testing.T) {
	s := startSession(t, polyglotFixture(t))
	defer s.close()

	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/call", map[string]any{
		"name":      "document_symbols",
		"arguments": map[string]any{},
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	if !r.IsError {
		t.Errorf("expected isError=true for missing file arg, got %+v", r)
	}
}

type refreshStatus struct {
	Root          string `json:"root"`
	Names         int    `json:"names"`
	DeclaredSites int    `json:"declaredSites"`
	SchemaSites   int    `json:"schemaSites"`
}

func TestRefreshToolPicksUpNewFiles(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath,
		[]byte("package main\n\nfunc OriginalName() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// Sanity: OriginalName is found.
	resp := s.request("tools/call", map[string]any{
		"name":      "find_references",
		"arguments": map[string]any{"name": "OriginalName"},
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	var hits []siteJSON
	json.Unmarshal([]byte(r.Content[0].Text), &hits)
	if len(hits) == 0 {
		t.Fatal("pre-refresh: OriginalName missing")
	}

	// Edit on disk: replace OriginalName with NewName.
	if err := os.WriteFile(mainPath,
		[]byte("package main\n\nfunc NewName() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Before refresh, NewName isn't indexed.
	resp = s.request("tools/call", map[string]any{
		"name":      "find_references",
		"arguments": map[string]any{"name": "NewName"},
	})
	json.Unmarshal(resp.Result, &r)
	json.Unmarshal([]byte(r.Content[0].Text), &hits)
	if len(hits) != 0 {
		t.Errorf("expected NewName to be invisible before refresh, got %d hits", len(hits))
	}

	// Refresh and re-query.
	resp = s.request("tools/call", map[string]any{
		"name":      "refresh",
		"arguments": map[string]any{},
	})
	json.Unmarshal(resp.Result, &r)
	if r.IsError {
		t.Fatalf("refresh errored: %+v", r.Content)
	}
	var status refreshStatus
	json.Unmarshal([]byte(r.Content[0].Text), &status)
	if status.Root != dir {
		t.Errorf("status.root = %q, want %q", status.Root, dir)
	}
	if status.Names == 0 {
		t.Errorf("status.names = 0 after refresh; expected something")
	}

	resp = s.request("tools/call", map[string]any{
		"name":      "find_references",
		"arguments": map[string]any{"name": "NewName"},
	})
	json.Unmarshal(resp.Result, &r)
	json.Unmarshal([]byte(r.Content[0].Text), &hits)
	if len(hits) == 0 {
		t.Error("post-refresh: NewName still not visible")
	}

	resp = s.request("tools/call", map[string]any{
		"name":      "find_references",
		"arguments": map[string]any{"name": "OriginalName"},
	})
	json.Unmarshal(resp.Result, &r)
	json.Unmarshal([]byte(r.Content[0].Text), &hits)
	if len(hits) != 0 {
		t.Errorf("post-refresh: OriginalName should be gone, got %d hits", len(hits))
	}
}

func TestRefreshToolSwitchesWorkspaceRoot(t *testing.T) {
	// Two temp workspaces with different names; refresh with workspace_root
	// to swap, then verify queries land on the second one.
	dirA := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "a.go"),
		[]byte("package x\n\nfunc OnlyInA() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "go.mod"),
		[]byte("module a\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirB := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirB, "b.go"),
		[]byte("package x\n\nfunc OnlyInB() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "go.mod"),
		[]byte("module b\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := startSessionFull(t, dirA, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// Refresh against the second workspace.
	resp := s.request("tools/call", map[string]any{
		"name":      "refresh",
		"arguments": map[string]any{"workspace_root": dirB},
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	if r.IsError {
		t.Fatalf("refresh errored: %+v", r.Content)
	}
	var status refreshStatus
	json.Unmarshal([]byte(r.Content[0].Text), &status)
	if status.Root != dirB {
		t.Errorf("status.root = %q, want %q", status.Root, dirB)
	}

	// OnlyInB is reachable.
	resp = s.request("tools/call", map[string]any{
		"name":      "find_references",
		"arguments": map[string]any{"name": "OnlyInB"},
	})
	json.Unmarshal(resp.Result, &r)
	var hits []siteJSON
	json.Unmarshal([]byte(r.Content[0].Text), &hits)
	if len(hits) == 0 {
		t.Error("OnlyInB not visible after refresh to dirB")
	}
	for _, h := range hits {
		if h.File != "b.go" {
			t.Errorf("file = %q, want b.go (path should be relative to NEW root)", h.File)
		}
	}

	// OnlyInA is no longer reachable (we're pointing at dirB).
	resp = s.request("tools/call", map[string]any{
		"name":      "find_references",
		"arguments": map[string]any{"name": "OnlyInA"},
	})
	json.Unmarshal(resp.Result, &r)
	json.Unmarshal([]byte(r.Content[0].Text), &hits)
	if len(hits) != 0 {
		t.Errorf("OnlyInA should be invisible after switching root, got %+v", hits)
	}
}

func TestRefreshToolRejectsRelativePath(t *testing.T) {
	s := startSession(t, polyglotFixture(t))
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/call", map[string]any{
		"name":      "refresh",
		"arguments": map[string]any{"workspace_root": "relative/path"},
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	if !r.IsError {
		t.Errorf("expected isError=true for relative path, got %+v", r)
	}
}

func TestRefreshToolReappliesSchemas(t *testing.T) {
	// Configure a schema; after refresh, schema-anchored bindings must
	// still be in effect.
	root := polyglotFixture(t)
	schemas := []config.Schema{{File: "api.proto", Dialect: "proto"}}
	s := startSessionFull(t, root, nil, schemas)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/call", map[string]any{
		"name":      "refresh",
		"arguments": map[string]any{},
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	if r.IsError {
		t.Fatalf("refresh errored: %+v", r.Content)
	}
	var status refreshStatus
	json.Unmarshal([]byte(r.Content[0].Text), &status)
	if status.SchemaSites == 0 {
		t.Errorf("expected schema sites > 0 after refresh, got %d", status.SchemaSites)
	}

	// list_bindings should still show UserID with proto in its languages.
	resp = s.request("tools/call", map[string]any{
		"name":      "list_bindings",
		"arguments": map[string]any{},
	})
	json.Unmarshal(resp.Result, &r)
	var cat []bindingSummary
	json.Unmarshal([]byte(r.Content[0].Text), &cat)
	found := false
	for _, b := range cat {
		if b.Name == "UserID" {
			for _, l := range b.Languages {
				if l == "proto" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("UserID/proto binding missing after refresh: %+v", cat)
	}
}

type applyStatus struct {
	Name         string        `json:"name"`
	NewName      string        `json:"newName"`
	FilesChanged int           `json:"filesChanged"`
	Results      []applyResult `json:"results"`
}

func TestApplyRenameToolWritesEditsToDisk(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.go")
	helperPath := filepath.Join(dir, "helper.go")
	original := "package main\n\ntype UserID int\n\nfunc f(id UserID) {}\n"
	if err := os.WriteFile(mainPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helperPath,
		[]byte("package main\n\nfunc g(x UserID) UserID { return x }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/call", map[string]any{
		"name":      "apply_rename",
		"arguments": map[string]any{"name": "UserID", "newName": "PersonID"},
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	if r.IsError {
		t.Fatalf("apply_rename errored: %+v", r.Content)
	}
	var status applyStatus
	json.Unmarshal([]byte(r.Content[0].Text), &status)
	if status.FilesChanged != 2 {
		t.Errorf("filesChanged = %d, want 2", status.FilesChanged)
	}
	for _, res := range status.Results {
		if res.Skipped != "" {
			t.Errorf("file %s skipped: %s", res.File, res.Skipped)
		}
		if res.Edits == 0 {
			t.Errorf("file %s had zero edits applied", res.File)
		}
	}

	mainAfter, _ := os.ReadFile(mainPath)
	helperAfter, _ := os.ReadFile(helperPath)
	if strings.Contains(string(mainAfter), "UserID") {
		t.Errorf("main.go still contains UserID after rename:\n%s", mainAfter)
	}
	if !strings.Contains(string(mainAfter), "PersonID") {
		t.Errorf("main.go missing PersonID after rename:\n%s", mainAfter)
	}
	if strings.Contains(string(helperAfter), "UserID") {
		t.Errorf("helper.go still contains UserID after rename:\n%s", helperAfter)
	}
	if !strings.Contains(string(helperAfter), "PersonID") {
		t.Errorf("helper.go missing PersonID after rename:\n%s", helperAfter)
	}
	// Sanity: surrounding text preserved.
	if !strings.Contains(string(mainAfter), "package main") {
		t.Error("apply_rename damaged the file header")
	}
	if !strings.Contains(string(mainAfter), "func f(id PersonID) {}") {
		t.Errorf("apply_rename produced unexpected main.go:\n%s", mainAfter)
	}
}

func TestApplyRenamePreservesFilePermissions(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "run.go")
	if err := os.WriteFile(scriptPath,
		[]byte("package main\n\nfunc Target() {}\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	s.request("tools/call", map[string]any{
		"name":      "apply_rename",
		"arguments": map[string]any{"name": "Target", "newName": "Renamed"},
	})
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("file permission lost: got %o, want 0755", info.Mode().Perm())
	}
}

func TestApplyRenameNoMatchesReturnsZeroFiles(t *testing.T) {
	root := polyglotFixture(t)
	s := startSessionFull(t, root, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/call", map[string]any{
		"name":      "apply_rename",
		"arguments": map[string]any{"name": "DefinitelyNotInWorkspace", "newName": "Whatever"},
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	if r.IsError {
		t.Fatalf("no-match should not error: %+v", r.Content)
	}
	var status applyStatus
	json.Unmarshal([]byte(r.Content[0].Text), &status)
	if status.FilesChanged != 0 {
		t.Errorf("filesChanged = %d, want 0 for no-match", status.FilesChanged)
	}
}

func TestApplyRenameMissingArgsIsToolError(t *testing.T) {
	s := startSession(t, polyglotFixture(t))
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/call", map[string]any{
		"name":      "apply_rename",
		"arguments": map[string]any{"name": "UserID"}, // no newName
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	if !r.IsError {
		t.Errorf("expected isError=true for missing newName, got %+v", r)
	}
}

// ---- resources ----

func TestInitializeAdvertisesResourcesCapability(t *testing.T) {
	s := startSession(t, "")
	defer s.close()
	resp := s.request("initialize", map[string]any{})
	var got struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	json.Unmarshal(resp.Result, &got)
	if got.Capabilities["resources"] == nil {
		t.Errorf("resources capability not advertised: %+v", got.Capabilities)
	}
}

func TestResourcesListReturnsCatalog(t *testing.T) {
	s := startSession(t, "")
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("resources/list", nil)
	var got struct {
		Resources []struct {
			URI         string `json:"uri"`
			Name        string `json:"name"`
			Description string `json:"description"`
			MimeType    string `json:"mimeType"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatal(err)
	}
	uris := map[string]bool{}
	for _, r := range got.Resources {
		uris[r.URI] = true
		if r.Description == "" {
			t.Errorf("resource %q has empty description", r.URI)
		}
		if r.MimeType == "" {
			t.Errorf("resource %q has empty mimeType", r.URI)
		}
	}
	for _, want := range []string{"tslsmcp://workspace", "tslsmcp://bindings"} {
		if !uris[want] {
			t.Errorf("resource %q missing from catalog", want)
		}
	}
}

func TestResourcesReadWorkspaceSummary(t *testing.T) {
	root := polyglotFixture(t)
	s := startSessionFull(t, root, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("resources/read", map[string]any{"uri": "tslsmcp://workspace"})
	var got struct {
		Contents []resourceContent `json:"contents"`
	}
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Contents) != 1 {
		t.Fatalf("got %d content blocks, want 1", len(got.Contents))
	}
	if got.Contents[0].URI != "tslsmcp://workspace" {
		t.Errorf("content URI = %q, want tslsmcp://workspace", got.Contents[0].URI)
	}
	if got.Contents[0].MimeType != "application/json" {
		t.Errorf("mimeType = %q, want application/json", got.Contents[0].MimeType)
	}
	var summary struct {
		Root      string   `json:"root"`
		Languages []string `json:"languages"`
		Names     int      `json:"names"`
		Declared  int      `json:"declared"`
	}
	if err := json.Unmarshal([]byte(got.Contents[0].Text), &summary); err != nil {
		t.Fatalf("workspace summary not JSON: %v\n%s", err, got.Contents[0].Text)
	}
	if summary.Root != root {
		t.Errorf("summary.root = %q, want %q", summary.Root, root)
	}
	if summary.Names == 0 {
		t.Error("summary.names = 0 but polyglot fixture has content")
	}
	if len(summary.Languages) == 0 {
		t.Error("summary.languages empty")
	}
}

func TestResourcesReadBindingsCatalog(t *testing.T) {
	root := polyglotFixture(t)
	schemas := []config.Schema{{File: "api.proto", Dialect: "proto"}}
	s := startSessionFull(t, root, nil, schemas)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("resources/read", map[string]any{"uri": "tslsmcp://bindings"})
	var got struct {
		Contents []resourceContent `json:"contents"`
	}
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatal(err)
	}
	var catalog []bindingSummary
	if err := json.Unmarshal([]byte(got.Contents[0].Text), &catalog); err != nil {
		t.Fatalf("bindings resource not JSON: %v\n%s", err, got.Contents[0].Text)
	}
	if len(catalog) == 0 {
		t.Fatal("expected non-empty bindings catalog with proto schema declared")
	}
	// The resource and the tool should produce the same payload. Sanity:
	// every entry must have a name + declared sites.
	for _, b := range catalog {
		if b.Name == "" {
			t.Errorf("binding with empty name: %+v", b)
		}
		if b.SiteCount == 0 {
			t.Errorf("binding %s has zero sites", b.Name)
		}
	}
}

func TestResourcesReadUnknownURIReturnsInvalidParams(t *testing.T) {
	s := startSession(t, "")
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("resources/read", map[string]any{"uri": "tslsmcp://no-such"})
	if resp.Error == nil {
		t.Fatalf("expected error for unknown resource, got %s", resp.Result)
	}
	if resp.Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602", resp.Error.Code)
	}
}

func TestSetCachePathPersistsAcrossSessions(t *testing.T) {
	// Run two MCP sessions against the same cache path. The second
	// session should load whatever the first one saved on Serve exit.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Persisted() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(dir, ".tslsmcp", "cache.gob")

	// Session 1: build + save.
	{
		reg, err := config.Default().Build()
		if err != nil {
			t.Fatal(err)
		}
		srv := New(reg, dir, nil, nil)
		srv.SetCachePath(cachePath)

		sIn, cOut := io.Pipe()
		cIn, sOut := io.Pipe()
		done := make(chan error, 1)
		go func() { done <- srv.Serve(sIn, sOut) }()

		s := &mcpSession{
			t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn),
			clientW: cOut, done: done,
		}
		s.request("initialize", map[string]any{})
		s.notify("notifications/initialized", map[string]any{})
		if srv.parseCache.Len() == 0 {
			t.Fatal("session 1: cache empty after initialize")
		}
		s.close()
	}

	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}

	// Session 2: load + verify.
	{
		reg, _ := config.Default().Build()
		srv := New(reg, dir, nil, nil)
		srv.SetCachePath(cachePath)

		sIn, cOut := io.Pipe()
		cIn, sOut := io.Pipe()
		done := make(chan error, 1)
		go func() { done <- srv.Serve(sIn, sOut) }()
		s := &mcpSession{
			t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn),
			clientW: cOut, done: done,
		}
		s.request("initialize", map[string]any{})
		s.notify("notifications/initialized", map[string]any{})
		if srv.parseCache.Len() == 0 {
			t.Error("session 2: cache empty after load (persistence not effective)")
		}
		// Sanity: tool still works.
		resp := s.request("tools/call", map[string]any{
			"name":      "find_references",
			"arguments": map[string]any{"name": "Persisted"},
		})
		var r struct {
			Content []Content `json:"content"`
		}
		json.Unmarshal(resp.Result, &r)
		var hits []siteJSON
		json.Unmarshal([]byte(r.Content[0].Text), &hits)
		if len(hits) == 0 {
			t.Error("Persisted name missing after reload")
		}
		s.close()
	}
}

func TestSetCachePathLoadMissingFileIsClean(t *testing.T) {
	// A cache path that doesn't exist yet should produce no error
	// and no spam — first-run behavior.
	dir := t.TempDir()
	cachePath := filepath.Join(dir, ".tslsmcp", "cache.gob")

	reg, _ := config.Default().Build()
	srv := New(reg, "", nil, nil) // empty root → no index build, no cache writes
	srv.SetCachePath(cachePath)

	sIn, cOut := io.Pipe()
	cIn, sOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sIn, sOut) }()
	s := &mcpSession{
		t: t, srv: srv, srvIn: cOut, clientR: json.NewDecoder(cIn),
		clientW: cOut, done: done,
	}
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	s.close()
	// Server still wrote an empty cache file on exit — that's the
	// "next session loads zero entries" path.
	if _, err := os.Stat(cachePath); err != nil {
		t.Errorf("expected cache file to be created on exit, got %v", err)
	}
}

func TestParseCachePersistsAcrossRefresh(t *testing.T) {
	// First initialize populates the cache. A subsequent refresh
	// against the same content must not add new entries — every file
	// the walker visits already has a hit in the cache.
	root := polyglotFixture(t)
	s := startSessionFull(t, root, nil, nil)
	defer s.close()

	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	afterInit := s.srv.parseCache.Len()
	if afterInit == 0 {
		t.Fatal("parseCache empty after initialize; expected entries for indexed files")
	}

	resp := s.request("tools/call", map[string]any{
		"name":      "refresh",
		"arguments": map[string]any{},
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	if r.IsError {
		t.Fatalf("refresh errored: %+v", r.Content)
	}
	afterRefresh := s.srv.parseCache.Len()
	if afterRefresh != afterInit {
		t.Errorf("parseCache grew across refresh: %d -> %d (content unchanged → no new entries expected)",
			afterInit, afterRefresh)
	}
}

func TestParseCacheAddsEntriesOnContentChange(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath,
		[]byte("package main\n\nfunc Original() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	before := s.srv.parseCache.Len()

	// Edit main.go; refresh; cache should gain exactly one entry for
	// the new content (the old entry persists too).
	if err := os.WriteFile(mainPath,
		[]byte("package main\n\nfunc Updated() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := s.request("tools/call", map[string]any{
		"name":      "refresh",
		"arguments": map[string]any{},
	})
	var r struct {
		Content []Content `json:"content"`
		IsError bool      `json:"isError"`
	}
	json.Unmarshal(resp.Result, &r)
	if r.IsError {
		t.Fatalf("refresh errored: %+v", r.Content)
	}
	after := s.srv.parseCache.Len()
	if after != before+1 {
		t.Errorf("parseCache size after content change: %d -> %d, want +1", before, after)
	}
}

func TestUnknownToolReturnsInvalidParams(t *testing.T) {
	s := startSession(t, "")
	defer s.close()

	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/call", map[string]any{
		"name":      "no_such_tool",
		"arguments": map[string]any{},
	})
	if resp.Error == nil {
		t.Errorf("expected error response, got result %s", resp.Result)
	} else if resp.Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602", resp.Error.Code)
	}
}
