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

	"github.com/iodesystems/poly-lsp-mcp/internal/bindings"
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
			Description: "Hierarchical tour of a workspace, directory, or file. Output is one of three JSON object shapes; the KEY is both the type discriminator AND the address, and \"/\" \"#\" ARE the path separators:\n" +
				"  directory: {\"dir\":\"src\", \"/\":[ …child dir/file objects… ]}\n" +
				"  file:      {\"file\":\"src/app.go\", \"lang\":\"go\", \"#\":[ …symbols… ]}\n" +
				"  symbol:    {\"sym\":\"Server.Start\", \"class\":\"method\", \"@\":[22,35]}\n" +
				"A symbol's FULL ADDRESS is the file's `file` value + \"#\" + the symbol's `sym` value, e.g. \"src/app.go#Server.Start\" — pass that as the `node` arg on node_read/edit/delete/references/refactor.\n" +
				"`sym` is the dotted path RELATIVE to the file; nesting is encoded in the dots (Server.Start = method Start of type Server), NOT in nested arrays — a file's `#` is a FLAT, source-ordered list of ALL symbols. Same-named same-class siblings and anonymous members get a 1-based `[n]` suffix (init[1], init[2], Server.[1]); a bare name is the first/only one.\n" +
				"`class` is a normalized kind: func, method, type, struct, interface, class, const, var, field, enum, ctor, module, import (files with no grammar get one whole-file `text` symbol with sym \"\").\n" +
				"`@` is [startLine, endLine] of the declaration (1-based).\n" +
				"`path` (default: workspace root) is workspace-relative or absolute; the root shape matches the path (dir→dir, file→file).\n" +
				"`depth` (default 1; 32 when `grep` is set) = directory levels for a dir, and max dot-count for a file's symbols (depth 1 = top-level only/no dots; depth 2 = one nesting level).\n" +
				"`grep` is an optional regex matched against `sym` (and dir/file basenames); subtrees without a match are pruned, and files expand to their symbols. Use it for symbol/file-name search; use `search` for full-text content search.\n" +
				"`nodeLimit` (default 250) caps how many entries return. When it fires the response adds `truncated:true` + `truncatedReason:\"auto\"|\"nodeLimit\"` + `totalNodes` + `hint`.\n" +
				"Tree-sitter grammars: go / typescript / tsx / python / sql.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":      {"type": "string", "description": "Workspace-relative or absolute path. Default: workspace root."},
    "depth":     {"type": "integer", "minimum": 0, "description": "How many levels to expand. Default: 1, or 32 when grep is set."},
    "grep":      {"type": "string", "description": "Optional regex; only nodes whose name matches (or have a matching descendant) survive."},
    "nodeLimit": {"type": "integer", "minimum": 1, "description": "Cap total entries in the response. Default 250. Triggers truncatedReason / hint metadata when it fires."}
  },
  "required": []
}`),
			Handler: handleStructure,
		},
		"node_references": {
			Name: "node_references",
			Description: "Return every workspace position where an identifier is referenced. " +
				"Address it EITHER with `node`=\"<file>#<sym>\" (from structure; resolves to the identifier automatically) OR with an explicit {file, startLine, startCol, endLine, endCol} range covering just the identifier. The two forms are mutually exclusive. " +
				"Output is grouped by file, reusing the file shape: {\"matches\":[ {\"file\":\"src/app.go\",\"lang\":\"go\",\"#\":[ {\"sym\":\"Start\",\"class\":\"declared\",\"@\":[22,22]}, … ]}, … ]}. Each hit's `class` is its confidence (declared / lexical / comment / lsp) and `@` is [line, line]. " +
				"Output combines lexical hits, declared bindings, and schema-anchored sites — the same union node_refactor would touch on rename.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "node":      {"type": "string", "description": "Node address \"<file>#<sym>\" (alternative to file+range)."},
    "file":      {"type": "string"},
    "startLine": {"type": "integer", "minimum": 1},
    "startCol":  {"type": "integer", "minimum": 1},
    "endLine":   {"type": "integer", "minimum": 1},
    "endCol":    {"type": "integer", "minimum": 1}
  },
  "required": []
}`),
			Handler: handleNodeReferences,
		},
		"node_read": {
			Name: "node_read",
			Description: "Read a file. Only `file` (or `node`) is required; every other field is optional and dispatches the shape: " +
				"no args → whole file, auto-capped at ~2k chars (returns `truncated: true` plus `totalChars` / `totalLines` / `maxLineLength` when the cap kicks in so the agent knows the full size). " +
				"`node`=\"<file>#<sym>\" reads exactly that symbol's whole declaration (resolved from structure; alternative to file+range, mutually exclusive with an explicit range). " +
				"`startLine` (default 1) starts reading at that line; `lineLimit` (default: auto — enough lines to fit ~2k chars) caps how many lines come back; `lineLength` (default: unbounded) truncates each line at that many chars (handy for files with huge lines like minified JS). " +
				"`startLine, startCol, endLine, endCol` together engage byte-precise slicing. " +
				"When any cap fires, the response includes the full source's `totalChars`, `totalLines`, `maxLineLength` so the agent can decide whether to widen and re-call. " +
				"Replaces read_file / cat / sed -n.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "file":       {"type": "string"},
    "node":       {"type": "string", "description": "Node address \"<file>#<sym>\": read that symbol's whole declaration."},
    "startLine":  {"type": "integer", "minimum": 1, "description": "Where to start reading. Default 1."},
    "lineLimit":  {"type": "integer", "minimum": 1, "description": "Max lines returned. Default: enough to fit ~2k chars."},
    "lineLength": {"type": "integer", "minimum": 1, "description": "Truncate each line at this many chars. Default: keep full lines."},
    "startCol":   {"type": "integer", "minimum": 1, "description": "Byte-precise form: required with startLine, endLine, endCol."},
    "endLine":    {"type": "integer", "minimum": 1},
    "endCol":     {"type": "integer", "minimum": 1}
  },
  "required": []
}`),
			Handler: handleNodeRead,
		},
		"node_edit": {
			Name: "node_edit",
			Description: "Atomically edit a file. Input shapes (pick one): " +
				"{node:\"<file>#<sym>\", newText} → replace that symbol's whole declaration with newText (address from structure). " +
				"{file, startLine, startCol, endLine, endCol, newText} → replace that range with newText. " +
				"{file, newText} → create or overwrite the whole file (replaces write_file; parent dirs auto-created); newText must be non-empty — this shape REJECTS an empty/absent newText rather than blanking the file (use node_delete to remove content or the whole file). " +
				"{file, diff} → apply a unified-diff patch (one tool call for non-contiguous multi-region edits; context lines are matched fuzzily — hunk header line numbers are a hint, not a hard requirement — but ambiguous or missing context still errors). " +
				"`node` is mutually exclusive with an explicit range and with diff. Writes are atomic and the response includes LSP diagnostics from any child language server.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "file":      {"type": "string"},
    "node":      {"type": "string", "description": "Node address \"<file>#<sym>\": replace that symbol's whole declaration with newText."},
    "newText":   {"type": "string", "description": "New contents. With range/node: replaces that span. Without either: whole-file create-or-overwrite — must be non-empty, or the call is rejected."},
    "diff":      {"type": "string", "description": "Unified-diff patch. Mutually exclusive with newText/range/node. Context lines are located fuzzily; hunk header numbers are a hint."},
    "startLine": {"type": "integer", "minimum": 1},
    "startCol":  {"type": "integer", "minimum": 1},
    "endLine":   {"type": "integer", "minimum": 1},
    "endCol":    {"type": "integer", "minimum": 1}
  },
  "required": []
}`),
			Handler: handleNodeEdit,
		},
		"node_delete": {
			Name: "node_delete",
			Description: "Delete text or a whole file. Input shapes (pick one): " +
				"{node:\"<file>#<sym>\"} → excise that symbol's whole declaration (file stays, just shorter; address from structure). " +
				"{file, startLine, startCol, endLine, endCol} → atomically remove the text in that range. Equivalent to node_edit{newText:''} but states intent. " +
				"{file} → delete the whole file from disk. Operator-grade destructive — the file is removed and its slice dropped from the symbol index, no temp + Rename undo. " +
				"Range/node deletion is exact: surrounding whitespace / blank lines are not adjusted; use a wider range or follow up with node_edit if you want them trimmed.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "file":      {"type": "string"},
    "node":      {"type": "string", "description": "Node address \"<file>#<sym>\": excise that symbol's whole declaration."},
    "startLine": {"type": "integer", "minimum": 1},
    "startCol":  {"type": "integer", "minimum": 1},
    "endLine":   {"type": "integer", "minimum": 1},
    "endCol":    {"type": "integer", "minimum": 1}
  },
  "required": []
}`),
			Handler: handleNodeDelete,
		},
		"node_refactor": {
			Name: "node_refactor",
			Description: "Composable cross-language refactor. " +
				"Address the target identifier EITHER with `node`=\"<file>#<sym>\" (from structure; resolves to the identifier automatically) OR with an explicit {file, startLine, startCol, endLine, endCol} range on the identifier — mutually exclusive. " +
				"`refactor` is an object selecting one or more ops to apply in a single call: " +
				"`rename` (workspace-wide rename across declared bindings, schema-anchored sites, and lexical hits — with per-site on-disk text verification so aliasing bindings can't substitute the wrong token); " +
				"`params` (rebuild the function's parameter list — go / typescript / python; callers across the workspace get their arg lists rewritten best-effort, padded with language-appropriate zero values on growth, truncated on shrink, spread/splat callers reported as skipped); " +
				"`return` (replace the return type, or insert one into a previously-void / unannotated signature). " +
				"Combine all three to change name + signature + return type in one tool call. " +
				"`includeComments` extends rename to documentation/prose via a word-boundary scan; partial-word matches like `thisUserID` stay untouched. " +
				"Legacy shape `kind=\"rename\", newName=X` still accepted; internally normalized into `refactor:{rename: X}`.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "node":      {"type": "string", "description": "Node address \"<file>#<sym>\" (alternative to file+range)."},
    "file":      {"type": "string"},
    "startLine": {"type": "integer", "minimum": 1},
    "startCol":  {"type": "integer", "minimum": 1},
    "endLine":   {"type": "integer", "minimum": 1},
    "endCol":    {"type": "integer", "minimum": 1},
    "refactor": {
      "type": "object",
      "description": "Composable ops; supply at least one field.",
      "properties": {
        "rename": {"type": "string", "description": "New identifier name."},
        "params": {
          "type": "array",
          "description": "Replacement parameter list. Each entry: {name, type}.",
          "items": {
            "type": "object",
            "properties": {
              "name": {"type": "string"},
              "type": {"type": "string"}
            },
            "required": ["name", "type"]
          }
        },
        "return": {"type": "string", "description": "New return type (Go tuples: include parens, e.g., \"(string, error)\")."}
      }
    },
    "kind":            {"type": "string", "enum": ["rename"], "description": "Legacy shape. Prefer refactor:{rename: X}."},
    "newName":         {"type": "string", "description": "Required when kind='rename'."},
    "includeComments": {"type": "boolean", "description": "Rename word-boundary mentions in comments / prose / non-indexed file types too. Default false."},
    "applyCandidates": {"type": "boolean", "description": "Also rename the lexical name-match sites that are cross-namespace GUESSES. Default false: those sites are returned under 'candidates' (recommendations) and NOT touched — review them, then re-run with applyCandidates:true. Authoritative sites (declared @derived/binding, child-LSP) are always renamed."},
    "resolution": {"type": "object", "description": "Resolves the variance when the target is a @derived source (a derivation-graph node — gat operation / sqlc column). Without it, such a rename is NOT applied and returns variance:true with the modes to choose from. mode='underlying' cascades the rename to the source + all references (the only automated mode); projection/mapping/hide are recognized but manual. target='file:line' picks one source when fan-in is ambiguous.", "properties": {"mode": {"type": "string", "enum": ["underlying", "projection", "mapping", "hide"]}, "target": {"type": "string"}}}
  },
  "required": []
}`),
			Handler: handleNodeRefactor,
		},
		"search": {
			Name: "search",
			Description: "Regex search over file contents across the workspace. " +
				"`pattern` is a Go regex (use `(?i)…` inline for case-insensitive). " +
				"`path` (default: workspace root) scopes the walk. " +
				"`glob` (filepath.Match pattern over file basenames, e.g. `*.go`) filters which files get scanned. " +
				"`limit` (default 100) caps hits; overflow surfaces as `droppedMatches`. " +
				"`contextLines` (default 0) returns N lines before AND after each match for previewing. " +
				"Matches are grouped by file, reusing the structure file shape: {\"matches\":[ {\"file\":…,\"lang\":…,\"#\":[ {\"sym\":\"<enclosing symbol or empty>\",\"class\":\"match\",\"@\":[line,line],\"col\":N,\"text\":\"<matched line>\"}, … ]}, … ]}. `sym` names the enclosing symbol when one is resolvable (so you can node_read it via \"<file>#<sym>\"); `class` is always \"match\". " +
				"Use this for full-text search — comment hunting, finding stringly-typed magic values, etc. " +
				"For symbol/file-NAME search use structure(grep=…) instead — it's tree-sitter aware. " +
				"Binary files, the standard noise dirs (.git / node_modules / vendor / __pycache__ / dist / build), and files > 1 MiB are skipped silently.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern":      {"type": "string"},
    "path":         {"type": "string", "description": "Workspace-relative or absolute. Default: workspace root."},
    "glob":         {"type": "string", "description": "filepath.Match pattern over basenames. Default: every file."},
    "limit":        {"type": "integer", "minimum": 1, "description": "Max hits. Default 100."},
    "contextLines": {"type": "integer", "minimum": 0, "description": "Lines before/after each match. Default 0."}
  },
  "required": ["pattern"]
}`),
			Handler: handleSearch,
		},
		"node_rename_file": {
			Name: "node_rename_file",
			Description: "Move/rename a file AND rewrite the import specifiers that point at it across the " +
				"workspace (the workspace/willRenameFiles capability). Without this a file rename leaves every " +
				"importer with a dangling path. TS/JS today: relative specifiers (extensionless or extensioned), " +
				"`import`/`export … from`, dynamic `import()`, and `require()`. Go imports are package-based " +
				"(a same-package move needs no edit); Python dotted imports are not yet rewritten.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "from": {"type": "string", "description": "Existing file path (workspace-relative or absolute)."},
    "to":   {"type": "string", "description": "New file path. Parent dirs are created; must not already exist."}
  },
  "required": ["from", "to"]
}`),
			Handler: handleNodeRenameFile,
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

// -------------------------------------------------------------- search

func handleSearch(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var p struct {
		Pattern      string `json:"pattern"`
		Path         string `json:"path"`
		Glob         string `json:"glob"`
		Limit        *int   `json:"limit"`
		ContextLines *int   `json:"contextLines"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}
	if p.Pattern == "" {
		return nil, true, errors.New("pattern is required")
	}
	re, err := regexp.Compile(p.Pattern)
	if err != nil {
		return nil, true, fmt.Errorf("invalid pattern: %w", err)
	}
	if p.Path == "" {
		p.Path = "."
	}
	root := s.resolveFileArg(p.Path)
	limit := 100
	if p.Limit != nil {
		if *p.Limit < 1 {
			return nil, true, errors.New("limit must be >= 1")
		}
		limit = *p.Limit
	}
	ctxLines := 0
	if p.ContextLines != nil {
		if *p.ContextLines < 0 {
			return nil, true, errors.New("contextLines must be >= 0")
		}
		ctxLines = *p.ContextLines
	}

	hits, dropped, err := symbols.Search(root, re, symbols.SearchOptions{
		Glob:         p.Glob,
		Limit:        limit,
		ContextLines: ctxLines,
	})
	if err != nil {
		return nil, true, err
	}

	// Group hits by file into the shared file shape. Each text hit
	// becomes a sym entry with class "match": `sym` carries the
	// enclosing symbol path when resolvable (for navigation), `@` is
	// the hit's [line, line], and `text` is the matched line.
	var order []string
	byFile := map[string][]any{}
	fileLang := map[string]string{}
	symCache := map[string][]symbols.Symbol{}
	for _, h := range hits {
		rel := relPath(h.File, s.getRoot())
		if _, ok := byFile[rel]; !ok {
			order = append(order, rel)
			fileLang[rel] = s.languageForFile(h.File)
		}
		m := map[string]any{
			"sym":   s.enclosingSymPath(h.File, h.Line, symCache),
			"class": "match",
			"@":     []int{h.Line, h.Line},
			"col":   h.Col,
			"text":  h.Text,
		}
		if len(h.Before) > 0 {
			m["before"] = h.Before
		}
		if len(h.After) > 0 {
			m["after"] = h.After
		}
		byFile[rel] = append(byFile[rel], m)
	}
	matches := make([]any, 0, len(order))
	for _, f := range order {
		fm := map[string]any{"file": f}
		if fileLang[f] != "" {
			fm["lang"] = fileLang[f]
		}
		fm["#"] = byFile[f]
		matches = append(matches, fm)
	}
	payload := map[string]any{
		"totalMatches": len(hits),
		"matches":      matches,
	}
	if dropped > 0 {
		payload["droppedMatches"] = dropped
	}
	return jsonContent(payload), false, nil
}

// defaultStructureNodeLimit caps how many entries a structure call
// returns when the agent doesn't specify nodeLimit. Picked to keep
// responses contained without forcing the agent to think about
// pagination on every call — most files and dirs are smaller.
const defaultStructureNodeLimit = 250

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
	var wrap struct {
		Node string `json:"node"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}
	_ = json.Unmarshal(args, &wrap)
	// node addressing resolves to the identifier (name) range.
	if wrap.Node != "" {
		if a.StartLine != 0 || a.StartCol != 0 || a.EndLine != 0 || a.EndCol != 0 {
			return nil, true, errors.New("pass either node or an explicit range, not both")
		}
		rn, err := s.resolveNodeAddr(wrap.Node)
		if err != nil {
			return nil, true, err
		}
		a = rn.nameRange
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
		return nil, true, errors.New("range is empty; pass the identifier's name range (or a node address)")
	}

	var items []matchItem
	for _, site := range idx.LookupExisting(name) {
		rel := relPath(site.File, s.getRoot())
		items = append(items, matchItem{
			file:  rel,
			lang:  site.Language,
			sym:   name,
			class: confidenceLabel(site.Confidence),
			at:    [2]int{site.Line, site.Line},
		})
	}
	return jsonContent(groupedMatches(items)), false, nil
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

// nodeReadArgs accepts the three polymorphic input shapes for
// node_read. All fields except File are optional; the handler
// detects which shape the caller meant.
// nodeReadArgs is the polymorphic input for node_read. ALL fields are
// optional except file. The handler picks a shape from which fields
// were set; sensible defaults fill the rest.
type nodeReadArgs struct {
	File string `json:"file"`

	// Node address ("<file>#<sym>") — an alternative to file+range.
	// Resolves to the symbol's whole declaration range.
	Node string `json:"node,omitempty"`

	// Line-based reading. StartLine alone (or no args at all) is the
	// common case; LineLimit / LineLength tune the size budget.
	StartLine  *int `json:"startLine,omitempty"`
	LineLimit  *int `json:"lineLimit,omitempty"`  // max lines returned
	LineLength *int `json:"lineLength,omitempty"` // truncate each line at this many chars

	// Byte-precise range form. All four (with StartLine) must be set
	// together to engage. StartCol presence is the disambiguator vs
	// the line-based form.
	StartCol *int `json:"startCol,omitempty"`
	EndLine  *int `json:"endLine,omitempty"`
	EndCol   *int `json:"endCol,omitempty"`

	// Legacy aliases — the v0.2 names. Accepted but discouraged in
	// the description.
	Line   *int `json:"line,omitempty"`
	Limit  *int `json:"limit,omitempty"`
	Offset *int `json:"offset,omitempty"`
}

// defaultReadCharBudget is the implicit cap when the agent doesn't
// set lineLimit explicitly. Tuned to be "a reasonable preview" —
// usually 30-60 lines of code, well under typical context budgets.
const defaultReadCharBudget = 2048

func handleNodeRead(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var a nodeReadArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}
	// Node address → whole-declaration byte-precise range.
	if a.Node != "" {
		if a.StartLine != nil || a.StartCol != nil || a.EndLine != nil || a.EndCol != nil {
			return nil, true, errors.New("pass either node or an explicit range, not both")
		}
		rn, err := s.resolveNodeAddr(a.Node)
		if err != nil {
			return nil, true, err
		}
		d := rn.declRange
		a.File = d.File
		a.StartLine, a.StartCol, a.EndLine, a.EndCol = &d.StartLine, &d.StartCol, &d.EndLine, &d.EndCol
	}
	if a.File == "" {
		return nil, true, errors.New("file is required")
	}

	// Legacy alias mapping.
	if a.Line != nil && a.StartLine == nil {
		a.StartLine = a.Line
	}
	if a.Limit != nil && a.LineLimit == nil {
		a.LineLimit = a.Limit
	}
	if a.Offset != nil && a.StartLine != nil {
		// Legacy `offset` advanced past `line` to compute the real
		// start. Fold it in for back-compat.
		adjusted := *a.StartLine + *a.Offset
		a.StartLine = &adjusted
	}

	hasRange := a.StartCol != nil || a.EndLine != nil || a.EndCol != nil
	hasLineCaps := a.LineLimit != nil || a.LineLength != nil
	abs := s.resolveFileArg(a.File)
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", a.File, err)
	}

	if hasRange {
		if a.StartLine == nil || a.StartCol == nil || a.EndLine == nil || a.EndCol == nil {
			return nil, true, errors.New("byte-precise range form needs startLine, startCol, endLine, and endCol all set")
		}
		if hasLineCaps {
			return nil, true, errors.New("cannot combine byte-precise range form with lineLimit / lineLength")
		}
		r := rangeArgs{
			File:      a.File,
			StartLine: *a.StartLine, StartCol: *a.StartCol,
			EndLine: *a.EndLine, EndCol: *a.EndCol,
		}
		if err := r.validate(); err != nil {
			return nil, true, err
		}
		text, err := readRangeText(content, r)
		if err != nil {
			return nil, true, err
		}
		return jsonContent(map[string]any{
			"file":      a.File,
			"startLine": r.StartLine, "startCol": r.StartCol,
			"endLine": r.EndLine, "endCol": r.EndCol,
			"text": text,
		}), false, nil
	}

	// Line-based read. Defaults:
	//   startLine = 1
	//   lineLength = unbounded
	//   lineLimit = auto (fit ~defaultReadCharBudget chars)
	startLine := 1
	if a.StartLine != nil {
		startLine = *a.StartLine
	}
	if startLine < 1 {
		return nil, true, errors.New("startLine must be >= 1")
	}
	lineLength := 0
	if a.LineLength != nil {
		if *a.LineLength < 1 {
			return nil, true, errors.New("lineLength must be >= 1")
		}
		lineLength = *a.LineLength
	}
	var lineLimit int // 0 means "auto"
	if a.LineLimit != nil {
		if *a.LineLimit < 1 {
			return nil, true, errors.New("lineLimit must be >= 1")
		}
		lineLimit = *a.LineLimit
	}

	out := buildReadPayload(content, a.File, startLine, lineLimit, lineLength)
	return jsonContent(out), false, nil
}

// buildReadPayload builds the response map for the line-based read
// shape. Splits content, applies startLine, optional per-line
// truncation, then takes either the user's lineLimit lines or as
// many as fit in defaultReadCharBudget. Whenever the returned slice
// is shorter than the full source (or per-line truncation kicked
// in), the response includes the full source's totalChars /
// totalLines / maxLineLength so the agent can decide whether to
// widen and re-call.
//
// truncatedReason distinguishes WHY the slice was clipped:
//
//	"auto"       → no user-set lineLimit; the implicit ~2k char
//	               budget fired. The most surprising case for the
//	               agent — a `hint` is included pointing at how to
//	               widen.
//	"lineLimit"  → user asked for fewer lines than the source has.
//	"lineLength" → individual lines truncated by user's lineLength
//	               with no line-count truncation.
//
// When multiple causes apply at once, the priority is auto >
// lineLimit > lineLength and the hint mentions all of them.
func buildReadPayload(content []byte, file string, startLine, lineLimit, lineLength int) map[string]any {
	lines := splitNodeReadLines(content)
	totalLines := len(lines)
	totalChars := len(content)

	// Maximum line length in the SOURCE (pre-truncation). Useful
	// signal that the agent is asking about a file with very long
	// lines (minified JS, generated code).
	maxLineLen := 0
	for _, l := range lines {
		if len(l) > maxLineLen {
			maxLineLen = len(l)
		}
	}

	out := map[string]any{
		"file": file,
	}
	if startLine > totalLines {
		out["startLine"] = startLine
		out["endLine"] = startLine
		out["text"] = ""
		out["truncated"] = true
		out["truncatedReason"] = "past-eof"
		out["hint"] = fmt.Sprintf("startLine=%d is past the last line (file has %d lines).", startLine, totalLines)
		out["totalLines"] = totalLines
		out["totalChars"] = totalChars
		out["maxLineLength"] = maxLineLen
		return out
	}

	// Take the slice from startLine onward, truncating per-line if
	// requested. Char accounting tracks the projected size after
	// any truncation has applied.
	collected := make([]string, 0, totalLines-(startLine-1))
	charsCollected := 0
	autoLimit := lineLimit == 0
	budget := defaultReadCharBudget
	endLine := startLine - 1
	for i := startLine - 1; i < totalLines; i++ {
		ln := lines[i]
		if lineLength > 0 && len(ln) > lineLength {
			ln = ln[:lineLength] + "…"
		}
		// Per-line "+1" accounts for the rejoining \n.
		cost := len(ln) + 1
		if autoLimit {
			if charsCollected+cost > budget && len(collected) > 0 {
				break
			}
		} else if len(collected) >= lineLimit {
			break
		}
		collected = append(collected, ln)
		charsCollected += cost
		endLine = i + 1
	}

	text := strings.Join(collected, "\n")
	// Preserve a trailing newline if the source had one AND we read
	// through to the last line. Mirrors the file's natural shape so
	// a whole-file read returns bytes exactly as on disk.
	if endLine == totalLines && len(content) > 0 && content[len(content)-1] == '\n' {
		text += "\n"
	}

	out["startLine"] = startLine
	out["endLine"] = endLine
	out["text"] = text

	// Classify the truncation: did the line count get clipped?
	// Did individual lines get truncated by lineLength?
	clippedByCount := endLine < totalLines
	clippedByLength := lineLength > 0 && maxLineLen > lineLength
	if !clippedByCount && !clippedByLength {
		return out
	}

	// Choose the dominant reason for the agent's primary signal.
	reason := ""
	switch {
	case clippedByCount && autoLimit:
		reason = "auto"
	case clippedByCount && !autoLimit:
		reason = "lineLimit"
	case clippedByLength:
		reason = "lineLength"
	}

	// Hint sentence — what the agent should do next. Spell out ALL the
	// alternatives, because the common failure is blind chunk-by-chunk
	// paging: an agent reads one ~2k-char slice, sees "continue", reads
	// the next, and burns a tool call (a whole turn) per slice to read
	// one file. Lead with the two ways to AVOID paging — widen in one
	// call, or target with search — and only then offer to keep paging.
	pageSize := endLine - startLine + 1
	if pageSize < 1 {
		pageSize = 1
	}
	morePages := (totalLines - endLine + pageSize - 1) / pageSize
	var hint strings.Builder
	switch reason {
	case "auto":
		fmt.Fprintf(&hint, "Returned lines %d-%d of %d (auto-capped at ~%d chars; ~%d more read(s) to page the rest). "+
			"Avoid paging chunk-by-chunk: re-read with lineLimit=%d to get the whole file in ONE call, "+
			"or use the search tool (pattern=<regex>, contextLines=3) to jump straight to the code you need. "+
			"To keep paging anyway, call again with startLine=%d.",
			startLine, endLine, totalLines, defaultReadCharBudget, morePages, totalLines, endLine+1)
	case "lineLimit":
		fmt.Fprintf(&hint, "Returned lines %d-%d of %d (lineLimit=%d; ~%d more read(s) to finish). "+
			"Re-read with lineLimit=%d for the whole file in one call, or use the search tool (pattern=<regex>) to target a section, "+
			"or call again with startLine=%d to continue.",
			startLine, endLine, totalLines, lineLimit, morePages, totalLines, endLine+1)
	case "lineLength":
		fmt.Fprintf(&hint, "Lines truncated to %d chars (max source line was %d chars). Pass a larger lineLength to keep full lines.",
			lineLength, maxLineLen)
	}
	if clippedByLength && reason != "lineLength" {
		fmt.Fprintf(&hint, " Also: lines truncated to %d chars (max source line was %d).", lineLength, maxLineLen)
	}

	out["truncated"] = true
	out["truncatedReason"] = reason
	out["hint"] = hint.String()
	out["totalLines"] = totalLines
	out["totalChars"] = totalChars
	out["maxLineLength"] = maxLineLen
	return out
}

// splitNodeReadLines splits content on \n, drops the trailing empty
// entry produced by a final newline so endLine math matches the
// editor's reckoning, and strips \r from CRLF inputs.
func splitNodeReadLines(content []byte) []string {
	lines := strings.Split(string(content), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// -------------------------------------------------------------- node_edit / node_delete

// nodeEditArgs accepts the three polymorphic input shapes for
// node_edit. Pointers distinguish "set" from zero so we can detect
// which shape the caller meant.
type nodeEditArgs struct {
	File    string `json:"file"`
	NewText string `json:"newText"`
	Diff    string `json:"diff"`

	// Node address ("<file>#<sym>") — selects the symbol's whole
	// declaration range as the span to replace with newText.
	Node string `json:"node,omitempty"`

	// Range form (existing).
	StartLine *int `json:"startLine,omitempty"`
	StartCol  *int `json:"startCol,omitempty"`
	EndLine   *int `json:"endLine,omitempty"`
	EndCol    *int `json:"endCol,omitempty"`

	diagnosticOptions
}

func handleNodeEdit(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var p nodeEditArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}

	hasRange := p.StartLine != nil || p.StartCol != nil || p.EndLine != nil || p.EndCol != nil
	hasDiff := p.Diff != ""

	// Node address → selects the declaration range for a newText
	// replace (diff mode is unaffected; a node is mutually exclusive
	// with an explicit range and with diff).
	if p.Node != "" {
		if hasRange {
			return nil, true, errors.New("pass either node or an explicit range, not both")
		}
		if hasDiff {
			return nil, true, errors.New("cannot combine node with diff")
		}
		rn, err := s.resolveNodeAddr(p.Node)
		if err != nil {
			return nil, true, err
		}
		return s.applyRangeRewrite(rn.declRange, p.NewText, p.diagnosticOptions)
	}

	if p.File == "" {
		return nil, true, errors.New("file is required")
	}
	switch {
	case hasDiff && hasRange:
		return nil, true, errors.New("cannot combine diff with range form")
	case hasDiff && p.NewText != "":
		return nil, true, errors.New("cannot combine diff with newText")
	case hasDiff:
		return s.applyDiffRewrite(p.File, p.Diff, p.diagnosticOptions)
	case hasRange:
		if p.StartLine == nil || p.StartCol == nil || p.EndLine == nil || p.EndCol == nil {
			return nil, true, errors.New("range form requires all of startLine, startCol, endLine, endCol")
		}
		return s.applyRangeRewrite(rangeArgs{
			File:      p.File,
			StartLine: *p.StartLine, StartCol: *p.StartCol,
			EndLine: *p.EndLine, EndCol: *p.EndCol,
		}, p.NewText, p.diagnosticOptions)
	default:
		// {file, newText} no range → whole-file create-or-overwrite.
		// Empty/absent newText is REJECTED: there's no legitimate
		// agent reason to blank a file via node_edit (node_delete
		// exists for range removal, and node_delete{file} removes the
		// file outright). This guards the failure mode that destroyed
		// a file in practice: an unrecognized field name resolved to
		// an empty newText, which silently truncated the file to
		// zero bytes.
		if p.NewText == "" {
			return nil, true, fmt.Errorf(
				"node_edit whole-file overwrite with empty newText would erase %s; pass non-empty newText, or use node_delete for range removal",
				p.File)
		}
		return s.applyWholeFileWrite(p.File, p.NewText, p.diagnosticOptions)
	}
}

// nodeDeleteArgs accepts the two polymorphic input shapes for
// node_delete. Same pattern as node_edit / node_read.
type nodeDeleteArgs struct {
	File      string `json:"file"`
	Node      string `json:"node,omitempty"`
	StartLine *int   `json:"startLine,omitempty"`
	StartCol  *int   `json:"startCol,omitempty"`
	EndLine   *int   `json:"endLine,omitempty"`
	EndCol    *int   `json:"endCol,omitempty"`
	diagnosticOptions
}

func handleNodeDelete(s *Server, args json.RawMessage) ([]Content, bool, error) {
	var p nodeDeleteArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}
	hasRange := p.StartLine != nil || p.StartCol != nil || p.EndLine != nil || p.EndCol != nil
	// Node address → remove the symbol's whole declaration range (the
	// file stays; the declaration is excised).
	if p.Node != "" {
		if hasRange {
			return nil, true, errors.New("pass either node or an explicit range, not both")
		}
		rn, err := s.resolveNodeAddr(p.Node)
		if err != nil {
			return nil, true, err
		}
		return s.applyRangeRewrite(rn.declRange, "", p.diagnosticOptions)
	}
	if p.File == "" {
		return nil, true, errors.New("file is required")
	}
	if hasRange {
		if p.StartLine == nil || p.StartCol == nil || p.EndLine == nil || p.EndCol == nil {
			return nil, true, errors.New("range form requires all of startLine, startCol, endLine, endCol")
		}
		return s.applyRangeRewrite(rangeArgs{
			File:      p.File,
			StartLine: *p.StartLine, StartCol: *p.StartCol,
			EndLine: *p.EndLine, EndCol: *p.EndCol,
		}, "", p.diagnosticOptions)
	}
	// {file} no range → delete the whole file. Operator-grade
	// destructive: the file is removed from disk and its slice
	// dropped from the index.
	return s.applyWholeFileDelete(p.File, p.diagnosticOptions)
}

// applyWholeFileDelete removes the file from disk + drops its slice
// from the symbol index. Distinct from a range-delete (which leaves
// the file in place) because the action is irreversible from the
// agent's side — no temp + Rename safety net, just os.Remove.
//
// Returns an error when the file doesn't exist so the agent gets a
// clear "already gone" signal instead of silent success.
func (s *Server) applyWholeFileDelete(file string, opts diagnosticOptions) ([]Content, bool, error) {
	abs := s.resolveFileArg(file)
	info, err := os.Stat(abs)
	if err != nil {
		return nil, true, fmt.Errorf("stat %s: %w", file, err)
	}
	if info.IsDir() {
		return nil, true, fmt.Errorf("%s is a directory; node_delete doesn't recurse", file)
	}
	bytesRemoved := info.Size()
	if err := os.Remove(abs); err != nil {
		return nil, true, fmt.Errorf("remove %s: %w", file, err)
	}
	// Drop the file's slice from the index. There's no
	// "refresh-to-empty" — we synthesize an empty extractor result.
	if idx := s.getIndex(); idx != nil {
		if lang := s.languageForFile(abs); lang != "" {
			idx.Refresh(abs, lang, nil)
		}
	}

	uri := pathToURI(abs)
	// No content for the diagnostic round-trip — the file is gone.
	// gopls reacts to file-on-disk disappearance via its own
	// watcher (or won't react at all in headless MCP); we just
	// pass an empty content map.
	diags := s.collectDiagnostics([]string{uri}, map[string][]byte{}, opts)
	payload := map[string]any{
		"file":                 file,
		"deleted":              true,
		"bytesRemoved":         bytesRemoved,
		"diagnosticsAvailable": diags.Available,
		"diagnosticsTimedOut":  diags.TimedOut,
		"diagnostics":          diags.Items,
	}
	if diags.DroppedDiagnostics > 0 {
		payload["droppedDiagnostics"] = diags.DroppedDiagnostics
	}
	return jsonContent(payload), false, nil
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

// applyWholeFileWrite is the {file, newText} no-range branch: create
// the file if missing, otherwise overwrite the whole contents. Same
// atomic temp + Rename path the range form uses; preserves the
// existing file's mode when overwriting, defaults to 0644 when
// creating.
func (s *Server) applyWholeFileWrite(file, newText string, opts diagnosticOptions) ([]Content, bool, error) {
	abs := s.resolveFileArg(file)
	created := false
	mode := os.FileMode(0o644)
	bytesRemoved := 0
	if info, err := os.Stat(abs); err == nil {
		mode = info.Mode().Perm()
		if existing, err := os.ReadFile(abs); err == nil {
			bytesRemoved = len(existing)
		}
	} else if os.IsNotExist(err) {
		created = true
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return nil, true, fmt.Errorf("mkdir parent: %w", err)
		}
	} else {
		return nil, true, fmt.Errorf("stat %s: %w", file, err)
	}

	out := []byte(newText)
	tmp := abs + ".poly-lsp-mcp.tmp"
	if err := os.WriteFile(tmp, out, mode); err != nil {
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
		"file":                 file,
		"created":              created,
		"bytesRemoved":         bytesRemoved,
		"bytesAdded":           len(out),
		"diagnosticsAvailable": diags.Available,
		"diagnosticsTimedOut":  diags.TimedOut,
		"diagnostics":          diags.Items,
	}
	if diags.DroppedDiagnostics > 0 {
		payload["droppedDiagnostics"] = diags.DroppedDiagnostics
	}
	return jsonContent(payload), false, nil
}

// applyDiffRewrite is the {file, diff} branch: parse the unified
// diff, apply against the file's current content, atomic write
// back. Errors surface the hunk/line that caused the mismatch so the
// agent can regenerate the patch against fresh content. File must
// already exist — create-on-write goes through the no-range path.
func (s *Server) applyDiffRewrite(file, diff string, opts diagnosticOptions) ([]Content, bool, error) {
	abs := s.resolveFileArg(file)
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", file, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, true, fmt.Errorf("stat %s: %w", file, err)
	}
	out, err := ApplyUnifiedDiff(content, diff)
	if err != nil {
		return nil, true, fmt.Errorf("apply diff: %w", err)
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

	uri := pathToURI(abs)
	diags := s.collectDiagnostics([]string{uri}, map[string][]byte{uri: out}, opts)
	payload := map[string]any{
		"file":                 file,
		"bytesRemoved":         len(content),
		"bytesAdded":           len(out),
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
		// ApplyCandidates also renames the lexical name-match sites that are
		// guesses across namespaces (otherwise they're returned as candidates,
		// not touched). Explicit intent to act on a guess.
		ApplyCandidates bool `json:"applyCandidates"`
		// Resolution resolves the variance when the rename target is a @derived
		// source (a derivation graph node): mode ∈ underlying|projection|mapping|hide,
		// target picks one source when fan-in is ambiguous. Absent → fail-closed.
		Resolution struct {
			Mode   string `json:"mode"`
			Target string `json:"target"`
		} `json:"resolution"`
		// Node address ("<file>#<sym>") — resolves to the symbol's
		// identifier range (what a refactor pins on).
		Node string `json:"node"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, true, fmt.Errorf("bad arguments: %w", err)
	}
	if p.Node != "" {
		if p.rangeArgs.StartLine != 0 || p.rangeArgs.StartCol != 0 || p.rangeArgs.EndLine != 0 || p.rangeArgs.EndCol != 0 {
			return nil, true, errors.New("pass either node or an explicit range, not both")
		}
		rn, err := s.resolveNodeAddr(p.Node)
		if err != nil {
			return nil, true, err
		}
		p.rangeArgs = rn.nameRange
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
		return s.refactorRename(p.rangeArgs, ops.Rename, p.IncludeComments, p.ApplyCandidates, p.Resolution.Mode, p.Resolution.Target, p.diagnosticOptions)
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
		renameContent, renameIsErr, renameErr := s.refactorRename(nameRangeArgs, ops.Rename, includeComments, false, "", "", diagnosticOptions{})
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

func (s *Server) refactorRename(a rangeArgs, newName string, includeComments, applyCandidates bool, mode, target string, opts diagnosticOptions) ([]Content, bool, error) {
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

	// Variance model: when name is a @derived source (a derivation-graph node), a
	// rename is mode-ambiguous and is NOT applied until resolution.mode says how.
	if roots := s.getDerivRoots(name); len(roots) > 0 {
		switch mode {
		case "":
			return s.varianceResponse(name, roots), false, nil
		case "underlying":
			if len(roots) > 1 && target == "" {
				return s.varianceResponse(name, roots), false, nil // fan-in: pick a source
			}
			applyCandidates = true // cascade: rename the source + every reference
		case "projection", "mapping", "hide":
			return s.modeNotAutomatedResponse(name, mode, roots), false, nil
		default:
			return nil, true, fmt.Errorf("unknown resolution mode %q (use underlying|projection|mapping|hide)", mode)
		}
	}

	resolved, candidates := s.buildRenameEdits(name, newName, applyCandidates)
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
	// Guessed (lexical, cross-namespace) sites are RECOMMENDED, not actioned —
	// surface them so the caller can review and opt in with applyCandidates:true.
	if len(candidates) > 0 {
		root := s.getRoot()
		recs := make([]map[string]any, 0, len(candidates))
		for _, c := range candidates {
			recs = append(recs, map[string]any{
				"file": relPath(c.File, root), "line": c.Line, "col": c.Col,
				"language": c.Language, "confidence": confidenceLabel(c.Confidence),
			})
		}
		payload["candidates"] = recs
		payload["candidatesNote"] = "lexical name-match sites NOT renamed (a guess across namespaces). " +
			"Review and re-run with applyCandidates:true to include them."
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

// buildRenameEdits plans the rewrites for renaming `name` to `newName`. Returns the
// edits to apply plus the lexical CANDIDATES that were NOT actioned (guessed sites
// held back unless applyCandidates is set — see chooseRenameSites). Aliasing safety:
// per-site on-disk text must equal name; mismatches are skipped so aliasing bindings
// don't substitute the wrong token.
func (s *Server) buildRenameEdits(name, newName string, applyCandidates bool) ([]resolvedEdit, []symbols.Site) {
	idx := s.getIndex()
	if idx == nil {
		return nil, nil
	}
	action, cands := chooseRenameSites(idx.LookupExisting(name))
	sites := action
	if applyCandidates {
		sites = append(append([]symbols.Site(nil), action...), cands...)
		cands = nil // actioned, so not reported as outstanding candidates
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
	return out, cands
}

// chooseRenameSites partitions Lookup results into the sites a rename should ACTION
// by default vs the lexical guesses it should only RECOMMEND. When any authoritative
// site exists (declared @derived/binding or a child-LSP result), those are actioned
// and the lexical name-matches are returned as candidates — a guess that needs
// explicit intent (applyCandidates) before it's touched. With no authoritative site
// (e.g. a single-language lexical rename), the lexical sites ARE the rename.
func chooseRenameSites(all []symbols.Site) (action, candidates []symbols.Site) {
	var declared, lexical []symbols.Site
	for _, s := range all {
		if s.Confidence >= symbols.ConfidenceDeclared {
			declared = append(declared, s)
		} else {
			lexical = append(lexical, s)
		}
	}
	if len(declared) > 0 {
		return declared, lexical
	}
	return lexical, nil
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
	// Compact (not indented): this output is consumed by an LLM, which parses
	// compact JSON fine — indentation is pure token waste, and the terse
	// structure/search shapes exist precisely to minimize tokens.
	raw, err := json.Marshal(value)
	if err != nil {
		return []Content{{Type: "text", Text: fmt.Sprintf("internal: %v", err)}}
	}
	return []Content{{Type: "text", Text: string(raw)}}
}

// varianceResponse is the fail-closed result for renaming a @derived source without a
// resolution mode: nothing is applied; the caller gets the source(s) and the modes to
// choose from. Re-run with resolution:{mode,target} to act.
func (s *Server) varianceResponse(name string, roots []bindings.DerivRoot) []Content {
	root := s.getRoot()
	srcs := make([]map[string]any, 0, len(roots))
	for _, r := range roots {
		srcs = append(srcs, map[string]any{
			"kind": r.Kind, "file": relPath(r.Source.File, root), "line": r.Source.Line, "col": r.Source.Col,
		})
	}
	howto := "re-run with resolution:{mode:'underlying'} to cascade the rename to the source + all references."
	if len(roots) > 1 {
		howto = fmt.Sprintf("fan-in: %d sources share this name — re-run with resolution:{mode:'underlying', target:'<file:line>'} to pick one.", len(roots))
	}
	return jsonContent(map[string]any{
		"variance": true,
		"applied":  false,
		"name":     name,
		"reason":   "this symbol is a @derived source (a derivation-graph node); a rename is mode-ambiguous and was NOT applied.",
		"sources":  srcs,
		"modes": []map[string]any{
			{"mode": "underlying", "automated": true, "effect": "rename the source and cascade every reference; the generated/derived layers regenerate."},
			{"mode": "projection", "automated": false, "effect": "rename only a specific projection/alias, leaving the source (manual)."},
			{"mode": "mapping", "automated": false, "effect": "keep the source name; add an alias so the derived name changes (manual)."},
			{"mode": "hide", "automated": false, "effect": "on delete: drop from a view / json tag instead of dropping the source (manual)."},
		},
		"howToProceed": howto,
	})
}

// modeNotAutomatedResponse handles a recognized-but-manual resolution mode.
func (s *Server) modeNotAutomatedResponse(name, mode string, roots []bindings.DerivRoot) []Content {
	return jsonContent(map[string]any{
		"variance":     true,
		"applied":      false,
		"name":         name,
		"mode":         mode,
		"reason":       "mode '" + mode + "' is recognized but not automated; only 'underlying' (cascade rename of the source) applies automatically.",
		"howToProceed": "apply '" + mode + "' by hand (alias / view / tag edit), or use resolution:{mode:'underlying'} to cascade.",
	})
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
