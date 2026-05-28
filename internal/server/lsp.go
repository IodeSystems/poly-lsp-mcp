package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/iodesystems/tslsmcp/internal/jsonrpc"
	"github.com/iodesystems/tslsmcp/internal/symbols"
)

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
			s.replyError(req, -32602, fmt.Sprintf("bad initialize params: %v", err))
			return
		}
	}

	root := pickRoot(p)
	if root != "" {
		idx, err := symbols.Build(root, s.registry)
		if err != nil {
			log.Printf("initialize: index build failed for %s: %v", root, err)
		} else {
			s.setIndex(idx)
			log.Printf("initialize: indexed %d names from %s", len(idx.Names()), root)
		}
	} else {
		log.Print("initialize: no workspace root; symbol index disabled")
	}

	result := map[string]any{
		"capabilities": map[string]any{
			"workspaceSymbolProvider": true,
			"referencesProvider":      true,
			// Sync=None means we don't ask for didChange streams; the
			// index is rebuilt from disk on didSave only.
			"textDocumentSync": map[string]any{
				"openClose": false,
				"change":    0,
				"save":      true,
			},
		},
		"serverInfo": map[string]any{
			"name":    "tslsmcp",
			"version": "0.0.0",
		},
	}
	s.reply(req, result)
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
			s.replyError(req, -32602, fmt.Sprintf("bad workspace/symbol params: %v", err))
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
		s.replyError(req, -32602, fmt.Sprintf("bad references params: %v", err))
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

// handleDidSave is a notification — no response. Re-extracts the file and
// refreshes its slice of the index. Files outside the registry, or saves
// while we have no index, are silently ignored.
func (s *Server) handleDidSave(req *jsonrpc.Message) {
	var p didSaveTextDocumentParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		log.Printf("didSave: bad params: %v", err)
		return
	}
	idx := s.getIndex()
	if idx == nil {
		return
	}
	path := uriToPath(p.TextDocument.URI)
	if path == "" {
		return
	}
	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	lang := s.registry.LookupByExt(ext)
	if lang == nil {
		return
	}
	ex := symbols.DefaultExtractor(lang.Name)
	if ex == nil {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("didSave: read %s: %v", path, err)
		return
	}
	idx.Refresh(path, lang.Name, ex.Extract(data))
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
