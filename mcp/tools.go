package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// Content is one block of tool output. MCP allows several block types
// (text, image, resource…); we only need text and emit JSON-formatted
// payloads inside it so the LLM agent can parse without extra
// round-trips.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Tool is the registry entry for one MCP tool. InputSchema is a raw
// JSON-Schema fragment so we don't have to plumb a schema-builder
// through the tools layer — they're constant per binary version.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Handler     func(s *Server, args json.RawMessage) ([]Content, bool, error)
}

// registerTools returns the 6-tool surface poly-lsp-mcp exposes. Each tool
// does one job; there is no preview-vs-apply duplication and no
// substring-vs-exact ambiguity. The surface mirrors how an LLM
// actually thinks about code:
//
//   - structure(path, depth) walks dirs at the workspace and named
//     nodes inside files, uniformly.
//   - node_references(file, range) points at an identifier and asks
//     where it's used.
//   - node_read / node_edit / node_delete are the read/write/erase
//     primitives that operate on (file, range) addresses.
//   - node_refactor(file, range, kind, ...) is the multi-modal
//     refactor channel — kind="rename" today; change_signature etc.
//     land here as use cases surface.
//
// There is no `refresh` tool. structure() does an implicit content-
// hash sweep when called on a directory; node_edit / node_delete
// re-parse the file they just wrote. Together they keep the index
// honest without an explicit refresh step.
func registerTools() map[string]Tool {
	return map[string]Tool{
		"structure": {
			Name: "structure",
			Description: "Hierarchical tour of a workspace, directory, or file. " +
				"`path` (default: workspace root) is workspace-relative or absolute. " +
				"`depth` (default: 1) controls how many levels to descend — at workspace level into directories, at file level into AST nodes. " +
				"For files: requires a tree-sitter grammar (go / typescript / tsx / python / sql). " +
				"Each `node` entry carries both `range` (the whole declaration) and the `name*` fields (the identifier within it); pass the identifier's range to node_references and node_refactor, the whole range to node_read / node_edit / node_delete. " +
				"Calling structure on the workspace root implicitly refreshes the index for any files whose bytes changed.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Workspace-relative or absolute path. Default: workspace root."},
    "depth": {"type": "integer", "minimum": 0, "description": "How many levels to expand. Default: 1."}
  },
  "required": []
}`),
			Handler: handleStructure,
		},
		"node_references": {
			Name: "node_references",
			Description: "Return every workspace position where the identifier at (file, range) is referenced. " +
				"Range must cover just the identifier (use `nameStartLine` / `nameStartCol` from structure(file)). " +
				"Output combines lexical hits, declared bindings, and schema-anchored sites — the same union node_refactor would touch on rename.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "file":      {"type": "string"},
    "startLine": {"type": "integer", "minimum": 1},
    "startCol":  {"type": "integer", "minimum": 1},
    "endLine":   {"type": "integer", "minimum": 1},
    "endCol":    {"type": "integer", "minimum": 1}
  },
  "required": ["file", "startLine", "startCol", "endLine", "endCol"]
}`),
			Handler: handleNodeReferences,
		},
		"node_read": {
			Name: "node_read",
			Description: "Return the text between two 1-based (line, col) positions in a file. " +
				"Convention matches structure's output: startLine/startCol inclusive, endLine/endCol exclusive. " +
				"Works on any file regardless of language.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "file":      {"type": "string"},
    "startLine": {"type": "integer", "minimum": 1},
    "startCol":  {"type": "integer", "minimum": 1},
    "endLine":   {"type": "integer", "minimum": 1},
    "endCol":    {"type": "integer", "minimum": 1}
  },
  "required": ["file", "startLine", "startCol", "endLine", "endCol"]
}`),
			Handler: handleNodeRead,
		},
		"node_edit": {
			Name: "node_edit",
			Description: "Atomically replace the text between two 1-based positions with newText. " +
				"Same range convention as node_read. " +
				"Goes through temp + Rename so a partial failure can't corrupt the file; file mode is preserved. " +
				"After the write the file's slice of the index is re-parsed so subsequent node_references calls see the new state.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "file":      {"type": "string"},
    "startLine": {"type": "integer", "minimum": 1},
    "startCol":  {"type": "integer", "minimum": 1},
    "endLine":   {"type": "integer", "minimum": 1},
    "endCol":    {"type": "integer", "minimum": 1},
    "newText":   {"type": "string"}
  },
  "required": ["file", "startLine", "startCol", "endLine", "endCol", "newText"]
}`),
			Handler: handleNodeEdit,
		},
		"node_delete": {
			Name: "node_delete",
			Description: "Atomically remove the text between two 1-based positions. " +
				"Equivalent to node_edit with newText='' but states intent. " +
				"Range deletion is exact — surrounding whitespace and blank lines are not adjusted; use a wider range or follow up with node_edit if you want them trimmed. " +
				"Index re-parses the file after the write.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "file":      {"type": "string"},
    "startLine": {"type": "integer", "minimum": 1},
    "startCol":  {"type": "integer", "minimum": 1},
    "endLine":   {"type": "integer", "minimum": 1},
    "endCol":    {"type": "integer", "minimum": 1}
  },
  "required": ["file", "startLine", "startCol", "endLine", "endCol"]
}`),
			Handler: handleNodeDelete,
		},
		"node_refactor": {
			Name: "node_refactor",
			Description: "Multi-modal cross-language refactor. " +
				"Point `range` at an identifier (use structure's `nameStart*` / `nameEnd*` fields). " +
				"`kind` selects the refactor: today only \"rename\" is supported — it propagates the rename across every site declared bindings or the lexical index turn up, with per-site on-disk text verification so aliasing bindings can't substitute the wrong token. " +
				"Set `includeComments` true for kind='rename' when you want documentation and comment references (which tree-sitter normally skips) renamed too — a workspace-wide word-boundary scan augments the plan; partial-word matches like `thisUserID` are not touched because the scan anchors on identifier boundaries. " +
				"Future kinds (change_signature, change_return_type) land here without growing the tool count.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "file":            {"type": "string"},
    "startLine":       {"type": "integer", "minimum": 1},
    "startCol":        {"type": "integer", "minimum": 1},
    "endLine":         {"type": "integer", "minimum": 1},
    "endCol":          {"type": "integer", "minimum": 1},
    "kind":            {"type": "string", "enum": ["rename"], "description": "Refactor kind."},
    "newName":         {"type": "string", "description": "Required when kind='rename'."},
    "includeComments": {"type": "boolean", "description": "When kind='rename', also rename word-boundary mentions in comments / prose / non-indexed file types. Default false."}
  },
  "required": ["file", "startLine", "startCol", "endLine", "endCol", "kind"]
}`),
			Handler: handleNodeRefactor,
		},
	}
}

// -------------------------------------------------------------- arg shapes

// rangeArgs is the shared (file, startLine, startCol, endLine, endCol)
// input shape used by node_read / node_edit / node_delete /
// node_references / node_refactor. 1-based, end-exclusive — same as
// structure's output.
type rangeArgs struct {
	File      string `json:"file"`
	StartLine int    `json:"startLine"`
	StartCol  int    `json:"startCol"`
	EndLine   int    `json:"endLine"`
	EndCol    int    `json:"endCol"`
}

func (a rangeArgs) validate() error {
	if a.File == "" {
		return errors.New("file is required")
	}
	if a.StartLine < 1 || a.StartCol < 1 || a.EndLine < 1 || a.EndCol < 1 {
		return fmt.Errorf("line and col must be >= 1 (got %+v)", a)
	}
	if a.EndLine < a.StartLine || (a.EndLine == a.StartLine && a.EndCol < a.StartCol) {
		return fmt.Errorf("range end before start: %+v", a)
	}
	return nil
}

// -------------------------------------------------------------- structure

// structureEntry is the unified tree node the structure tool emits.
// `kind` distinguishes filesystem entries from AST nodes:
//
//	"directory" — a directory on disk
//	"file"      — a regular file
//	"node"      — a tree-sitter named child inside a file
type structureEntry struct {
	Kind          string           `json:"kind"`
	Path          string           `json:"path,omitempty"`
	Type          string           `json:"type,omitempty"`
	Name          string           `json:"name,omitempty"`
	StartLine     int              `json:"startLine,omitempty"`
	StartCol      int              `json:"startCol,omitempty"`
	EndLine       int              `json:"endLine,omitempty"`
	EndCol        int              `json:"endCol,omitempty"`
	NameStartLine int              `json:"nameStartLine,omitempty"`
	NameStartCol  int              `json:"nameStartCol,omitempty"`
	NameEndLine   int              `json:"nameEndLine,omitempty"`
	NameEndCol    int              `json:"nameEndCol,omitempty"`
	Children      []structureEntry `json:"children,omitempty"`
}

func handleStructure(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var p struct {
		Path  string `json:"path"`
		Depth *int   `json:"depth"`
	}
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, true, fmt.Errorf("bad arguments: %w", err)
		}
	}
	depth := 1
	if p.Depth != nil {
		depth = *p.Depth
		if depth < 0 {
			return nil, true, fmt.Errorf("depth must be >= 0")
		}
	}
	if p.Path == "" {
		p.Path = "."
	}
	abs := s.resolveFileArg(p.Path)
	info, err := os.Stat(abs)
	if err != nil {
		return nil, true, fmt.Errorf("stat %s: %w", p.Path, err)
	}
	if info.IsDir() {
		entry, _ := structureForDir(abs, s.getRoot(), depth)
		return jsonContent(entry), false, nil
	}
	entry, err := structureForFile(abs, s.languageForFile(abs), s.getRoot(), depth)
	if err != nil {
		return nil, true, err
	}
	return jsonContent(entry), false, nil
}

func structureForDir(abs, root string, depth int) (structureEntry, bool) {
	entry := structureEntry{
		Kind: "directory",
		Path: relPath(abs, root),
		Name: filepath.Base(abs),
	}
	if depth <= 0 {
		return entry, false
	}
	dirEntries, err := os.ReadDir(abs)
	if err != nil {
		return entry, false
	}
	for _, de := range dirEntries {
		name := de.Name()
		if skipStructureDir(name) {
			continue
		}
		childAbs := filepath.Join(abs, name)
		if de.IsDir() {
			child, _ := structureForDir(childAbs, root, depth-1)
			entry.Children = append(entry.Children, child)
		} else {
			entry.Children = append(entry.Children, structureEntry{
				Kind: "file",
				Path: relPath(childAbs, root),
				Name: name,
			})
		}
	}
	sort.Slice(entry.Children, func(i, j int) bool {
		return entry.Children[i].Path < entry.Children[j].Path
	})
	return entry, true
}

// skipStructureDir mirrors symbols.Build's allow-list: never descend
// into .git / node_modules / vendor / __pycache__ etc.
func skipStructureDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "__pycache__", "dist", "build", ".idea", ".vscode", ".poly-lsp-mcp":
		return true
	}
	return false
}

func structureForFile(abs, lang, root string, depth int) (structureEntry, error) {
	entry := structureEntry{
		Kind: "file",
		Path: relPath(abs, root),
		Name: filepath.Base(abs),
	}
	if depth <= 0 {
		return entry, nil
	}

	content, err := os.ReadFile(abs)
	if err != nil {
		return entry, fmt.Errorf("read %s: %w", abs, err)
	}

	// For files we have a tree-sitter grammar for, return named child
	// nodes (declarations, types, classes, etc.).
	if lang != "" && symbols.LanguageByName(lang) != nil {
		nodes, err := symbols.StructureNodes(lang, content)
		if err != nil {
			return entry, err
		}
		for _, n := range nodes {
			entry.Children = append(entry.Children, structureEntry{
				Kind:          "node",
				Type:          n.Type,
				Name:          n.Name,
				StartLine:     n.StartLine,
				StartCol:      n.StartCol,
				EndLine:       n.EndLine,
				EndCol:        n.EndCol,
				NameStartLine: n.NameStartLine,
				NameStartCol:  n.NameStartCol,
				NameEndLine:   n.NameEndLine,
				NameEndCol:    n.NameEndCol,
			})
		}
		return entry, nil
	}

	// Otherwise — language uses the lexical extractor only (yaml /
	// json / markdown) or has no registered language at all (Dockerfile,
	// TOML, HCL, .env, …). Either way, return a single "text" node
	// covering the whole file so agents can node_read / node_edit /
	// node_delete it like any other range.
	endLine, endCol := contentEndPosition(content)
	entry.Children = []structureEntry{{
		Kind:      "node",
		Type:      "text",
		StartLine: 1,
		StartCol:  1,
		EndLine:   endLine,
		EndCol:    endCol,
	}}
	return entry, nil
}

// contentEndPosition returns the 1-based (line, col) position one past
// the last byte of content. For "abc\n" the end is (2, 1); for "abc"
// it's (1, 4); for empty content it's (1, 1).
func contentEndPosition(content []byte) (int, int) {
	line, col := 1, 1
	for _, b := range content {
		if b == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

// -------------------------------------------------------------- node_references

func handleNodeReferences(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var a rangeArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}
	if err := a.validate(); err != nil {
		return nil, true, err
	}
	idx := s.getIndex()
	if idx == nil {
		return []Content{{Type: "text", Text: "index not built (no workspace root configured)"}}, true, nil
	}

	abs := s.resolveFileArg(a.File)
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", a.File, err)
	}
	name, err := readRangeText(content, a)
	if err != nil {
		return nil, true, err
	}
	if name == "" {
		return nil, true, errors.New("range is empty; pass the identifier's nameStart/nameEnd range")
	}

	var hits []siteJSON
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
	return jsonContent(hits), false, nil
}

// siteJSON is the wire shape of one site in tool output. Files are
// reported workspace-relative.
type siteJSON struct {
	Name       string `json:"name"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Col        int    `json:"col"`
	Language   string `json:"language,omitempty"`
	Confidence string `json:"confidence"`
}

// -------------------------------------------------------------- node_read

func handleNodeRead(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var a rangeArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}
	if err := a.validate(); err != nil {
		return nil, true, err
	}
	abs := s.resolveFileArg(a.File)
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", a.File, err)
	}
	text, err := readRangeText(content, a)
	if err != nil {
		return nil, true, err
	}
	return jsonContent(map[string]any{
		"file":      a.File,
		"startLine": a.StartLine,
		"startCol":  a.StartCol,
		"endLine":   a.EndLine,
		"endCol":    a.EndCol,
		"text":      text,
	}), false, nil
}

// -------------------------------------------------------------- node_edit / node_delete

func handleNodeEdit(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var p struct {
		rangeArgs
		diagnosticOptions
		NewText string `json:"newText"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}
	return s.applyRangeRewrite(p.rangeArgs, p.NewText, p.diagnosticOptions)
}

func handleNodeDelete(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var p struct {
		rangeArgs
		diagnosticOptions
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}
	return s.applyRangeRewrite(p.rangeArgs, "", p.diagnosticOptions)
}

// applyRangeRewrite is the shared write path. Validates the range,
// reads + edits + atomic-renames the file, then reparses just this
// file's slice of the index so subsequent node_references sees the
// new state.
func (s *Server) applyRangeRewrite(a rangeArgs, newText string, opts diagnosticOptions) ([]Content, bool, error) {
	if err := a.validate(); err != nil {
		return nil, true, err
	}
	abs := s.resolveFileArg(a.File)
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", a.File, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, true, fmt.Errorf("stat %s: %w", a.File, err)
	}
	startOff, ok := lineColToByteOffset(content, a.StartLine, a.StartCol)
	if !ok {
		return nil, true, fmt.Errorf("start out of range: %d:%d", a.StartLine, a.StartCol)
	}
	endOff, ok := lineColToByteOffset(content, a.EndLine, a.EndCol)
	if !ok {
		return nil, true, fmt.Errorf("end out of range: %d:%d", a.EndLine, a.EndCol)
	}
	if endOff > len(content) {
		endOff = len(content)
	}
	if startOff > endOff {
		return nil, true, fmt.Errorf("start byte offset %d > end %d", startOff, endOff)
	}

	out := make([]byte, 0, len(content)-(endOff-startOff)+len(newText))
	out = append(out, content[:startOff]...)
	out = append(out, newText...)
	out = append(out, content[endOff:]...)

	tmp := abs + ".poly-lsp-mcp.tmp"
	if err := os.WriteFile(tmp, out, info.Mode().Perm()); err != nil {
		return nil, true, fmt.Errorf("write temp: %w", err)
	}
	if err := os.Rename(tmp, abs); err != nil {
		_ = os.Remove(tmp)
		return nil, true, fmt.Errorf("rename: %w", err)
	}

	s.refreshFileInIndex(abs, out)

	uri := pathToURI(abs)
	diags := s.collectDiagnostics([]string{uri}, map[string][]byte{uri: out}, opts)

	payload := map[string]any{
		"file":                 a.File,
		"replacedFrom":         map[string]int{"line": a.StartLine, "col": a.StartCol},
		"replacedTo":           map[string]int{"line": a.EndLine, "col": a.EndCol},
		"bytesRemoved":         endOff - startOff,
		"bytesAdded":           len(newText),
		"diagnosticsAvailable": diags.Available,
		"diagnosticsTimedOut":  diags.TimedOut,
		"diagnostics":          diags.Items,
	}
	if diags.DroppedDiagnostics > 0 {
		payload["droppedDiagnostics"] = diags.DroppedDiagnostics
	}
	return jsonContent(payload), false, nil
}

// refreshFileInIndex re-extracts this file's slice into the index so
// node_references picks up the new state on the next call. Best
// effort: extractor lookup misses are silently ignored (the file just
// stays at its previous index entry, which is harmless).
func (s *Server) refreshFileInIndex(abs string, content []byte) {
	idx := s.getIndex()
	if idx == nil {
		return
	}
	lang := s.languageForFile(abs)
	if lang == "" {
		return
	}
	ex := symbols.DefaultExtractor(lang)
	if ex == nil {
		return
	}
	hits := ex.Extract(content)
	idx.Refresh(abs, lang, hits)
	s.parseCache.Put(lang, content, hits)

	// Comment markers (@see / @link / @ref / x-ref) get re-scanned
	// alongside the lexical pass. The Refresh call above only clears
	// lexical sites for this file; clear comment sites separately so
	// the new content's markers replace the prior snapshot.
	idx.RefreshCommentsForFile(abs)
	for _, ref := range symbols.ExtractCommentRefs(content) {
		switch ref.Confidence {
		case symbols.ConfidenceDeclared:
			idx.InsertDeclared(ref.Name, abs, lang, ref.Line, ref.Col)
		default:
			idx.InsertComment(ref.Name, abs, lang, ref.Line, ref.Col)
		}
	}
}

// -------------------------------------------------------------- node_refactor

// refactorOps is the nested shape node_refactor accepts: pass any
// non-empty subset of fields to apply that combination in one call.
// rename touches the identifier across the workspace; params rebuilds
// the function declaration's parameter list (and best-effort rewrites
// call sites); return rebuilds the result type.
type refactorOps struct {
	Rename string          `json:"rename,omitempty"`
	Params []refactorParam `json:"params,omitempty"`
	Return string          `json:"return,omitempty"`
}

type refactorParam struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// nonEmpty reports whether at least one refactor op is requested.
func (r refactorOps) nonEmpty() bool {
	return r.Rename != "" || r.Params != nil || r.Return != ""
}

func handleNodeRefactor(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var p struct {
		rangeArgs
		diagnosticOptions
		// New shape — preferred. Nested object so callers can bundle
		// multiple refactors in one tool call.
		Refactor refactorOps `json:"refactor"`
		// Legacy shape — kept for callers using the original
		// kind=rename, newName= surface. Equivalent to
		// refactor: {rename: <newName>}.
		Kind            string `json:"kind"`
		NewName         string `json:"newName"`
		IncludeComments bool   `json:"includeComments"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}
	if err := p.rangeArgs.validate(); err != nil {
		return nil, true, err
	}

	// Normalize legacy kind=rename into the nested shape so the rest
	// of the handler has one path.
	ops := p.Refactor
	if p.Kind != "" {
		switch p.Kind {
		case "rename":
			if p.NewName == "" {
				return nil, true, errors.New("newName is required when kind='rename'")
			}
			if ops.Rename != "" && ops.Rename != p.NewName {
				return nil, true, errors.New("conflicting rename: kind/newName and refactor.rename disagree")
			}
			ops.Rename = p.NewName
		default:
			return nil, true, fmt.Errorf("unsupported refactor kind: %q (use refactor:{...} instead)", p.Kind)
		}
	}
	if !ops.nonEmpty() {
		return nil, true, errors.New("refactor must specify at least one of {rename, params, return}")
	}
	signatureOps := ops.Params != nil || ops.Return != ""
	if !signatureOps {
		return s.refactorRename(p.rangeArgs, ops.Rename, p.IncludeComments, p.diagnosticOptions)
	}
	return s.refactorSignature(p.rangeArgs, ops, p.IncludeComments, p.diagnosticOptions)
}

// refactorSignature handles refactor ops that change a function's
// signature — params, return type, or both, optionally combined with
// a rename. Today this is Go-only; non-Go files at the range get a
// clear error.
//
// The signature rewrite is purely declaration-local: parameters /
// result are rebuilt; the body is left untouched. When rename is also
// set, the existing workspace-wide rename path runs in addition so
// callers get the new name. Best-effort call-site rewriting (drop
// removed args, insert zero values for added ones) lands in a follow-
// up; for now the diagnostic round-trip surfaces broken callers.
func (s *Server) refactorSignature(a rangeArgs, ops refactorOps, includeComments bool, dopts diagnosticOptions) ([]Content, bool, error) {
	abs := s.resolveFileArg(a.File)
	lang := s.languageForFile(abs)
	if !signatureSupportedLanguage(lang) {
		return nil, true, fmt.Errorf("signature refactor not supported for language %q (try go / typescript / python)", lang)
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", a.File, err)
	}
	sig, err := symbols.FindFunctionSignature(lang, content, a.StartLine, a.StartCol)
	if err != nil {
		return nil, true, fmt.Errorf("parse %s: %w", a.File, err)
	}
	if sig == nil {
		return nil, true, fmt.Errorf("no function declaration at %s:%d:%d", a.File, a.StartLine, a.StartCol)
	}

	oldName := string(content[sig.Name.Start:sig.Name.End])

	// Apply signature-local edits via the language-dispatched
	// rewriter. We DON'T pass ops.Rename to the rewriter because the
	// workspace-wide rename path (refactorRename) needs to run across
	// every file, not just the declaration — it touches the name in
	// callers too. Local rename of just the declaration would leave
	// callers desynced. The rename is applied later, after the
	// signature edit lands.
	localOps := symbols.SignatureOps{
		Params: toSymbolsParams(ops.Params),
		Return: ops.Return,
	}
	out, n, err := symbols.RewriteSignature(content, sig, localOps)
	if err != nil {
		return nil, true, fmt.Errorf("rewrite signature: %w", err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		return nil, true, fmt.Errorf("stat %s: %w", a.File, err)
	}
	tmp := abs + ".poly-lsp-mcp.tmp"
	if err := os.WriteFile(tmp, out, info.Mode().Perm()); err != nil {
		return nil, true, fmt.Errorf("write temp: %w", err)
	}
	if err := os.Rename(tmp, abs); err != nil {
		_ = os.Remove(tmp)
		return nil, true, fmt.Errorf("rename: %w", err)
	}
	s.refreshFileInIndex(abs, out)

	results := []applyResult{{File: relPath(abs, s.getRoot()), Edits: n}}

	// Optional workspace-wide rename on top. Re-find the signature
	// in the post-edit content because byte offsets moved.
	postSig, err := symbols.FindFunctionSignature(lang, out, a.StartLine, a.StartCol)
	if err != nil || postSig == nil {
		return nil, true, fmt.Errorf("post-edit signature lookup failed: %v", err)
	}
	if ops.Rename != "" && ops.Rename != oldName {
		nameRangeArgs := nameRangeAfterSignature(a, postSig, out)
		renameContent, renameIsErr, renameErr := s.refactorRename(nameRangeArgs, ops.Rename, includeComments, diagnosticOptions{})
		if renameErr != nil {
			return renameContent, renameIsErr, renameErr
		}
		var renamePayload map[string]any
		if len(renameContent) > 0 {
			_ = json.Unmarshal([]byte(renameContent[0].Text), &renamePayload)
		}
		if extra, ok := renamePayload["results"].([]any); ok {
			for _, r := range extra {
				if m, ok := r.(map[string]any); ok {
					file, _ := m["file"].(string)
					if file == "" || file == results[0].File {
						continue
					}
					edits := 0
					if v, ok := m["edits"].(float64); ok {
						edits = int(v)
					}
					results = append(results, applyResult{File: file, Edits: edits})
				}
			}
		}
	}

	// Best-effort call-site rewriting: only meaningful when the
	// parameter count changes. Same-count signature edits leave
	// args alone — the type checker is the authority on whether
	// existing expressions still fit.
	currentName := oldName
	if ops.Rename != "" {
		currentName = ops.Rename
	}
	uris := []string{pathToURI(abs)}
	contentsByURI := map[string][]byte{uris[0]: out}
	if ops.Params != nil {
		callResults, callContents := s.rewriteCallSites(lang, currentName, ops.Params)
		for _, cr := range callResults {
			if cr.File == results[0].File {
				results[0].Edits += cr.Edits
				continue
			}
			results = append(results, cr)
		}
		for u, c := range callContents {
			uris = append(uris, u)
			contentsByURI[u] = c
		}
	}

	diags := s.collectDiagnostics(uris, contentsByURI, dopts)

	payload := map[string]any{
		"kind":                 "signature",
		"oldName":              oldName,
		"newName":              ops.Rename,
		"filesChanged":         len(results),
		"results":              results,
		"diagnosticsAvailable": diags.Available,
		"diagnosticsTimedOut":  diags.TimedOut,
		"diagnostics":          diags.Items,
	}
	if diags.DroppedDiagnostics > 0 {
		payload["droppedDiagnostics"] = diags.DroppedDiagnostics
	}
	return jsonContent(payload), false, nil
}

// signatureSupportedLanguage reports whether RewriteSignature has a
// per-language implementation. Today: go / typescript / python.
func signatureSupportedLanguage(lang string) bool {
	switch lang {
	case "go", "typescript", "python":
		return true
	}
	return false
}

// rewriteCallSites walks every file in the index that mentions
// funcName (filtered to the supplied language) and rewrites its
// argument lists to match the new parameter count. Three cases per
// call site:
//
//   - count matches: skip (type-only changes left for diagnostics)
//   - new < old: drop trailing args
//   - new > old: append zero-value placeholders for the new positions
//
// Per-site outcomes return as applyResult entries (one per touched
// file); contents-by-URI for the diagnostic round-trip.
func (s *Server) rewriteCallSites(language, funcName string, params []refactorParam) ([]applyResult, map[string][]byte) {
	idx := s.getIndex()
	if idx == nil {
		return nil, nil
	}
	files := map[string]bool{}
	for _, site := range idx.Lookup(funcName) {
		if site.Language == language {
			files[site.File] = true
		}
	}
	if len(files) == 0 {
		return nil, nil
	}

	symParams := toSymbolsParams(params)
	target := len(params)

	var results []applyResult
	updated := map[string][]byte{}
	root := s.getRoot()
	for file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		sites, err := symbols.FindCallSites(language, content, funcName)
		if err != nil || len(sites) == 0 {
			continue
		}
		type rewrite struct {
			start, end int
			newText    string
		}
		var edits []rewrite
		for _, cs := range sites {
			if cs.Skipped != "" {
				continue
			}
			if len(cs.CurrentArgs) == target {
				continue
			}
			newInner, err := symbols.RewriteCallSiteArgs(language, cs.CurrentArgs, symParams)
			if err != nil {
				continue
			}
			edits = append(edits, rewrite{
				start:   cs.ArgsInnerStart,
				end:     cs.ArgsInnerEnd,
				newText: newInner,
			})
		}
		if len(edits) == 0 {
			continue
		}
		sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
		out := append([]byte(nil), content...)
		for _, e := range edits {
			next := make([]byte, 0, len(out)-(e.end-e.start)+len(e.newText))
			next = append(next, out[:e.start]...)
			next = append(next, e.newText...)
			next = append(next, out[e.end:]...)
			out = next
		}
		info, err := os.Stat(file)
		if err != nil {
			continue
		}
		tmp := file + ".poly-lsp-mcp.tmp"
		if err := os.WriteFile(tmp, out, info.Mode().Perm()); err != nil {
			continue
		}
		if err := os.Rename(tmp, file); err != nil {
			_ = os.Remove(tmp)
			continue
		}
		s.refreshFileInIndex(file, out)

		results = append(results, applyResult{
			File:  relPath(file, root),
			Edits: len(edits),
		})
		updated[pathToURI(file)] = out
	}
	return results, updated
}

// toSymbolsParams converts the MCP-side refactorParam shape into the
// symbols-package Param shape (same fields, different package).
func toSymbolsParams(params []refactorParam) []symbols.Param {
	if params == nil {
		return nil
	}
	out := make([]symbols.Param, len(params))
	for i, p := range params {
		out[i] = symbols.Param{Name: p.Name, Type: p.Type}
	}
	return out
}

// nameRangeAfterSignature returns a rangeArgs covering the name of a
// freshly-rewritten signature, so the workspace rename can pin on
// the right token. Line/col are 1-based; computed from byte offsets
// inside `content`.
func nameRangeAfterSignature(orig rangeArgs, sig *symbols.FunctionSignature, content []byte) rangeArgs {
	startLine, startCol := byteOffsetToLineColPos(content, sig.Name.Start)
	endLine, endCol := byteOffsetToLineColPos(content, sig.Name.End)
	return rangeArgs{
		File:      orig.File,
		StartLine: startLine, StartCol: startCol,
		EndLine: endLine, EndCol: endCol,
	}
}

// byteOffsetToLineColPos converts a 0-based byte offset to a 1-based
// (line, col) position. Mirrors lineColToByteOffset's inverse.
func byteOffsetToLineColPos(content []byte, offset int) (int, int) {
	if offset > len(content) {
		offset = len(content)
	}
	line, col := 1, 1
	for i := 0; i < offset; i++ {
		if content[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

func (s *Server) refactorRename(a rangeArgs, newName string, includeComments bool, opts diagnosticOptions) ([]Content, bool, error) {
	idx := s.getIndex()
	if idx == nil {
		return []Content{{Type: "text", Text: "index not built (no workspace root configured)"}}, true, nil
	}
	abs := s.resolveFileArg(a.File)
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", a.File, err)
	}
	name, err := readRangeText(content, a)
	if err != nil {
		return nil, true, err
	}
	if name == "" {
		return nil, true, errors.New("range is empty; pass the identifier's nameStart/nameEnd range")
	}

	resolved := s.buildRenameEdits(name, newName)
	if includeComments {
		// Workspace-wide word-boundary scan picks up positions the
		// index intentionally doesn't see — most commonly comments,
		// docstrings, markdown prose, and config formats we don't
		// have a grammar for. \b anchors mean partial-word matches
		// (thisUserID) are NOT renamed.
		more := s.findCommentMentions(name, newName, resolved)
		resolved = append(resolved, more...)
	}
	byFile := map[string][]resolvedEdit{}
	order := []string{}
	for _, e := range resolved {
		if _, ok := byFile[e.AbsFile]; !ok {
			order = append(order, e.AbsFile)
		}
		byFile[e.AbsFile] = append(byFile[e.AbsFile], e)
	}

	results := make([]applyResult, 0, len(order))
	newContents := map[string][]byte{}
	for _, abs := range order {
		edits := byFile[abs]
		rel := edits[0].RelFile
		n, err := applyFileEdits(abs, edits)
		if err != nil {
			results = append(results, applyResult{File: rel, Skipped: err.Error()})
			continue
		}
		// After write, reparse the file's slice.
		if newContent, err := os.ReadFile(abs); err == nil {
			s.refreshFileInIndex(abs, newContent)
			newContents[pathToURI(abs)] = newContent
		}
		results = append(results, applyResult{File: rel, Edits: n})
	}

	uris := make([]string, 0, len(newContents))
	for u := range newContents {
		uris = append(uris, u)
	}
	sort.Strings(uris)
	diags := s.collectDiagnostics(uris, newContents, opts)

	payload := map[string]any{
		"kind":                 "rename",
		"oldName":              name,
		"newName":              newName,
		"filesChanged":         len(results),
		"results":              results,
		"diagnosticsAvailable": diags.Available,
		"diagnosticsTimedOut":  diags.TimedOut,
		"diagnostics":          diags.Items,
	}
	if diags.DroppedDiagnostics > 0 {
		payload["droppedDiagnostics"] = diags.DroppedDiagnostics
	}
	return jsonContent(payload), false, nil
}

// findCommentMentions runs a workspace-wide word-boundary scan for
// name and returns a resolvedEdit per match whose position isn't
// already covered by `existing`. Used by node_refactor with
// kind=rename + includeComments=true to pick up comments / prose /
// non-indexed file types the symbol index intentionally skips.
//
// Word-boundary anchoring keeps partial-word matches out (`thisUserID`
// won't match `\bUserID\b`). Aliasing safety is implicit: the
// rewriter still replaces the matched bytes verbatim, and we only
// match the exact name.
func (s *Server) findCommentMentions(name, newName string, existing []resolvedEdit) []resolvedEdit {
	root := s.getRoot()
	if root == "" {
		return nil
	}
	type loc struct {
		abs  string
		line int
		col  int
	}
	seen := map[loc]bool{}
	for _, e := range existing {
		seen[loc{e.AbsFile, e.Line, e.Col}] = true
	}
	re, err := regexp.Compile(`\b` + regexp.QuoteMeta(name) + `\b`)
	if err != nil {
		return nil
	}
	var out []resolvedEdit
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipScanDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > maxScanSize {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		newlines := scanNewlineOffsets(data)
		for _, m := range re.FindAllIndex(data, -1) {
			line, col := byteOffsetToLineCol(m[0], newlines)
			key := loc{path, line, col}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, resolvedEdit{
				AbsFile: path,
				RelFile: relPath(path, root),
				Line:    line,
				Col:     col,
				OldText: name,
				NewText: newName,
			})
		}
		return nil
	})
	return out
}

const maxScanSize = 1 << 20 // 1 MiB per file; mirrors the lexical pass

func skipScanDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "__pycache__",
		"dist", "build", ".idea", ".vscode", ".poly-lsp-mcp":
		return true
	}
	return false
}

// scanNewlineOffsets returns the byte offset of every '\n' in data,
// sorted. Used by byteOffsetToLineCol to convert a byte offset into
// (line, col) in O(log n) per match.
func scanNewlineOffsets(data []byte) []int {
	var out []int
	for i, b := range data {
		if b == '\n' {
			out = append(out, i)
		}
	}
	return out
}

// byteOffsetToLineCol converts a byte offset into 1-based (line, col).
// Mirrors offsetToLineCol in internal/bindings — duplicated here so
// internal/mcp doesn't import that package's internals.
func byteOffsetToLineCol(offset int, newlines []int) (int, int) {
	lo, hi := 0, len(newlines)
	for lo < hi {
		mid := (lo + hi) / 2
		if newlines[mid] < offset {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	line := lo + 1
	lineStart := 0
	if lo > 0 {
		lineStart = newlines[lo-1] + 1
	}
	return line, offset - lineStart + 1
}

// -------------------------------------------------------------- rename helpers

// resolvedEdit carries the absolute path alongside the wire-shape
// file path so refactorRename can read/write without re-joining root.
type resolvedEdit struct {
	AbsFile string
	RelFile string
	Line    int
	Col     int
	OldText string
	NewText string
}

// applyResult is the wire shape returned per file the refactor touched
// (or skipped with a reason).
type applyResult struct {
	File    string `json:"file"`
	Edits   int    `json:"edits,omitempty"`
	Skipped string `json:"skipped,omitempty"`
}

// buildRenameEdits plans the rewrites for renaming `name` to `newName`.
// Confidence policy: declared sites win when any are present
// (precision opt-in via poly-lsp-mcp.yaml bindings or schemas), otherwise
// lexical hits as best effort. Aliasing safety: per-site on-disk text
// must equal name; mismatches are skipped so aliasing bindings don't
// substitute the wrong token.
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
	tmp := absFile + ".poly-lsp-mcp.tmp"
	if err := os.WriteFile(tmp, out, info.Mode().Perm()); err != nil {
		return applied, err
	}
	if err := os.Rename(tmp, absFile); err != nil {
		_ = os.Remove(tmp)
		return applied, err
	}
	return applied, nil
}

// -------------------------------------------------------------- shared helpers

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

func bytesIndexNewline(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}

func readRangeText(content []byte, a rangeArgs) (string, error) {
	startOff, ok := lineColToByteOffset(content, a.StartLine, a.StartCol)
	if !ok {
		return "", fmt.Errorf("start out of range: %d:%d", a.StartLine, a.StartCol)
	}
	endOff, ok := lineColToByteOffset(content, a.EndLine, a.EndCol)
	if !ok {
		return "", fmt.Errorf("end out of range: %d:%d", a.EndLine, a.EndCol)
	}
	if endOff > len(content) {
		endOff = len(content)
	}
	if startOff > endOff {
		return "", fmt.Errorf("start byte offset %d > end %d", startOff, endOff)
	}
	return string(content[startOff:endOff]), nil
}

// bindingSummary is the catalog entry shape shared between
// poly-lsp-mcp://bindings (resource) and any future tool that wants the
// catalog. Lives here so resources.go can import it without a circular
// reference.
type bindingSummary struct {
	Name      string     `json:"name"`
	SiteCount int        `json:"siteCount"`
	Languages []string   `json:"languages"`
	Sites     []siteJSON `json:"sites"`
}

func jsonContent(value any) []Content {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return []Content{{Type: "text", Text: fmt.Sprintf("internal: %v", err)}}
	}
	return []Content{{Type: "text", Text: string(raw)}}
}

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

func confidenceLabel(c symbols.Confidence) string {
	switch c {
	case symbols.ConfidenceComment:
		return "comment"
	case symbols.ConfidenceLexical:
		return "lexical"
	case symbols.ConfidenceDeclared:
		return "declared"
	case symbols.ConfidenceLSP:
		return "lsp"
	}
	return "unknown"
}

// resolveFileArg turns a workspace-relative or absolute path from a
// tool argument into an absolute path.
func (s *Server) resolveFileArg(file string) string {
	if filepath.IsAbs(file) {
		return file
	}
	if root := s.getRoot(); root != "" {
		return filepath.Join(root, file)
	}
	return file
}

// languageForFile dispatches by extension via the registry. Returns ""
// if the file's extension isn't registered.
func (s *Server) languageForFile(path string) string {
	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	if ext == "" {
		return ""
	}
	lang := s.registry.LookupByExt(ext)
	if lang == nil {
		return ""
	}
	return lang.Name
}

// _ ensures the fs import isn't dead even if walking helpers move
// around in future refactors.
var _ fs.DirEntry
