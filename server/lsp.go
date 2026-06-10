package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/internal/bindings"
	"github.com/iodesystems/poly-lsp-mcp/internal/jsonrpc"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// forwardTimeout caps how long the server will wait on a child LSP
// before giving up on a forwarded request. Notifications don't use it.
const forwardTimeout = 30 * time.Second

// Minimal LSP type subset. We only declare the fields we actually read or
// write — the LSP spec is large, and tight types make it easier to know
// what we support. Anything not modelled is silently dropped on Unmarshal.

type initializeParams struct {
	RootURI          string            `json:"rootUri,omitempty"`
	RootPath         string            `json:"rootPath,omitempty"` // deprecated; fallback
	WorkspaceFolders []workspaceFolder `json:"workspaceFolders,omitempty"`
}

type workspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

type position struct {
	Line      int `json:"line"`      // 0-based
	Character int `json:"character"` // 0-based, UTF-16 in spec; we treat as bytes (ASCII OK)
}

type lspRange struct {
	Start position `json:"start"`
	End   position `json:"end"`
}

type location struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

type referenceParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     position               `json:"position"`
}

type workspaceSymbolParams struct {
	Query string `json:"query"`
}

type symbolInformation struct {
	Name     string   `json:"name"`
	Kind     int      `json:"kind"`
	Location location `json:"location"`
}

type didSaveTextDocumentParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type renameParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     position               `json:"position"`
	NewName      string                 `json:"newName"`
}

type textEdit struct {
	Range   lspRange `json:"range"`
	NewText string   `json:"newText"`
}

type workspaceEdit struct {
	Changes map[string][]textEdit `json:"changes"`
}

// LSP SymbolKind enum, only the ones we use. The protocol requires *some*
// kind, and lexical hits don't have a meaningful one — Variable is the
// least misleading catch-all until tree-sitter gives us better signal.
const symbolKindVariable = 13

// handleInitialize parses params, builds the symbol index from the
// resolved workspace root, and advertises the capabilities we serve.
func (s *Server) handleInitialize(req *jsonrpc.Message) {
	var p initializeParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			s.replyError(req, errInvalidParams, fmt.Sprintf("bad initialize params: %v", err))
			return
		}
	}

	root := pickRoot(p)
	if root != "" {
		idx, err := symbols.Build(root, s.registry, symbols.WithCache(s.parseCache))
		if err != nil {
			log.Printf("initialize: index build failed for %s: %v", root, err)
		} else {
			s.setIndex(idx)
			log.Printf("initialize: indexed %d names from %s", len(idx.Names()), root)
			// Apply declared bindings (Tier 2) then schema-anchored
			// bindings (Tier 3). Failures in any single binding/schema
			// are logged but don't abort initialization — the lexical
			// index is still useful on its own.
			resolver := bindings.NewResolver(root)
			if len(s.bindings) > 0 {
				n, err := resolver.Apply(idx, s.bindings)
				if err != nil {
					log.Printf("initialize: some bindings failed validation: %v", err)
				}
				log.Printf("initialize: applied %d declared binding site(s)", n)
			}
			if len(s.schemas) > 0 {
				n := resolver.ApplySchemas(idx, s.schemas)
				log.Printf("initialize: applied %d schema-anchored site(s)", n)
			}
			// Tier-3 auto: gat @derived(operationId) edges → declared Go-source bindings.
			if n := resolver.ApplyDerived(idx); n > 0 {
				log.Printf("initialize: applied %d @derived source binding(s)", n)
			}
		}
	} else {
		log.Print("initialize: no workspace root; symbol index disabled")
	}

	// Spawn child LSPs for every language we observed in the workspace.
	// Languages with no LSP binary in the registry, and binaries that
	// fail to start, are skipped — fallback path serves them.
	if s.manager != nil && root != "" {
		idx := s.getIndex()
		var langs []string
		if idx != nil {
			langs = idx.Languages()
		}
		if len(langs) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			rootURI := pathToURI(root)
			if err := s.manager.Start(ctx, root, rootURI, langs); err != nil {
				log.Printf("initialize: manager.Start: %v", err)
			}
		}
	}

	// Our own capabilities. These win over any child entry of the same
	// name — we're the entity actually answering for the client on the
	// methods we name here. We OWN rename because it crosses languages
	// via declared bindings; per-language child results would miss the
	// other languages.
	ourCaps := map[string]any{
		"workspaceSymbolProvider": true,
		"referencesProvider":      true,
		"documentSymbolProvider":  true,
		"renameProvider":          true,
		"textDocumentSync": map[string]any{
			"openClose": false,
			"change":    0,
			"save":      true,
		},
	}

	caps := mergeCapabilities(s.childCaps(), ourCaps)
	result := map[string]any{
		"capabilities": caps,
		"serverInfo": map[string]any{
			"name":    "poly-lsp-mcp",
			"version": "0.0.0",
		},
	}
	s.reply(req, result)
}

// childCaps returns every running child's reported ServerCapabilities,
// or nil when no manager is configured.
func (s *Server) childCaps() map[string]json.RawMessage {
	if s.manager == nil {
		return nil
	}
	return s.manager.Capabilities()
}

// mergeCapabilities unions every child's capability map and overlays
// our own (last write wins for ours). Children that disagree on a key
// settle in iteration order; that's deterministic enough for v0.1 since
// every child the manager spawned is unique per language.
func mergeCapabilities(childCaps map[string]json.RawMessage, ourCaps map[string]any) map[string]any {
	merged := map[string]any{}
	for _, raw := range childCaps {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		maps.Copy(merged, m)
	}
	maps.Copy(merged, ourCaps)
	return merged
}

// pickRoot prefers workspaceFolders[0], falls back to rootUri, then to
// the legacy rootPath. Returns "" if nothing usable was sent.
func pickRoot(p initializeParams) string {
	if len(p.WorkspaceFolders) > 0 {
		if path := uriToPath(p.WorkspaceFolders[0].URI); path != "" {
			return path
		}
	}
	if path := uriToPath(p.RootURI); path != "" {
		return path
	}
	return p.RootPath
}

// uriToPath converts a file:// URI to a filesystem path. Returns "" for
// anything that isn't a file URI or fails to parse. POSIX-only for now.
func uriToPath(rawURI string) string {
	if rawURI == "" {
		return ""
	}
	u, err := url.Parse(rawURI)
	if err != nil {
		return ""
	}
	if u.Scheme != "file" {
		return ""
	}
	return u.Path
}

// pathToURI produces a file:// URI from an absolute path. POSIX-only.
func pathToURI(path string) string {
	return "file://" + path
}

func (s *Server) handleWorkspaceSymbol(req *jsonrpc.Message) {
	var p workspaceSymbolParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			s.replyError(req, errInvalidParams, fmt.Sprintf("bad workspace/symbol params: %v", err))
			return
		}
	}

	idx := s.getIndex()
	if idx == nil {
		s.reply(req, []symbolInformation{})
		return
	}

	out := []symbolInformation{}
	query := strings.ToLower(p.Query)
	for _, name := range idx.Names() {
		if query != "" && !strings.Contains(strings.ToLower(name), query) {
			continue
		}
		for _, site := range idx.Lookup(name) {
			out = append(out, symbolInformation{
				Name: name,
				Kind: symbolKindVariable,
				Location: location{
					URI:   pathToURI(site.File),
					Range: rangeForToken(site.Line, site.Col, name),
				},
			})
		}
	}
	s.reply(req, out)
}

func (s *Server) handleReferences(req *jsonrpc.Message) {
	var p referenceParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.replyError(req, errInvalidParams, fmt.Sprintf("bad references params: %v", err))
		return
	}

	idx := s.getIndex()
	if idx == nil {
		s.reply(req, []location{})
		return
	}

	path := uriToPath(p.TextDocument.URI)
	if path == "" {
		s.reply(req, []location{})
		return
	}
	name := wordAtPosition(path, p.Position.Line, p.Position.Character)
	if name == "" {
		s.reply(req, []location{})
		return
	}

	out := []location{}
	for _, site := range idx.Lookup(name) {
		out = append(out, location{
			URI:   pathToURI(site.File),
			Range: rangeForToken(site.Line, site.Col, name),
		})
	}
	s.reply(req, out)
}

// handleRename synthesizes a cross-language WorkspaceEdit from the
// symbol index. Confidence policy: if any declared sites exist for the
// name at the cursor, rename only those (safe by user declaration).
// Otherwise fall back to every lexical hit (best effort).
//
// Aliasing protection: for each candidate site we read the file and
// confirm the text at (line, col) for len(name) bytes equals the name
// being renamed. Sites whose text doesn't match (e.g., a declared
// binding that aliases UserType to UserID's positions) are skipped so
// rename never substitutes the wrong text into a file.
func (s *Server) handleRename(req *jsonrpc.Message) {
	var p renameParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.replyError(req, errInvalidParams, fmt.Sprintf("bad rename params: %v", err))
		return
	}
	if p.NewName == "" {
		s.replyError(req, errInvalidParams, "newName is required")
		return
	}

	idx := s.getIndex()
	if idx == nil {
		s.reply(req, nil)
		return
	}

	path := uriToPath(p.TextDocument.URI)
	if path == "" {
		s.reply(req, nil)
		return
	}
	name := wordAtPosition(path, p.Position.Line, p.Position.Character)
	if name == "" {
		s.reply(req, nil)
		return
	}

	sites := chooseRenameSites(idx.Lookup(name))
	if len(sites) == 0 {
		s.reply(req, nil)
		return
	}

	edit := workspaceEdit{Changes: map[string][]textEdit{}}
	fileCache := map[string][]byte{}
	for _, site := range sites {
		if !siteTextMatches(site, name, fileCache) {
			log.Printf("rename: skip %s:%d:%d (text != %q)",
				site.File, site.Line, site.Col, name)
			continue
		}
		uri := pathToURI(site.File)
		edit.Changes[uri] = append(edit.Changes[uri], textEdit{
			Range:   rangeForToken(site.Line, site.Col, name),
			NewText: p.NewName,
		})
	}

	if len(edit.Changes) == 0 {
		s.reply(req, nil)
		return
	}
	s.reply(req, edit)
}

// chooseRenameSites implements the confidence policy: prefer declared
// sites when any are present (the user opted into precision); fall back
// to lexical only when no declarations cover this name.
func chooseRenameSites(all []symbols.Site) []symbols.Site {
	var declared, lexical []symbols.Site
	for _, s := range all {
		if s.Confidence >= symbols.ConfidenceDeclared {
			declared = append(declared, s)
		} else {
			lexical = append(lexical, s)
		}
	}
	if len(declared) > 0 {
		return declared
	}
	return lexical
}

// siteTextMatches reads the file (cached across the request) and checks
// that the bytes at (Line, Col) length len(name) equal name. Returns
// false on read errors or out-of-range coordinates — both treated as
// "don't include in the edit" rather than as errors to surface.
func siteTextMatches(site symbols.Site, name string, cache map[string][]byte) bool {
	data, ok := cache[site.File]
	if !ok {
		var err error
		data, err = os.ReadFile(site.File)
		if err != nil {
			return false
		}
		cache[site.File] = data
	}
	// Find the line we want.
	lineStart := 0
	currentLine := 1
	for currentLine < site.Line && lineStart < len(data) {
		nl := indexNewline(data[lineStart:])
		if nl < 0 {
			return false
		}
		lineStart += nl + 1
		currentLine++
	}
	if currentLine != site.Line {
		return false
	}
	nl := indexNewline(data[lineStart:])
	lineEnd := len(data)
	if nl >= 0 {
		lineEnd = lineStart + nl
	}
	line := data[lineStart:lineEnd]
	start := site.Col - 1
	end := start + len(name)
	if start < 0 || end > len(line) {
		return false
	}
	return string(line[start:end]) == name
}

func indexNewline(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}

// handleDidSave is a notification — no response. Re-extracts the file and
// refreshes its slice of the index, then forwards the notification to
// the routed child (if any) so its in-memory view stays consistent.
// Files outside the registry, or saves while we have no index, are
// silently ignored (we still forward — the child decides).
func (s *Server) handleDidSave(req *jsonrpc.Message) {
	var p didSaveTextDocumentParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		log.Printf("didSave: bad params: %v", err)
		return
	}
	if idx := s.getIndex(); idx != nil {
		path := uriToPath(p.TextDocument.URI)
		if path != "" {
			ext := strings.TrimPrefix(filepath.Ext(path), ".")
			if lang := s.registry.LookupByExt(ext); lang != nil {
				if ex := symbols.DefaultExtractor(lang.Name); ex != nil {
					data, err := os.ReadFile(path)
					if err != nil {
						log.Printf("didSave: read %s: %v", path, err)
					} else {
						idx.Refresh(path, lang.Name, ex.Extract(data))
					}
				}
			}
		}
	}
	s.forwardTextDocument(req)
}

// handleDocumentSymbol forwards to the child for the URI's language if
// one exists; otherwise falls back to our own per-file slice of the
// symbol index. The forward path is preferred because the child has
// semantic precision; the fallback ensures the method always works for
// any URI inside the workspace.
func (s *Server) handleDocumentSymbol(req *jsonrpc.Message) {
	var p struct {
		TextDocument textDocumentIdentifier `json:"textDocument"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.replyError(req, errInvalidParams, fmt.Sprintf("bad documentSymbol params: %v", err))
		return
	}

	if s.manager != nil {
		if child := s.manager.RouteByURI(p.TextDocument.URI); child != nil {
			ctx, cancel := context.WithTimeout(context.Background(), forwardTimeout)
			defer cancel()
			var passParams any
			if len(req.Params) > 0 {
				passParams = json.RawMessage(req.Params)
			}
			result, err := child.Call(ctx, req.Method, passParams)
			if err == nil {
				s.send(&jsonrpc.Message{JSONRPC: "2.0", ID: req.ID, Result: result})
				return
			}
			log.Printf("documentSymbol forward: %v; falling back to index", err)
		}
	}

	idx := s.getIndex()
	if idx == nil {
		s.reply(req, []symbolInformation{})
		return
	}
	path := uriToPath(p.TextDocument.URI)
	if path == "" {
		s.reply(req, []symbolInformation{})
		return
	}
	out := []symbolInformation{}
	for _, name := range idx.Names() {
		for _, site := range idx.Lookup(name) {
			if site.File != path {
				continue
			}
			out = append(out, symbolInformation{
				Name: name,
				Kind: symbolKindVariable,
				Location: location{
					URI:   pathToURI(site.File),
					Range: rangeForToken(site.Line, site.Col, name),
				},
			})
		}
	}
	s.reply(req, out)
}

// forwardTextDocument is the generic forwarder for textDocument/* methods
// the server doesn't intercept. Requests get the child's response (or an
// error if the child fails / no child matches); notifications are
// fire-and-forget. URIs that don't resolve to a child are answered with
// null (requests) or dropped (notifications).
func (s *Server) forwardTextDocument(req *jsonrpc.Message) {
	if s.manager == nil {
		if !req.IsNotification() {
			s.reply(req, nil)
		}
		return
	}
	uri := extractURI(req.Params)
	if uri == "" {
		if !req.IsNotification() {
			s.reply(req, nil)
		}
		return
	}
	child := s.manager.RouteByURI(uri)
	if child == nil {
		if !req.IsNotification() {
			s.reply(req, nil)
		}
		return
	}

	var passParams any
	if len(req.Params) > 0 {
		passParams = json.RawMessage(req.Params)
	}

	if req.IsNotification() {
		if err := child.Notify(req.Method, passParams); err != nil {
			log.Printf("forward %s: %v", req.Method, err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), forwardTimeout)
	defer cancel()
	result, err := child.Call(ctx, req.Method, passParams)
	if err != nil {
		s.replyError(req, errInternal, err.Error())
		return
	}
	s.send(&jsonrpc.Message{JSONRPC: "2.0", ID: req.ID, Result: result})
}

// extractURI pulls textDocument.uri out of an LSP params object. Returns
// "" if the field is missing or the JSON doesn't decode.
func extractURI(raw json.RawMessage) string {
	var p struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return p.TextDocument.URI
}

// rangeForToken converts our 1-based (line, col) plus a token name into
// the LSP 0-based range covering that token. End-character uses byte
// length — accurate for ASCII identifiers (the only kind our regex emits).
func rangeForToken(line, col int, name string) lspRange {
	startLine := line - 1
	startChar := col - 1
	return lspRange{
		Start: position{Line: startLine, Character: startChar},
		End:   position{Line: startLine, Character: startChar + len(name)},
	}
}

// wordAtPosition reads file and returns the identifier (sequence of
// [A-Za-z0-9_]) covering (line, character), or "" if the position is not
// inside one. Line and character are 0-based per LSP. Returns "" on read
// error or out-of-range coordinates — the caller treats that as "no match"
// rather than an error to surface to the client.
func wordAtPosition(file string, line, character int) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if line < 0 || line >= len(lines) {
		return ""
	}
	l := lines[line]
	if character < 0 || character > len(l) {
		return ""
	}
	start := character
	for start > 0 && isIdentByte(l[start-1]) {
		start--
	}
	end := character
	for end < len(l) && isIdentByte(l[end]) {
		end++
	}
	if start == end {
		return ""
	}
	return l[start:end]
}

func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}
