package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/iodesystems/tslsmcp/internal/symbols"
)

// Content is one block of tool output. MCP allows several block types
// (text, image, resource…); we only need text right now and emit
// JSON-formatted payloads inside it so the LLM agent can parse without
// extra round-trips.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Tool is the registry entry for one MCP tool. InputSchema is a raw
// JSON-Schema fragment so we don't have to plumb a schema-builder type
// through the tools layer — they're constant per binary version.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Handler     func(s *Server, args json.RawMessage) ([]Content, bool, error)
}

// registerTools builds the read-only tool table that handleToolsCall
// dispatches against. Tools intentionally have small, name-keyed input
// shapes — LLM agents work well with simple flat arguments and the
// shapes mirror how a human would describe each operation.
func registerTools() map[string]Tool {
	return map[string]Tool{
		"find_symbol": {
			Name: "find_symbol",
			Description: "Search the cross-language workspace symbol index. " +
				"Returns every matching site across go/ts/py/sql/yaml/json/md/proto/openapi/jsonschema, " +
				"tagged with confidence (declared / lexical). Query is a case-insensitive substring.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Case-insensitive substring matched against symbol names. Empty string returns every symbol."}
  },
  "required": ["query"]
}`),
			Handler: handleFindSymbol,
		},
		"find_references": {
			Name: "find_references",
			Description: "Return every workspace position for an exact name. " +
				"Combines lexical hits, declared bindings, and schema-anchored sites — the same set textDocument/references would surface in an editor.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Exact symbol name."}
  },
  "required": ["name"]
}`),
			Handler: handleFindReferences,
		},
		"rename": {
			Name: "rename",
			Description: "Cross-language rename. Returns a list of file edits {file, line, col, oldText, newText} you can apply atomically. " +
				"Confidence policy: if any declared bindings exist for the name they are used (safe by user declaration); otherwise lexical hits are returned (best effort). " +
				"Aliasing safety: edits whose on-disk text doesn't match the name being renamed are skipped, so this is always safe to apply.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Name currently in the workspace."},
    "newName": {"type": "string", "description": "New name."}
  },
  "required": ["name", "newName"]
}`),
			Handler: handleRename,
		},
	}
}

// siteJSON is the wire shape of one site in tool output. Files are
// reported relative to the workspace root so the LLM agent gets stable
// references regardless of where the workspace lives on disk.
type siteJSON struct {
	Name       string `json:"name"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Col        int    `json:"col"`
	Language   string `json:"language,omitempty"`
	Confidence string `json:"confidence"`
}

func handleFindSymbol(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var p struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}
	idx := s.getIndex()
	if idx == nil {
		return []Content{{Type: "text", Text: "index not built (no workspace root configured)"}}, true, nil
	}

	q := strings.ToLower(p.Query)
	var hits []siteJSON
	for _, name := range idx.Names() {
		if q != "" && !strings.Contains(strings.ToLower(name), q) {
			continue
		}
		for _, site := range idx.Lookup(name) {
			hits = append(hits, siteJSON{
				Name:       name,
				File:       relPath(site.File, s.root),
				Line:       site.Line,
				Col:        site.Col,
				Language:   site.Language,
				Confidence: confidenceLabel(site.Confidence),
			})
		}
	}
	return jsonContent(hits), false, nil
}

func handleFindReferences(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}
	if p.Name == "" {
		return nil, true, fmt.Errorf("name is required")
	}
	idx := s.getIndex()
	if idx == nil {
		return []Content{{Type: "text", Text: "index not built (no workspace root configured)"}}, true, nil
	}
	var hits []siteJSON
	for _, site := range idx.Lookup(p.Name) {
		hits = append(hits, siteJSON{
			Name:       p.Name,
			File:       relPath(site.File, s.root),
			Line:       site.Line,
			Col:        site.Col,
			Language:   site.Language,
			Confidence: confidenceLabel(site.Confidence),
		})
	}
	return jsonContent(hits), false, nil
}

type renameEdit struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

func handleRename(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var p struct {
		Name    string `json:"name"`
		NewName string `json:"newName"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}
	if p.Name == "" || p.NewName == "" {
		return nil, true, fmt.Errorf("name and newName are required")
	}
	idx := s.getIndex()
	if idx == nil {
		return []Content{{Type: "text", Text: "index not built (no workspace root configured)"}}, true, nil
	}

	sites := chooseRenameSites(idx.Lookup(p.Name))
	if len(sites) == 0 {
		return jsonContent(map[string]any{
			"name": p.Name, "newName": p.NewName, "edits": []renameEdit{},
		}), false, nil
	}

	fileCache := map[string][]byte{}
	edits := []renameEdit{}
	for _, site := range sites {
		if !siteTextMatches(site, p.Name, fileCache) {
			continue
		}
		edits = append(edits, renameEdit{
			File:    relPath(site.File, s.root),
			Line:    site.Line,
			Col:     site.Col,
			OldText: p.Name,
			NewText: p.NewName,
		})
	}

	return jsonContent(map[string]any{
		"name":    p.Name,
		"newName": p.NewName,
		"edits":   edits,
	}), false, nil
}

// chooseRenameSites mirrors the LSP-side confidence policy: declared
// sites win when any are present, otherwise fall back to lexical hits.
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

// siteTextMatches reads the file (cached across one tool call) and
// confirms the bytes at (Line, Col) of length len(name) equal name.
// Returns false on read errors or out-of-range coordinates — both
// treated as "don't include this edit" rather than tool failure, so
// aliasing bindings never produce wrong edits.
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
	lineStart := 0
	current := 1
	for current < site.Line && lineStart < len(data) {
		nl := bytesIndexNewline(data[lineStart:])
		if nl < 0 {
			return false
		}
		lineStart += nl + 1
		current++
	}
	if current != site.Line {
		return false
	}
	nl := bytesIndexNewline(data[lineStart:])
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

func bytesIndexNewline(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}

// jsonContent marshals value into a single text content block — the
// most reliable shape for LLM consumers across MCP clients.
func jsonContent(value any) []Content {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return []Content{{Type: "text", Text: fmt.Sprintf("internal: %v", err)}}
	}
	return []Content{{Type: "text", Text: string(raw)}}
}

// relPath returns abs relative to root when possible, otherwise abs
// unchanged. Stable workspace-relative paths beat absolute ones for the
// LLM (and across machines).
func relPath(abs, root string) string {
	if root == "" {
		return abs
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return abs
	}
	return rel
}

// confidenceLabel maps the internal enum to a short string the LLM can
// reason about.
func confidenceLabel(c symbols.Confidence) string {
	switch c {
	case symbols.ConfidenceLexical:
		return "lexical"
	case symbols.ConfidenceDeclared:
		return "declared"
	case symbols.ConfidenceLSP:
		return "lsp"
	}
	return "unknown"
}
