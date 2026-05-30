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

	"github.com/iodesystems/poly-lsp-mcp/internal/config"
	"github.com/iodesystems/poly-lsp-mcp/internal/jsonrpc"
)

// mcpSession drives a live MCP server through io.Pipe pairs using
// newline-delimited JSON-RPC framing.
type mcpSession struct {
	t       *testing.T
	srv     *Server
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
	if err := json.NewEncoder(s.clientW).Encode(msg); err != nil {
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
		JSONRPC: "2.0", ID: rawID, Method: method, Params: rawParams,
	})
	var resp jsonrpc.Message
	if err := s.clientR.Decode(&resp); err != nil {
		s.t.Fatalf("decode response for %s: %v", method, err)
	}
	if string(resp.ID) != string(rawID) {
		s.t.Fatalf("id mismatch on %s: sent %s got %s", method, rawID, resp.ID)
	}
	return &resp
}

func (s *mcpSession) notify(method string, params any) {
	s.t.Helper()
	var rawParams json.RawMessage
	if params != nil {
		rawParams, _ = json.Marshal(params)
	}
	s.sendMessage(&jsonrpc.Message{JSONRPC: "2.0", Method: method, Params: rawParams})
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

// callTool issues a tools/call request and returns the decoded
// content + isError flag. Most tool tests use this.
type toolResp struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError"`
}

func (s *mcpSession) callTool(name string, args any) toolResp {
	s.t.Helper()
	resp := s.request("tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	var out toolResp
	if resp.Error != nil {
		s.t.Fatalf("tools/call %s JSON-RPC error: %+v", name, resp.Error)
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		s.t.Fatalf("decode tools/call %s result: %v", name, err)
	}
	return out
}

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
			Tools     any `json:"tools"`
			Resources any `json:"resources"`
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
	if got.Capabilities.Resources == nil {
		t.Error("resources capability missing")
	}
	if got.ServerInfo.Name != "poly-lsp-mcp" {
		t.Errorf("serverInfo.name = %q, want poly-lsp-mcp", got.ServerInfo.Name)
	}
}

func TestPreInitMethodsAreRejected(t *testing.T) {
	s := startSession(t, "")
	defer func() {
		s.clientW.Close()
		<-s.done
	}()
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

func TestToolsListAdvertisesSixToolSurface(t *testing.T) {
	s := startSession(t, "")
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/list", nil)
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
	want := map[string]bool{
		"structure":       false,
		"node_references": false,
		"node_read":       false,
		"node_edit":       false,
		"node_delete":     false,
		"node_refactor":   false,
	}
	for _, tool := range got.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		} else {
			t.Errorf("unexpected tool in catalog: %q", tool.Name)
		}
		if tool.Description == "" {
			t.Errorf("tool %q has empty description", tool.Name)
		}
		if len(tool.InputSchema) == 0 {
			t.Errorf("tool %q has empty inputSchema", tool.Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %q missing from catalog", name)
		}
	}
}

// ---- structure ----

type structureEntryWire struct {
	Kind          string               `json:"kind"`
	Path          string               `json:"path"`
	Type          string               `json:"type"`
	Name          string               `json:"name"`
	StartLine     int                  `json:"startLine"`
	StartCol      int                  `json:"startCol"`
	EndLine       int                  `json:"endLine"`
	EndCol        int                  `json:"endCol"`
	NameStartLine int                  `json:"nameStartLine"`
	NameStartCol  int                  `json:"nameStartCol"`
	NameEndLine   int                  `json:"nameEndLine"`
	NameEndCol    int                  `json:"nameEndCol"`
	Children      []structureEntryWire `json:"children"`
}

func TestStructureWorkspaceListsTopLevelEntries(t *testing.T) {
	root := polyglotFixture(t)
	s := startSessionFull(t, root, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("structure", map[string]any{"path": ".", "depth": 1})
	if r.IsError {
		t.Fatalf("structure errored: %+v", r.Content)
	}
	var entry structureEntryWire
	if err := json.Unmarshal([]byte(r.Content[0].Text), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Kind != "directory" {
		t.Errorf("kind = %q, want directory", entry.Kind)
	}
	if len(entry.Children) == 0 {
		t.Fatal("empty workspace listing")
	}
	hasMain := false
	for _, c := range entry.Children {
		if c.Name == "main.go" && c.Kind == "file" {
			hasMain = true
		}
	}
	if !hasMain {
		t.Errorf("main.go missing from workspace listing: %+v", entry.Children)
	}
}

func TestStructureFileReturnsAstOutlineWithBothRanges(t *testing.T) {
	root := polyglotFixture(t)
	s := startSessionFull(t, root, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("structure", map[string]any{"path": "main.go", "depth": 1})
	if r.IsError {
		t.Fatalf("structure errored: %+v", r.Content)
	}
	var entry structureEntryWire
	if err := json.Unmarshal([]byte(r.Content[0].Text), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Kind != "file" {
		t.Errorf("kind = %q, want file", entry.Kind)
	}
	var greet *structureEntryWire
	for i := range entry.Children {
		if entry.Children[i].Name == "GreetUser" {
			greet = &entry.Children[i]
			break
		}
	}
	if greet == nil {
		t.Fatal("GreetUser missing from structure")
	}
	if greet.Kind != "node" {
		t.Errorf("GreetUser kind = %q, want node", greet.Kind)
	}
	if greet.Type != "function_declaration" {
		t.Errorf("GreetUser type = %q, want function_declaration", greet.Type)
	}
	// Both ranges must be populated and the name range must sit
	// inside the declaration range.
	if greet.StartLine < 1 || greet.EndLine < greet.StartLine {
		t.Errorf("GreetUser decl range malformed: %+v", greet)
	}
	if greet.NameStartLine < greet.StartLine || greet.NameEndLine > greet.EndLine {
		t.Errorf("GreetUser nameRange not inside declaration range: %+v", greet)
	}
}

func TestStructureFileWithoutTreeSitterGrammar(t *testing.T) {
	// Markdown has no tree-sitter grammar wired (lexical-only). The
	// fallback returns a single "text" node covering the whole file
	// so agents can node_read / node_edit / node_delete it the same
	// way they would any other node.
	root := polyglotFixture(t)
	s := startSessionFull(t, root, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("structure", map[string]any{"path": "README.md"})
	if r.IsError {
		t.Fatalf("README structure errored: %+v", r.Content)
	}
	var entry structureEntryWire
	json.Unmarshal([]byte(r.Content[0].Text), &entry)
	if entry.Kind != "file" {
		t.Errorf("kind = %q, want file", entry.Kind)
	}
	if len(entry.Children) != 1 {
		t.Fatalf("got %d children, want 1 text node: %+v", len(entry.Children), entry.Children)
	}
	textNode := entry.Children[0]
	if textNode.Kind != "node" || textNode.Type != "text" {
		t.Errorf("text fallback shape wrong: %+v", textNode)
	}
	if textNode.StartLine != 1 || textNode.StartCol != 1 {
		t.Errorf("text node should start at 1:1, got %d:%d", textNode.StartLine, textNode.StartCol)
	}
	if textNode.EndLine < textNode.StartLine {
		t.Errorf("text node range malformed: %+v", textNode)
	}
}

func TestStructureUnknownExtensionReturnsTextNode(t *testing.T) {
	// Files in extensions poly-lsp-mcp doesn't recognize (Dockerfile,
	// .toml, .env) still surface as text nodes so the agent can
	// edit them.
	dir := t.TempDir()
	path := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(path, []byte("FROM golang:1.26\nCOPY . /app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("structure", map[string]any{"path": "Dockerfile"})
	if r.IsError {
		t.Fatalf("structure errored: %+v", r.Content)
	}
	var entry structureEntryWire
	json.Unmarshal([]byte(r.Content[0].Text), &entry)
	if len(entry.Children) != 1 || entry.Children[0].Type != "text" {
		t.Errorf("expected one text node for Dockerfile, got %+v", entry.Children)
	}
	// Agent reads + edits via node_read / node_edit using that range.
	tn := entry.Children[0]
	rd := s.callTool("node_read", map[string]any{
		"file":      "Dockerfile",
		"startLine": tn.StartLine, "startCol": tn.StartCol,
		"endLine": tn.EndLine, "endCol": tn.EndCol,
	})
	var payload struct {
		Text string `json:"text"`
	}
	json.Unmarshal([]byte(rd.Content[0].Text), &payload)
	if payload.Text != "FROM golang:1.26\nCOPY . /app\n" {
		t.Errorf("text node read returned %q, want full Dockerfile body", payload.Text)
	}
}

func TestStructureRejectsNegativeDepth(t *testing.T) {
	s := startSession(t, polyglotFixture(t))
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("structure", map[string]any{"path": ".", "depth": -1})
	if !r.IsError {
		t.Errorf("expected isError on negative depth, got %+v", r)
	}
}

// ---- node_references ----

func TestNodeReferencesByIdentifierRange(t *testing.T) {
	root := polyglotFixture(t)
	s := startSessionFull(t, root, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// First: structure(main.go) → find UserID's nameRange.
	sr := s.callTool("structure", map[string]any{"path": "main.go"})
	var f structureEntryWire
	json.Unmarshal([]byte(sr.Content[0].Text), &f)
	var userID *structureEntryWire
	for i := range f.Children {
		if f.Children[i].Name == "UserID" {
			userID = &f.Children[i]
			break
		}
	}
	if userID == nil {
		t.Fatal("UserID not in main.go structure")
	}

	r := s.callTool("node_references", map[string]any{
		"file":      "main.go",
		"startLine": userID.NameStartLine,
		"startCol":  userID.NameStartCol,
		"endLine":   userID.NameEndLine,
		"endCol":    userID.NameEndCol,
	})
	if r.IsError {
		t.Fatalf("node_references errored: %+v", r.Content)
	}
	var hits []siteJSON
	json.Unmarshal([]byte(r.Content[0].Text), &hits)
	if len(hits) < 5 {
		t.Errorf("got %d UserID refs, want >= 5 across polyglot", len(hits))
	}
	for _, h := range hits {
		if h.Name != "UserID" {
			t.Errorf("hit name = %q, want UserID", h.Name)
		}
	}
}

func TestNodeReferencesIncludesAtRefMarker(t *testing.T) {
	// Comment markers in a .go file: tree-sitter doesn't index inside
	// comments, so without the universal scanner the .go file would
	// have zero hits for these names. The scanner adds:
	//   @see Foo  → comment-confidence site
	//   @ref Bar  → declared-confidence site
	// Both should surface via node_references when queried from the
	// peer file that defines the target name.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\n// @see TsHelper for the frontend impl.\n// @ref types.ts:SharedType\nfunc DoStuff() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "types.ts"),
		[]byte("export const TsHelper = 1;\nexport type SharedType = string;\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// Find TsHelper's range in types.ts to seed node_references.
	sr := s.callTool("structure", map[string]any{"path": "types.ts"})
	var f structureEntryWire
	json.Unmarshal([]byte(sr.Content[0].Text), &f)
	var tsHelper *structureEntryWire
	for i := range f.Children {
		if f.Children[i].Name == "TsHelper" {
			tsHelper = &f.Children[i]
			break
		}
	}
	if tsHelper == nil {
		t.Fatal("TsHelper not in types.ts structure")
	}

	r := s.callTool("node_references", map[string]any{
		"file":      "types.ts",
		"startLine": tsHelper.NameStartLine,
		"startCol":  tsHelper.NameStartCol,
		"endLine":   tsHelper.NameEndLine,
		"endCol":    tsHelper.NameEndCol,
	})
	if r.IsError {
		t.Fatalf("node_references TsHelper errored: %+v", r.Content)
	}
	var hits []siteJSON
	json.Unmarshal([]byte(r.Content[0].Text), &hits)
	var sawCommentGo bool
	for _, h := range hits {
		if h.File == "main.go" && h.Confidence == "comment" {
			sawCommentGo = true
		}
	}
	if !sawCommentGo {
		t.Errorf("expected comment-confidence hit in main.go for TsHelper (from @see); hits=%+v", hits)
	}

	// SharedType: only in main.go via @ref + in types.ts as the
	// declaration. The @ref site must show up as declared.
	sr2 := s.callTool("structure", map[string]any{"path": "types.ts"})
	json.Unmarshal([]byte(sr2.Content[0].Text), &f)
	var shared *structureEntryWire
	for i := range f.Children {
		if f.Children[i].Name == "SharedType" {
			shared = &f.Children[i]
			break
		}
	}
	if shared == nil {
		t.Fatal("SharedType not in types.ts structure")
	}
	r2 := s.callTool("node_references", map[string]any{
		"file":      "types.ts",
		"startLine": shared.NameStartLine,
		"startCol":  shared.NameStartCol,
		"endLine":   shared.NameEndLine,
		"endCol":    shared.NameEndCol,
	})
	if r2.IsError {
		t.Fatalf("node_references SharedType errored: %+v", r2.Content)
	}
	var hits2 []siteJSON
	json.Unmarshal([]byte(r2.Content[0].Text), &hits2)
	var sawDeclaredGo bool
	for _, h := range hits2 {
		if h.File == "main.go" && h.Confidence == "declared" {
			sawDeclaredGo = true
		}
	}
	if !sawDeclaredGo {
		t.Errorf("expected declared-confidence hit in main.go for SharedType (from @ref); hits=%+v", hits2)
	}
}

func TestNodeReferencesEmptyRangeIsError(t *testing.T) {
	s := startSession(t, polyglotFixture(t))
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// Zero-width range (same start and end position) — empty text.
	r := s.callTool("node_references", map[string]any{
		"file":      "main.go",
		"startLine": 1, "startCol": 1,
		"endLine": 1, "endCol": 1,
	})
	if !r.IsError {
		t.Errorf("expected isError on zero-width range, got %+v", r)
	}
}

// ---- node_read / node_edit / node_delete ----

func TestNodeReadReturnsExactText(t *testing.T) {
	dir := t.TempDir()
	body := "package main\n\nfunc Foo() {\n\treturn\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(body), 0o644); err != nil {
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

	r := s.callTool("node_read", map[string]any{
		"file":      "main.go",
		"startLine": 3, "startCol": 1,
		"endLine": 5, "endCol": 2,
	})
	if r.IsError {
		t.Fatalf("node_read errored: %+v", r.Content)
	}
	var payload struct {
		Text string `json:"text"`
	}
	json.Unmarshal([]byte(r.Content[0].Text), &payload)
	want := "func Foo() {\n\treturn\n}"
	if payload.Text != want {
		t.Errorf("text = %q, want %q", payload.Text, want)
	}
}

func TestNodeEditRewritesFileAtomicallyAndRefreshesIndex(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath,
		[]byte("package main\n\nfunc Original() {}\n"), 0o755); err != nil {
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

	// Sanity: Original is in the index (via structure → references roundtrip).
	sr := s.callTool("structure", map[string]any{"path": "main.go"})
	var f structureEntryWire
	json.Unmarshal([]byte(sr.Content[0].Text), &f)
	var orig *structureEntryWire
	for i := range f.Children {
		if f.Children[i].Name == "Original" {
			orig = &f.Children[i]
		}
	}
	if orig == nil {
		t.Fatal("Original missing from initial structure")
	}

	// Rewrite the function — note rangeArgs uses the DECL range,
	// not nameRange, for whole-node edits.
	r := s.callTool("node_edit", map[string]any{
		"file":      "main.go",
		"startLine": orig.StartLine, "startCol": orig.StartCol,
		"endLine": orig.EndLine, "endCol": orig.EndCol,
		"newText": "func Updated() {}",
	})
	if r.IsError {
		t.Fatalf("node_edit errored: %+v", r.Content)
	}

	got, _ := os.ReadFile(mainPath)
	if !strings.Contains(string(got), "Updated") {
		t.Errorf("file after edit missing Updated:\n%s", got)
	}
	if strings.Contains(string(got), "Original") {
		t.Errorf("file still contains Original:\n%s", got)
	}

	// Mode preserved.
	info, _ := os.Stat(mainPath)
	if info.Mode().Perm() != 0o755 {
		t.Errorf("file mode = %o, want 0755", info.Mode().Perm())
	}

	// Index auto-refreshed: structure now shows Updated, not Original.
	sr = s.callTool("structure", map[string]any{"path": "main.go"})
	json.Unmarshal([]byte(sr.Content[0].Text), &f)
	names := map[string]bool{}
	for _, c := range f.Children {
		names[c.Name] = true
	}
	if !names["Updated"] || names["Original"] {
		t.Errorf("structure after edit didn't refresh: %+v", names)
	}

	// No multiplex manager attached in this session: the response
	// must signal diagnosticsAvailable=false (and never silently
	// claim "no errors").
	var payload struct {
		DiagnosticsAvailable bool `json:"diagnosticsAvailable"`
		DiagnosticsTimedOut  bool `json:"diagnosticsTimedOut"`
		Diagnostics          []struct {
			Message string `json:"message"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal([]byte(r.Content[0].Text), &payload); err != nil {
		t.Fatalf("decode node_edit response: %v", err)
	}
	if payload.DiagnosticsAvailable {
		t.Errorf("diagnosticsAvailable=true without a manager; want false")
	}
	if len(payload.Diagnostics) != 0 {
		t.Errorf("diagnostics non-empty without manager: %+v", payload.Diagnostics)
	}
}

func TestNodeDeleteRemovesRange(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath, []byte("AXB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_delete", map[string]any{
		"file":      "main.go",
		"startLine": 1, "startCol": 2,
		"endLine": 1, "endCol": 3,
	})
	if r.IsError {
		t.Fatalf("node_delete errored: %+v", r.Content)
	}
	got, _ := os.ReadFile(mainPath)
	if string(got) != "AB\n" {
		t.Errorf("after delete = %q, want %q", got, "AB\n")
	}
}

// ---- node_refactor ----

type refactorResult struct {
	Kind         string        `json:"kind"`
	OldName      string        `json:"oldName"`
	NewName      string        `json:"newName"`
	FilesChanged int           `json:"filesChanged"`
	Results      []applyResult `json:"results"`
}

func TestNodeRefactorRenameAcrossLanguages(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.go")
	helperPath := filepath.Join(dir, "helper.go")
	if err := os.WriteFile(mainPath,
		[]byte("package main\n\ntype UserID int\n\nfunc f(id UserID) {}\n"), 0o644); err != nil {
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

	// Get UserID's nameRange via structure.
	sr := s.callTool("structure", map[string]any{"path": "main.go"})
	var f structureEntryWire
	json.Unmarshal([]byte(sr.Content[0].Text), &f)
	var typ *structureEntryWire
	for i := range f.Children {
		if f.Children[i].Name == "UserID" {
			typ = &f.Children[i]
		}
	}
	if typ == nil {
		t.Fatal("UserID not in structure")
	}

	r := s.callTool("node_refactor", map[string]any{
		"file":      "main.go",
		"startLine": typ.NameStartLine, "startCol": typ.NameStartCol,
		"endLine": typ.NameEndLine, "endCol": typ.NameEndCol,
		"kind":    "rename",
		"newName": "PersonID",
	})
	if r.IsError {
		t.Fatalf("node_refactor errored: %+v", r.Content)
	}
	var result refactorResult
	json.Unmarshal([]byte(r.Content[0].Text), &result)
	if result.Kind != "rename" || result.OldName != "UserID" || result.NewName != "PersonID" {
		t.Errorf("result header wrong: %+v", result)
	}
	if result.FilesChanged != 2 {
		t.Errorf("filesChanged = %d, want 2", result.FilesChanged)
	}
	// Files on disk reflect the rename.
	mainAfter, _ := os.ReadFile(mainPath)
	helperAfter, _ := os.ReadFile(helperPath)
	if strings.Contains(string(mainAfter), "UserID") || strings.Contains(string(helperAfter), "UserID") {
		t.Errorf("UserID still present:\nmain.go: %s\nhelper.go: %s", mainAfter, helperAfter)
	}
}

func TestNodeRefactorMissingKindIsError(t *testing.T) {
	s := startSession(t, polyglotFixture(t))
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_refactor", map[string]any{
		"file":      "main.go",
		"startLine": 6, "startCol": 6,
		"endLine": 6, "endCol": 12,
		// no kind and no refactor
	})
	if !r.IsError {
		t.Errorf("expected isError on missing kind/refactor, got %+v", r)
	}
}

// TestNodeRefactorNestedShapeRenameIsEquivalent verifies the new
// refactor:{rename: ...} shape produces the same result as the
// legacy kind=rename, newName=... shape.
func TestNodeRefactorNestedShapeRenameIsEquivalent(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath,
		[]byte("package main\n\ntype UserID int\n\nvar u UserID = 1\n"), 0o644); err != nil {
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

	sr := s.callTool("structure", map[string]any{"path": "main.go"})
	var f structureEntryWire
	json.Unmarshal([]byte(sr.Content[0].Text), &f)
	var typ *structureEntryWire
	for i := range f.Children {
		if f.Children[i].Name == "UserID" {
			typ = &f.Children[i]
			break
		}
	}
	if typ == nil {
		t.Fatal("UserID missing from structure")
	}

	r := s.callTool("node_refactor", map[string]any{
		"file":      "main.go",
		"startLine": typ.NameStartLine, "startCol": typ.NameStartCol,
		"endLine": typ.NameEndLine, "endCol": typ.NameEndCol,
		"refactor": map[string]any{
			"rename": "PersonID",
		},
	})
	if r.IsError {
		t.Fatalf("nested-shape rename errored: %+v", r.Content)
	}
	var result refactorResult
	json.Unmarshal([]byte(r.Content[0].Text), &result)
	if result.OldName != "UserID" || result.NewName != "PersonID" {
		t.Errorf("result header wrong: %+v", result)
	}
	got, _ := os.ReadFile(mainPath)
	if strings.Contains(string(got), "UserID") {
		t.Errorf("UserID still present after nested-shape rename: %s", got)
	}
	if !strings.Contains(string(got), "PersonID") {
		t.Errorf("PersonID missing after nested-shape rename: %s", got)
	}
}

// TestNodeRefactorConflictingShapesIsError makes sure callers don't
// pass kind=rename, newName=X AND refactor:{rename: Y} with disagreeing
// names — that ambiguity is rejected.
func TestNodeRefactorConflictingShapesIsError(t *testing.T) {
	s := startSession(t, polyglotFixture(t))
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_refactor", map[string]any{
		"file":      "main.go",
		"startLine": 6, "startCol": 6,
		"endLine": 6, "endCol": 12,
		"kind":     "rename",
		"newName":  "X",
		"refactor": map[string]any{"rename": "Y"},
	})
	if !r.IsError {
		t.Errorf("expected isError on conflicting names, got %+v", r)
	}
}

// TestNodeRefactorSignatureChangeParams rewrites a Go function's
// parameter list via the nested refactor shape and verifies the
// declaration on disk reflects the new params.
func TestNodeRefactorSignatureChangeParams(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.go")
	src := "package main\n\nfunc Greet(name string) string {\n\treturn name\n}\n"
	if err := os.WriteFile(mainPath, []byte(src), 0o644); err != nil {
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

	sr := s.callTool("structure", map[string]any{"path": "main.go"})
	var f structureEntryWire
	json.Unmarshal([]byte(sr.Content[0].Text), &f)
	var fn *structureEntryWire
	for i := range f.Children {
		if f.Children[i].Name == "Greet" {
			fn = &f.Children[i]
			break
		}
	}
	if fn == nil {
		t.Fatal("Greet missing from structure")
	}

	r := s.callTool("node_refactor", map[string]any{
		"file":      "main.go",
		"startLine": fn.NameStartLine, "startCol": fn.NameStartCol,
		"endLine": fn.NameEndLine, "endCol": fn.NameEndCol,
		"refactor": map[string]any{
			"params": []map[string]any{
				{"name": "name", "type": "string"},
				{"name": "age", "type": "int"},
			},
		},
	})
	if r.IsError {
		t.Fatalf("signature refactor errored: %+v", r.Content)
	}
	got, _ := os.ReadFile(mainPath)
	if !strings.Contains(string(got), "func Greet(name string, age int) string") {
		t.Errorf("declaration not rewritten:\n%s", got)
	}
}

// TestNodeRefactorSignatureChangeReturn rewrites the return type.
// Covers both the "replace existing result" and "insert when void"
// branches.
func TestNodeRefactorSignatureChangeReturn(t *testing.T) {
	t.Run("replace-existing", func(t *testing.T) {
		dir := t.TempDir()
		mainPath := filepath.Join(dir, "main.go")
		if err := os.WriteFile(mainPath,
			[]byte("package main\n\nfunc Greet() string {\n\treturn \"hi\"\n}\n"), 0o644); err != nil {
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

		r := s.callTool("node_refactor", map[string]any{
			"file":      "main.go",
			"startLine": 3, "startCol": 6,
			"endLine": 3, "endCol": 11,
			"refactor": map[string]any{"return": "(string, error)"},
		})
		if r.IsError {
			t.Fatalf("errored: %+v", r.Content)
		}
		got, _ := os.ReadFile(mainPath)
		if !strings.Contains(string(got), "func Greet() (string, error)") {
			t.Errorf("return type not rewritten:\n%s", got)
		}
	})

	t.Run("insert-into-void", func(t *testing.T) {
		dir := t.TempDir()
		mainPath := filepath.Join(dir, "main.go")
		if err := os.WriteFile(mainPath,
			[]byte("package main\n\nfunc Greet() {\n\t_ = \"hi\"\n}\n"), 0o644); err != nil {
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

		r := s.callTool("node_refactor", map[string]any{
			"file":      "main.go",
			"startLine": 3, "startCol": 6,
			"endLine": 3, "endCol": 11,
			"refactor": map[string]any{"return": "error"},
		})
		if r.IsError {
			t.Fatalf("errored: %+v", r.Content)
		}
		got, _ := os.ReadFile(mainPath)
		if !strings.Contains(string(got), "func Greet() error {") {
			t.Errorf("void → typed return not inserted correctly:\n%s", got)
		}
	})
}

// TestNodeRefactorSignatureCallSiteAddArg adds a parameter to a
// function that's called from a sibling file. The call site should
// be rewritten with a zero-value placeholder for the new arg.
func TestNodeRefactorSignatureCallSiteAddArg(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lib.go"),
		[]byte("package main\n\nfunc Greet(name string) string {\n\treturn name\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "caller.go"),
		[]byte("package main\n\nfunc init() {\n\t_ = Greet(\"hi\")\n\t_ = Greet(\"there\")\n}\n"), 0o644); err != nil {
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

	r := s.callTool("node_refactor", map[string]any{
		"file":      "lib.go",
		"startLine": 3, "startCol": 6,
		"endLine": 3, "endCol": 11,
		"refactor": map[string]any{
			"params": []map[string]any{
				{"name": "name", "type": "string"},
				{"name": "count", "type": "int"},
			},
		},
	})
	if r.IsError {
		t.Fatalf("errored: %+v", r.Content)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "caller.go"))
	if !strings.Contains(string(got), `Greet("hi", 0)`) {
		t.Errorf("expected first call rewritten to Greet(\"hi\", 0); got:\n%s", got)
	}
	if !strings.Contains(string(got), `Greet("there", 0)`) {
		t.Errorf("expected second call rewritten to Greet(\"there\", 0); got:\n%s", got)
	}
}

// TestNodeRefactorSignatureCallSiteDropArg removes a parameter; call
// sites drop their trailing arg accordingly.
func TestNodeRefactorSignatureCallSiteDropArg(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lib.go"),
		[]byte("package main\n\nfunc Greet(name string, count int) string {\n\treturn name\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "caller.go"),
		[]byte("package main\n\nfunc init() {\n\t_ = Greet(\"hi\", 3)\n}\n"), 0o644); err != nil {
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

	r := s.callTool("node_refactor", map[string]any{
		"file":      "lib.go",
		"startLine": 3, "startCol": 6,
		"endLine": 3, "endCol": 11,
		"refactor": map[string]any{
			"params": []map[string]any{
				{"name": "name", "type": "string"},
			},
		},
	})
	if r.IsError {
		t.Fatalf("errored: %+v", r.Content)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "caller.go"))
	if !strings.Contains(string(got), `Greet("hi")`) {
		t.Errorf("expected trailing arg dropped: %s", got)
	}
	if strings.Contains(string(got), `, 3`) {
		t.Errorf("dropped arg still present: %s", got)
	}
}

// TestNodeRefactorSignatureCombinedRename rewrites params AND renames
// the function in the same call. The rename should also touch any
// callers in the workspace (here: another file using the function).
func TestNodeRefactorSignatureCombinedRename(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Greet(name string) string {\n\treturn name\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "caller.go"),
		[]byte("package main\n\nfunc init() {\n\t_ = Greet(\"world\")\n}\n"), 0o644); err != nil {
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

	r := s.callTool("node_refactor", map[string]any{
		"file":      "main.go",
		"startLine": 3, "startCol": 6,
		"endLine": 3, "endCol": 11,
		"refactor": map[string]any{
			"rename": "Hello",
			"params": []map[string]any{
				{"name": "name", "type": "string"},
				{"name": "age", "type": "int"},
			},
		},
	})
	if r.IsError {
		t.Fatalf("combined refactor errored: %+v", r.Content)
	}
	mainGot, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	callerGot, _ := os.ReadFile(filepath.Join(dir, "caller.go"))
	if !strings.Contains(string(mainGot), "func Hello(name string, age int) string") {
		t.Errorf("main.go declaration wrong:\n%s", mainGot)
	}
	// Call-site rewriting padded the second arg with the int zero
	// value. Best-effort: the agent might tweak it after seeing
	// diagnostics, but the call compiles and gopls can type-check.
	if !strings.Contains(string(callerGot), `Hello("world", 0)`) {
		t.Errorf("caller.go expected Hello(\"world\", 0) after rename + param-add; got:\n%s", callerGot)
	}
}

// TestNodeRefactorTSSignature exercises the TypeScript path: rewrite
// a function declaration's params + return type via the same nested
// refactor shape, plus call-site rewriting in a sibling file.
func TestNodeRefactorTSSignature(t *testing.T) {
	dir := t.TempDir()
	libPath := filepath.Join(dir, "lib.ts")
	if err := os.WriteFile(libPath,
		[]byte("export function greet(name: string): string {\n\treturn name;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	callerPath := filepath.Join(dir, "caller.ts")
	if err := os.WriteFile(callerPath,
		[]byte("import {greet} from \"./lib\";\nconst out = greet(\"hi\");\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// greet's name starts at line 1 col 17 (after "export function ").
	r := s.callTool("node_refactor", map[string]any{
		"file":      "lib.ts",
		"startLine": 1, "startCol": 17,
		"endLine": 1, "endCol": 22,
		"refactor": map[string]any{
			"params": []map[string]any{
				{"name": "name", "type": "string"},
				{"name": "age", "type": "number"},
			},
			"return": "string",
		},
	})
	if r.IsError {
		t.Fatalf("TS signature refactor errored: %+v", r.Content)
	}
	libGot, _ := os.ReadFile(libPath)
	callerGot, _ := os.ReadFile(callerPath)
	if !strings.Contains(string(libGot), "function greet(name: string, age: number): string {") {
		t.Errorf("lib.ts declaration wrong:\n%s", libGot)
	}
	if !strings.Contains(string(callerGot), `greet("hi", 0)`) {
		t.Errorf("caller.ts call-site padding wrong; got:\n%s", callerGot)
	}
}

// TestNodeRefactorPythonSignature exercises Python: rewrite params +
// return type. Python uses `-> T:` for the return type, and the test
// covers both inserting one (untyped → typed) and call-site padding.
func TestNodeRefactorPythonSignature(t *testing.T) {
	dir := t.TempDir()
	libPath := filepath.Join(dir, "lib.py")
	if err := os.WriteFile(libPath,
		[]byte("def greet(name):\n    return name\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	callerPath := filepath.Join(dir, "caller.py")
	if err := os.WriteFile(callerPath,
		[]byte("from lib import greet\nprint(greet(\"hi\"))\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	// greet's name starts at line 1 col 5 (`def greet(`).
	r := s.callTool("node_refactor", map[string]any{
		"file":      "lib.py",
		"startLine": 1, "startCol": 5,
		"endLine": 1, "endCol": 10,
		"refactor": map[string]any{
			"params": []map[string]any{
				{"name": "name", "type": "str"},
				{"name": "items", "type": "list"},
			},
			"return": "str",
		},
	})
	if r.IsError {
		t.Fatalf("Python signature refactor errored: %+v", r.Content)
	}
	libGot, _ := os.ReadFile(libPath)
	callerGot, _ := os.ReadFile(callerPath)
	if !strings.Contains(string(libGot), "def greet(name: str, items: list) -> str:") {
		t.Errorf("lib.py declaration wrong:\n%s", libGot)
	}
	if !strings.Contains(string(callerGot), `greet("hi", [])`) {
		t.Errorf("caller.py call-site padding wrong; got:\n%s", callerGot)
	}
}

func TestNodeRefactorUnknownKindIsError(t *testing.T) {
	s := startSession(t, polyglotFixture(t))
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	r := s.callTool("node_refactor", map[string]any{
		"file":      "main.go",
		"startLine": 6, "startCol": 6,
		"endLine": 6, "endCol": 12,
		"kind": "make_better",
	})
	if !r.IsError {
		t.Errorf("expected isError on unknown kind, got %+v", r)
	}
}

func TestNodeRefactorRenameDefaultSkipsComments(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath,
		[]byte("package main\n\n// UserID is the canonical id\ntype UserID int\n"), 0o644); err != nil {
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

	sr := s.callTool("structure", map[string]any{"path": "main.go"})
	var f structureEntryWire
	json.Unmarshal([]byte(sr.Content[0].Text), &f)
	var typ *structureEntryWire
	for i := range f.Children {
		if f.Children[i].Name == "UserID" {
			typ = &f.Children[i]
		}
	}
	if typ == nil {
		t.Fatal("UserID not in structure")
	}

	r := s.callTool("node_refactor", map[string]any{
		"file":      "main.go",
		"startLine": typ.NameStartLine, "startCol": typ.NameStartCol,
		"endLine": typ.NameEndLine, "endCol": typ.NameEndCol,
		"kind":    "rename",
		"newName": "PersonID",
	})
	if r.IsError {
		t.Fatalf("rename errored: %+v", r.Content)
	}
	got, _ := os.ReadFile(mainPath)
	// Default: the comment is preserved (still says UserID).
	if !strings.Contains(string(got), "// UserID is the canonical id") {
		t.Errorf("comment was rewritten without includeComments:\n%s", got)
	}
	if !strings.Contains(string(got), "type PersonID int") {
		t.Errorf("type declaration not renamed:\n%s", got)
	}
}

func TestNodeRefactorRenameIncludeCommentsTouchesComments(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath,
		[]byte("package main\n\n// UserID is the canonical id\n// thisUserID stays as-is (partial word)\ntype UserID int\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A README in markdown — also has a UserID reference in prose.
	readmePath := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmePath,
		[]byte("# polyglot\n\nThe `UserID` identifier crosses languages.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := startSessionFull(t, dir, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	sr := s.callTool("structure", map[string]any{"path": "main.go"})
	var f structureEntryWire
	json.Unmarshal([]byte(sr.Content[0].Text), &f)
	var typ *structureEntryWire
	for i := range f.Children {
		if f.Children[i].Name == "UserID" {
			typ = &f.Children[i]
		}
	}
	if typ == nil {
		t.Fatal("UserID not in structure")
	}

	r := s.callTool("node_refactor", map[string]any{
		"file":            "main.go",
		"startLine":       typ.NameStartLine,
		"startCol":        typ.NameStartCol,
		"endLine":         typ.NameEndLine,
		"endCol":          typ.NameEndCol,
		"kind":            "rename",
		"newName":         "PersonID",
		"includeComments": true,
	})
	if r.IsError {
		t.Fatalf("rename errored: %+v", r.Content)
	}

	main, _ := os.ReadFile(mainPath)
	readme, _ := os.ReadFile(readmePath)

	if !strings.Contains(string(main), "// PersonID is the canonical id") {
		t.Errorf("comment line not renamed:\n%s", main)
	}
	// Partial-word match MUST be preserved.
	if !strings.Contains(string(main), "thisUserID") {
		t.Errorf("partial-word match was wrongly renamed:\n%s", main)
	}
	if strings.Contains(string(main), "thisPersonID") {
		t.Errorf("partial-word match was wrongly renamed:\n%s", main)
	}
	if !strings.Contains(string(readme), "The `PersonID` identifier") {
		t.Errorf("markdown prose not renamed under includeComments:\n%s", readme)
	}
}

func TestNodeRefactorRenameIncludeCommentsDedupesWithDeclaredSites(t *testing.T) {
	// Declared sites already include the type declaration. The
	// comment-scan must not produce a duplicate edit at that
	// position (which would corrupt the file by replacing the same
	// bytes twice).
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath,
		[]byte("package main\n\n// UserID note\ntype UserID int\n"), 0o644); err != nil {
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

	sr := s.callTool("structure", map[string]any{"path": "main.go"})
	var f structureEntryWire
	json.Unmarshal([]byte(sr.Content[0].Text), &f)
	var typ *structureEntryWire
	for i := range f.Children {
		if f.Children[i].Name == "UserID" {
			typ = &f.Children[i]
		}
	}

	r := s.callTool("node_refactor", map[string]any{
		"file":            "main.go",
		"startLine":       typ.NameStartLine,
		"startCol":        typ.NameStartCol,
		"endLine":         typ.NameEndLine,
		"endCol":          typ.NameEndCol,
		"kind":            "rename",
		"newName":         "PersonID",
		"includeComments": true,
	})
	if r.IsError {
		t.Fatalf("rename errored: %+v", r.Content)
	}
	got, _ := os.ReadFile(mainPath)
	want := "package main\n\n// PersonID note\ntype PersonID int\n"
	if string(got) != want {
		t.Errorf("rename + includeComments produced unexpected content:\nGOT:\n%s\nWANT:\n%s", got, want)
	}
}

// ---- resources (unchanged surface) ----

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
	}
	for _, want := range []string{"poly-lsp-mcp://workspace", "poly-lsp-mcp://bindings", "poly-lsp-mcp://diagnostics"} {
		if !uris[want] {
			t.Errorf("resource %q missing from catalog", want)
		}
	}
}

// poly-lsp-mcp://diagnostics with no manager attached: diagnosticsAvailable
// must be false. The agent must NOT infer "compiles clean" from an
// empty list when no LSP is talking to us.
func TestResourcesReadDiagnosticsWithoutManager(t *testing.T) {
	root := polyglotFixture(t)
	s := startSessionFull(t, root, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("resources/read", map[string]any{"uri": "poly-lsp-mcp://diagnostics"})
	var got struct {
		Contents []resourceContent `json:"contents"`
	}
	json.Unmarshal(resp.Result, &got)
	if len(got.Contents) != 1 {
		t.Fatalf("got %d contents, want 1", len(got.Contents))
	}
	var payload struct {
		DiagnosticsAvailable bool             `json:"diagnosticsAvailable"`
		Languages            []string         `json:"languages"`
		Diagnostics          []map[string]any `json:"diagnostics"`
	}
	if err := json.Unmarshal([]byte(got.Contents[0].Text), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.DiagnosticsAvailable {
		t.Errorf("diagnosticsAvailable=true without a manager; want false")
	}
	if len(payload.Diagnostics) != 0 {
		t.Errorf("diagnostics non-empty without manager: %+v", payload.Diagnostics)
	}
}

func TestResourcesReadWorkspaceSummary(t *testing.T) {
	root := polyglotFixture(t)
	s := startSessionFull(t, root, nil, nil)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("resources/read", map[string]any{"uri": "poly-lsp-mcp://workspace"})
	var got struct {
		Contents []resourceContent `json:"contents"`
	}
	json.Unmarshal(resp.Result, &got)
	if len(got.Contents) != 1 {
		t.Fatalf("got %d contents, want 1", len(got.Contents))
	}
	var summary struct {
		Root      string   `json:"root"`
		Languages []string `json:"languages"`
		Names     int      `json:"names"`
	}
	json.Unmarshal([]byte(got.Contents[0].Text), &summary)
	if summary.Root != root {
		t.Errorf("root = %q, want %q", summary.Root, root)
	}
	if summary.Names == 0 || len(summary.Languages) == 0 {
		t.Errorf("expected non-zero names + languages: %+v", summary)
	}
}

func TestResourcesReadBindingsCatalogWithSchemas(t *testing.T) {
	root := polyglotFixture(t)
	schemas := []config.Schema{{File: "api.proto", Dialect: "proto"}}
	s := startSessionFull(t, root, nil, schemas)
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("resources/read", map[string]any{"uri": "poly-lsp-mcp://bindings"})
	var got struct {
		Contents []resourceContent `json:"contents"`
	}
	json.Unmarshal(resp.Result, &got)
	var catalog []bindingSummary
	json.Unmarshal([]byte(got.Contents[0].Text), &catalog)
	if len(catalog) == 0 {
		t.Fatal("expected catalog entries with proto schema declared")
	}
}

func TestResourcesReadUnknownURIReturnsInvalidParams(t *testing.T) {
	s := startSession(t, "")
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("resources/read", map[string]any{"uri": "poly-lsp-mcp://no-such"})
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("error = %+v, want -32602", resp.Error)
	}
}

// ---- cache persistence ----

func TestSetCachePathPersistsAcrossSessions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Persisted() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module x\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(dir, ".poly-lsp-mcp", "cache.gob")

	// Session 1.
	{
		reg, _ := config.Default().Build()
		srv := New(reg, dir, nil, nil)
		srv.SetCachePath(cachePath)
		sIn, cOut := io.Pipe()
		cIn, sOut := io.Pipe()
		done := make(chan error, 1)
		go func() { done <- srv.Serve(sIn, sOut) }()
		s := &mcpSession{
			t: t, srv: srv, srvIn: cOut,
			clientR: json.NewDecoder(cIn), clientW: cOut, done: done,
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
	// Session 2.
	{
		reg, _ := config.Default().Build()
		srv := New(reg, dir, nil, nil)
		srv.SetCachePath(cachePath)
		sIn, cOut := io.Pipe()
		cIn, sOut := io.Pipe()
		done := make(chan error, 1)
		go func() { done <- srv.Serve(sIn, sOut) }()
		s := &mcpSession{
			t: t, srv: srv, srvIn: cOut,
			clientR: json.NewDecoder(cIn), clientW: cOut, done: done,
		}
		s.request("initialize", map[string]any{})
		s.notify("notifications/initialized", map[string]any{})
		if srv.parseCache.Len() == 0 {
			t.Error("session 2: cache empty after load (persistence not effective)")
		}
		s.close()
	}
}

// ---- generic error paths ----

func TestUnknownToolReturnsInvalidParams(t *testing.T) {
	s := startSession(t, "")
	defer s.close()
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})

	resp := s.request("tools/call", map[string]any{
		"name":      "no_such_tool",
		"arguments": map[string]any{},
	})
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602, got %+v", resp.Error)
	}
}
