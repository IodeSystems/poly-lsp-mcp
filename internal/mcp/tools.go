package mcp

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/iodesystems/tslsmcp/internal/bindings"
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
		"list_bindings": {
			Name: "list_bindings",
			Description: "List every cross-language binding the index knows about — the ones declared by the user in tslsmcp.yaml (Tier 2: symbol / jsonpath / regex sites) and the ones auto-derived from schema files (Tier 3: proto / openapi / jsonschema). " +
				"For each binding the response carries the name, total site count, the set of languages those sites live in, and every (file, line, col) position. Use this to explore what the workspace's cross-language model looks like without running rename queries.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {},
  "required": []
}`),
			Handler: handleListBindings,
		},
		"document_symbols": {
			Name: "document_symbols",
			Description: "Return every symbol in one file — declarations, uses, and binding members — sorted by position. " +
				"Pass `file` workspace-relative (preferred) or absolute. Output entries carry the confidence tag so you can distinguish declared bindings from lexical hits.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "file": {"type": "string", "description": "Workspace-relative or absolute path"}
  },
  "required": ["file"]
}`),
			Handler: handleDocumentSymbols,
		},
		"refresh": {
			Name: "refresh",
			Description: "Rebuild the workspace symbol index from disk. Use after files have changed (an agent applied edits, a worktree was checked out, etc.). " +
				"With no arguments, rebuilds against the current workspace root. With `workspace_root`, points the index at a different absolute directory — useful when one tslsmcp instance serves multiple worktrees of the same project, since the bindings and schemas configured at startup are re-applied at the new root.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "workspace_root": {"type": "string", "description": "Optional absolute path. If omitted, rebuild the current root."}
  },
  "required": []
}`),
			Handler: handleRefresh,
		},
		"apply_rename": {
			Name: "apply_rename",
			Description: "Cross-language rename that WRITES the edits to disk instead of returning them. Same confidence policy and aliasing-safety check as `rename` — declared sites win when present, edits whose on-disk text doesn't match the name being renamed are skipped. " +
				"Each file is written via temp-file + rename so a partial failure doesn't leave a half-edited file. Use this when the agent doesn't need to inspect the plan before applying.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Name currently in the workspace."},
    "newName": {"type": "string", "description": "New name."}
  },
  "required": ["name", "newName"]
}`),
			Handler: handleApplyRename,
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
				File:       relPath(site.File, s.getRoot()),
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
			File:       relPath(site.File, s.getRoot()),
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

// resolvedEdit is the internal shape used by buildRenameEdits — it
// carries the absolute path alongside the wire-shape file path so the
// apply_rename tool can read/write without re-joining root.
type resolvedEdit struct {
	AbsFile string
	RelFile string
	Line    int
	Col     int
	OldText string
	NewText string
}

// buildRenameEdits is the shared plan-builder for the rename and
// apply_rename tools. It applies the same confidence policy (declared
// sites win when present) and aliasing-safety check (on-disk text must
// equal name) as the LSP rename handler.
func (s *Server) buildRenameEdits(name, newName string) []resolvedEdit {
	idx := s.getIndex()
	if idx == nil {
		return nil
	}
	sites := chooseRenameSites(idx.Lookup(name))
	if len(sites) == 0 {
		return nil
	}
	fileCache := map[string][]byte{}
	out := make([]resolvedEdit, 0, len(sites))
	root := s.getRoot()
	for _, site := range sites {
		if !siteTextMatches(site, name, fileCache) {
			continue
		}
		out = append(out, resolvedEdit{
			AbsFile: site.File,
			RelFile: relPath(site.File, root),
			Line:    site.Line,
			Col:     site.Col,
			OldText: name,
			NewText: newName,
		})
	}
	return out
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
	if s.getIndex() == nil {
		return []Content{{Type: "text", Text: "index not built (no workspace root configured)"}}, true, nil
	}

	resolved := s.buildRenameEdits(p.Name, p.NewName)
	edits := make([]renameEdit, len(resolved))
	for i, r := range resolved {
		edits[i] = renameEdit{
			File:    r.RelFile,
			Line:    r.Line,
			Col:     r.Col,
			OldText: r.OldText,
			NewText: r.NewText,
		}
	}
	return jsonContent(map[string]any{
		"name":    p.Name,
		"newName": p.NewName,
		"edits":   edits,
	}), false, nil
}

// applyResult is the wire shape apply_rename returns per file it
// touched (or per file it skipped with a reason).
type applyResult struct {
	File    string `json:"file"`
	Edits   int    `json:"edits,omitempty"`
	Skipped string `json:"skipped,omitempty"`
}

func handleApplyRename(s *Server, args json.RawMessage) ([]Content, bool, error) {
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
	if s.getIndex() == nil {
		return []Content{{Type: "text", Text: "index not built (no workspace root configured)"}}, true, nil
	}

	resolved := s.buildRenameEdits(p.Name, p.NewName)

	// Group edits by file. Each file's writes go through a single
	// temp+rename so partial failures don't leave a half-written file.
	byFile := map[string][]resolvedEdit{}
	order := []string{}
	for _, e := range resolved {
		if _, ok := byFile[e.AbsFile]; !ok {
			order = append(order, e.AbsFile)
		}
		byFile[e.AbsFile] = append(byFile[e.AbsFile], e)
	}

	results := make([]applyResult, 0, len(order))
	for _, abs := range order {
		edits := byFile[abs]
		rel := edits[0].RelFile
		n, err := applyFileEdits(abs, edits)
		if err != nil {
			results = append(results, applyResult{File: rel, Skipped: err.Error()})
			continue
		}
		results = append(results, applyResult{File: rel, Edits: n})
	}

	return jsonContent(map[string]any{
		"name":         p.Name,
		"newName":      p.NewName,
		"filesChanged": len(results),
		"results":      results,
	}), false, nil
}

// applyFileEdits writes the supplied edits to absFile. Edits are sorted
// by (line desc, col desc) so applying them rightmost-first leaves
// earlier byte offsets undisturbed. Per-edit text mismatches are
// silently skipped — buildRenameEdits already verified them but the
// file might have changed under us between Plan and Apply. Returns the
// number of edits actually applied. The file is written via
// temp-file + os.Rename for atomicity; the temp file inherits the
// original file's mode so executable bits survive.
func applyFileEdits(absFile string, edits []resolvedEdit) (int, error) {
	data, err := os.ReadFile(absFile)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(absFile)
	if err != nil {
		return 0, err
	}

	sorted := make([]resolvedEdit, len(edits))
	copy(sorted, edits)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Line != sorted[j].Line {
			return sorted[i].Line > sorted[j].Line
		}
		return sorted[i].Col > sorted[j].Col
	})

	out := data
	applied := 0
	for _, e := range sorted {
		offset, ok := lineColToByteOffset(out, e.Line, e.Col)
		if !ok {
			continue
		}
		end := offset + len(e.OldText)
		if end > len(out) {
			continue
		}
		if string(out[offset:end]) != e.OldText {
			continue
		}
		out = append(append(append([]byte{}, out[:offset]...), []byte(e.NewText)...), out[end:]...)
		applied++
	}

	if applied == 0 {
		return 0, nil
	}

	tmp := absFile + ".tslsmcp.tmp"
	if err := os.WriteFile(tmp, out, info.Mode().Perm()); err != nil {
		return applied, err
	}
	if err := os.Rename(tmp, absFile); err != nil {
		// Best effort cleanup so a failed rename doesn't leave a
		// stray temp file.
		_ = os.Remove(tmp)
		return applied, err
	}
	return applied, nil
}

// lineColToByteOffset walks data line-by-line and returns the byte
// offset of (1-based line, 1-based col), or false if out of range.
// Mirrors siteTextMatches's walk so apply uses the same line discipline
// the index/lookup pipeline produced.
func lineColToByteOffset(data []byte, line, col int) (int, bool) {
	pos := 0
	cur := 1
	for cur < line && pos < len(data) {
		nl := bytesIndexNewline(data[pos:])
		if nl < 0 {
			return 0, false
		}
		pos += nl + 1
		cur++
	}
	if cur != line {
		return 0, false
	}
	return pos + col - 1, true
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

// handleRefresh rebuilds the symbol index from disk, optionally at a
// new workspace root, then re-applies the bindings and schemas that
// were configured at startup. The new index swaps in atomically — on
// build failure the old index keeps serving.
func handleRefresh(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var p struct {
		WorkspaceRoot string `json:"workspace_root"`
	}
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, true, fmt.Errorf("bad arguments: %w", err)
		}
	}

	newRoot := s.getRoot()
	if p.WorkspaceRoot != "" {
		if !filepath.IsAbs(p.WorkspaceRoot) {
			return nil, true, fmt.Errorf("workspace_root must be an absolute path; got %q", p.WorkspaceRoot)
		}
		newRoot = p.WorkspaceRoot
	}
	if newRoot == "" {
		return nil, true, fmt.Errorf("no workspace root configured and none provided")
	}

	idx, err := symbols.Build(newRoot, s.registry)
	if err != nil {
		return nil, true, fmt.Errorf("build index at %s: %w", newRoot, err)
	}

	// Re-apply Tier 2 + Tier 3 bindings at the new root. Per-binding
	// failures are logged inside the resolver; we surface the resulting
	// counts so the caller can sanity-check.
	resolver := bindings.NewResolver(newRoot)
	declaredCount := 0
	if len(s.bindings) > 0 {
		n, err := resolver.Apply(idx, s.bindings)
		if err != nil {
			log.Printf("refresh: some bindings failed validation: %v", err)
		}
		declaredCount = n
	}
	schemaCount := 0
	if len(s.schemas) > 0 {
		schemaCount = resolver.ApplySchemas(idx, s.schemas)
	}

	s.setRoot(newRoot)
	s.setIndex(idx)

	return jsonContent(map[string]any{
		"root":          newRoot,
		"names":         len(idx.Names()),
		"declaredSites": declaredCount,
		"schemaSites":   schemaCount,
	}), false, nil
}

// bindingSummary is the wire shape `list_bindings` emits per name.
type bindingSummary struct {
	Name      string     `json:"name"`
	SiteCount int        `json:"siteCount"`
	Languages []string   `json:"languages"`
	Sites     []siteJSON `json:"sites"`
}

func handleListBindings(s *Server, _ json.RawMessage) ([]Content, bool, error) {
	idx := s.getIndex()
	if idx == nil {
		return []Content{{Type: "text", Text: "index not built (no workspace root configured)"}}, true, nil
	}
	names := idx.DeclaredNames()
	out := make([]bindingSummary, 0, len(names))
	for _, name := range names {
		var sites []siteJSON
		langSet := map[string]struct{}{}
		for _, site := range idx.Lookup(name) {
			if site.Confidence < symbols.ConfidenceDeclared {
				continue
			}
			sites = append(sites, siteJSON{
				Name:       name,
				File:       relPath(site.File, s.getRoot()),
				Line:       site.Line,
				Col:        site.Col,
				Language:   site.Language,
				Confidence: confidenceLabel(site.Confidence),
			})
			if site.Language != "" {
				langSet[site.Language] = struct{}{}
			}
		}
		langs := make([]string, 0, len(langSet))
		for l := range langSet {
			langs = append(langs, l)
		}
		sort.Strings(langs)
		out = append(out, bindingSummary{
			Name:      name,
			SiteCount: len(sites),
			Languages: langs,
			Sites:     sites,
		})
	}
	return jsonContent(out), false, nil
}

func handleDocumentSymbols(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var p struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}
	if p.File == "" {
		return nil, true, fmt.Errorf("file is required")
	}
	idx := s.getIndex()
	if idx == nil {
		return []Content{{Type: "text", Text: "index not built (no workspace root configured)"}}, true, nil
	}

	abs := p.File
	if !filepath.IsAbs(abs) && s.getRoot() != "" {
		abs = filepath.Join(s.getRoot(), abs)
	}

	var hits []siteJSON
	for _, name := range idx.Names() {
		for _, site := range idx.Lookup(name) {
			if site.File != abs {
				continue
			}
			hits = append(hits, siteJSON{
				Name:       name,
				File:       relPath(site.File, s.getRoot()),
				Line:       site.Line,
				Col:        site.Col,
				Language:   site.Language,
				Confidence: confidenceLabel(site.Confidence),
			})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Line != hits[j].Line {
			return hits[i].Line < hits[j].Line
		}
		return hits[i].Col < hits[j].Col
	})
	return jsonContent(hits), false, nil
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
