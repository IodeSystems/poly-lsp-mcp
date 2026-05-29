package mcp

import (
	"encoding/json"
	"errors"
	"io"
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
	for _, want := range []string{"find_symbol", "find_references", "rename", "list_bindings", "document_symbols"} {
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
