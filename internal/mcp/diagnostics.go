package mcp

import (
	"context"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/internal/multiplex"
	"github.com/iodesystems/poly-lsp-mcp/internal/symbols"
)

// diagnosticJSON is the wire shape we emit alongside edit responses.
// Positions are 1-based UTF-16 character offsets to match LSP, NOT
// byte offsets — the LLM gets the same coordinates `node_read` would
// produce, with start/end inclusive of the affected span.
//
// The enrichment fields below are what saves an agent a follow-up
// query: Text (slice at the range), Context (surrounding lines),
// EnclosingNode (tree-sitter declaration containing the position —
// pass to node_edit / node_refactor), and References (indexed sites
// for the flagged identifier — same shape as node_references).
type diagnosticJSON struct {
	File      string `json:"file"`
	Severity  string `json:"severity"`
	Code      any    `json:"code,omitempty"`
	Source    string `json:"source,omitempty"`
	Message   string `json:"message"`
	StartLine int    `json:"startLine"`
	StartCol  int    `json:"startCol"`
	EndLine   int    `json:"endLine"`
	EndCol    int    `json:"endCol"`

	Text          string         `json:"text,omitempty"`
	Context       []contextLine  `json:"context,omitempty"`
	EnclosingNode *nodeInfo      `json:"enclosingNode,omitempty"`
	References    []siteJSON     `json:"references,omitempty"`
	Truncated     *truncatedInfo `json:"truncated,omitempty"`
}

// contextLine is one line of source near the diagnostic. Line is
// 1-based; Text has trailing whitespace stripped but otherwise is
// the file content verbatim.
type contextLine struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// nodeInfo is the same shape as a structure-tool node entry — kind
// dropped because every nodeInfo is a tree-sitter "node" (file /
// directory wouldn't appear here). startLine/Col delimit the whole
// declaration; nameStartLine/Col delimits just the identifier inside
// it (when the grammar exposes one).
type nodeInfo struct {
	Type          string `json:"type"`
	Name          string `json:"name,omitempty"`
	StartLine     int    `json:"startLine"`
	StartCol      int    `json:"startCol"`
	EndLine       int    `json:"endLine"`
	EndCol        int    `json:"endCol"`
	NameStartLine int    `json:"nameStartLine,omitempty"`
	NameStartCol  int    `json:"nameStartCol,omitempty"`
	NameEndLine   int    `json:"nameEndLine,omitempty"`
	NameEndCol    int    `json:"nameEndCol,omitempty"`
}

// truncatedInfo carries dropped-count totals when an enrichment cap
// fires. Only populated when something was actually dropped.
type truncatedInfo struct {
	References int `json:"references,omitempty"`
}

// editDiagnostics is the bundle attached to every node_edit /
// node_delete / node_refactor response. When the workspace has no LSP
// for the language (markdown, yaml, plain text), Available is false
// AND Items is empty — the agent must NOT infer "compiles clean" from
// the absence of items.
//
// Diagnostics is total-capped via diagnosticOptions.DiagnosticLimit.
// If the LSP published more items than the cap allows,
// DroppedDiagnostics holds the overflow count.
type editDiagnostics struct {
	Available          bool             `json:"diagnosticsAvailable"`
	TimedOut           bool             `json:"diagnosticsTimedOut,omitempty"`
	Items              []diagnosticJSON `json:"diagnostics"`
	DroppedDiagnostics int              `json:"droppedDiagnostics,omitempty"`
}

// diagnosticOptions are the per-call enrichment caps. Zero values
// fall back to the package defaults (defaultDiagnosticLimit etc.).
// Callers pass these through tool arguments so an agent dealing with
// a tight context window can ask for compact responses, while one
// debugging an aggressive refactor can ask for the full picture.
type diagnosticOptions struct {
	DiagnosticLimit int `json:"diagnosticLimit"`
	ReferenceLimit  int `json:"referenceLimit"`
	ContextLines    int `json:"contextLines"`

	// SiblingDiagnostics controls whether the response includes
	// diagnostics for files OTHER than the one(s) the tool edited.
	// gopls (and most LSPs) publish at the package level — a single
	// file edit can produce new errors on sibling files. Default ON;
	// set false to scope the response to edited URIs only (less
	// noise, but compile cascades stay invisible).
	//
	// Pointer so we can distinguish unset (default) from explicit
	// false: zero value of bool would force the caller to write
	// `siblingDiagnostics: true` for current behavior.
	SiblingDiagnostics *bool `json:"siblingDiagnostics,omitempty"`
}

const (
	defaultDiagnosticLimit = 25
	defaultReferenceLimit  = 15
	defaultContextLines    = 3
	maxRangeTextChars      = 256
)

func (o diagnosticOptions) diagnosticLimit() int {
	if o.DiagnosticLimit > 0 {
		return o.DiagnosticLimit
	}
	return defaultDiagnosticLimit
}

func (o diagnosticOptions) referenceLimit() int {
	if o.ReferenceLimit > 0 {
		return o.ReferenceLimit
	}
	return defaultReferenceLimit
}

func (o diagnosticOptions) contextLines() int {
	if o.ContextLines > 0 {
		return o.ContextLines
	}
	return defaultContextLines
}

func (o diagnosticOptions) siblingDiagnostics() bool {
	if o.SiblingDiagnostics == nil {
		return true
	}
	return *o.SiblingDiagnostics
}

// startManagerIfPresent spawns child LSPs for the languages observed
// in the freshly-built index. No-op when manager is nil (callers
// opted out of LSP integration). Called from handleInitialize so the
// language list comes from a real Build, not a guess.
func (s *Server) startManagerIfPresent(idx *symbols.Index) {
	if s.manager == nil || s.getRoot() == "" {
		return
	}
	langs := idx.Languages()
	if len(langs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.manager.Start(ctx, s.getRoot(), pathToURI(s.getRoot()), langs); err != nil {
		log.Printf("mcp initialize: manager.Start: %v", err)
	}
}

// stopManagerIfPresent drains the manager during shutdown. Best
// effort: errors are logged, never surfaced.
func (s *Server) stopManagerIfPresent() {
	if s.manager == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.manager.Shutdown(ctx); err != nil {
		log.Printf("mcp shutdown: manager.Shutdown: %v", err)
	}
}

// collectDiagnostics is the post-write hook. For every URI in `uris`,
// route to the matching child LSP, send didOpen-or-didChange + didSave
// notifications with the supplied content, and wait up to
// diagWaitDuration() for a publishDiagnostics to arrive.
//
// Each surviving diagnostic is enriched (Text / Context /
// EnclosingNode / References) so the agent can act without re-querying
// the workspace. Per-call caps live on `opts`; zeros fall back to
// package defaults.
//
// Sibling rollup: gopls (and most LSPs) publish at the package level
// — one file edit can produce new diagnostics on sibling files. After
// the direct waits return, we snapshot the store and include any URI
// whose generation advanced past the pre-edit baseline. Disable via
// opts.SiblingDiagnostics=false.
//
// Returns:
//   - available=false when manager is nil OR none of the URIs has a
//     child LSP. Empty Items.
//   - available=true with the per-URI diagnostic snapshot otherwise.
//     TimedOut indicates at least one edited URI hit the deadline
//     without a fresh publish (the snapshot may still be a prior
//     state).
func (s *Server) collectDiagnostics(uris []string, contents map[string][]byte, opts diagnosticOptions) editDiagnostics {
	if s.manager == nil || len(uris) == 0 {
		return editDiagnostics{Available: false, Items: []diagnosticJSON{}}
	}

	store := s.manager.Diagnostics()
	type captured struct {
		uri   string
		child *multiplex.Child
		since uint64
	}

	// Pre-edit snapshot of every URI's gen counter. Anything that
	// advances past this during the wait window was a consequence
	// of THIS edit (modulo other clients editing concurrently —
	// not a concern in single-client MCP).
	preEditGens := storeGenSnapshot(store)

	tracked := make([]captured, 0, len(uris))
	editedURIs := make(map[string]struct{}, len(uris))
	for _, uri := range uris {
		child := s.manager.RouteByURI(uri)
		if child == nil {
			continue
		}
		// Capture gen BEFORE sending notifications so WaitAfter wakes
		// on any publish that arrives as a consequence of this edit.
		since := store.Gen(uri)
		s.notifyChildOfEdit(child, uri, contents[uri])
		tracked = append(tracked, captured{uri: uri, child: child, since: since})
		editedURIs[uri] = struct{}{}
	}

	if len(tracked) == 0 {
		return editDiagnostics{Available: false, Items: []diagnosticJSON{}}
	}

	deadline := s.diagWaitDuration()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	out := editDiagnostics{Available: true, Items: []diagnosticJSON{}}
	limit := opts.diagnosticLimit()

	// Phase 1: wait for each edited URI's gen to advance. Diagnostics
	// for the edited URIs are added first so they show up at the top
	// of the response — the cap-overflow ordering then favors the
	// agent's direct edit over sibling fallout.
	editedDiags := make(map[string][]multiplex.Diagnostic, len(tracked))
	for _, c := range tracked {
		diags := store.WaitAfter(ctx, c.uri, c.since)
		if ctx.Err() != nil {
			out.TimedOut = true
		}
		editedDiags[c.uri] = diags
	}

	// Build the response in two passes — edited URIs first (favored
	// by the cap), then siblings.
	addDiagnostics := func(uri string, diags []multiplex.Diagnostic) {
		for _, d := range diags {
			if len(out.Items) >= limit {
				out.DroppedDiagnostics++
				continue
			}
			out.Items = append(out.Items, s.enrichDiagnostic(uri, d, contents[uri], opts))
		}
	}

	// Sort edited URIs for stable output.
	editedSorted := make([]string, 0, len(tracked))
	for _, c := range tracked {
		editedSorted = append(editedSorted, c.uri)
	}
	sort.Strings(editedSorted)
	for _, uri := range editedSorted {
		addDiagnostics(uri, editedDiags[uri])
	}

	// Phase 2: sibling rollup. Snapshot the store and add any URI
	// whose gen advanced past pre-edit baseline AND wasn't part of
	// the edited set. New URIs (never seen before) count as advanced
	// from 0 → ≥1.
	if opts.siblingDiagnostics() {
		snapshot := store.Snapshot()
		siblingURIs := make([]string, 0, len(snapshot))
		for uri := range snapshot {
			if _, edited := editedURIs[uri]; edited {
				continue
			}
			if store.Gen(uri) > preEditGens[uri] {
				siblingURIs = append(siblingURIs, uri)
			}
		}
		sort.Strings(siblingURIs)
		for _, uri := range siblingURIs {
			addDiagnostics(uri, snapshot[uri])
		}
	}
	return out
}

// storeGenSnapshot captures the current gen for every URI present in
// the store. Used as the pre-edit baseline for sibling rollup.
func storeGenSnapshot(store *multiplex.DiagnosticStore) map[string]uint64 {
	current := store.Snapshot()
	out := make(map[string]uint64, len(current))
	for uri := range current {
		out[uri] = store.Gen(uri)
	}
	return out
}

// enrichDiagnostic builds the wire entry for one diagnostic, pulling
// the post-edit content for the URI (or reading from disk if the
// diagnostic landed on a sibling file we didn't write to) and
// computing Text / Context / EnclosingNode / References. Failures in
// any one enrichment step degrade gracefully — the diagnostic itself
// is always emitted with its LSP coordinates.
func (s *Server) enrichDiagnostic(uri string, d multiplex.Diagnostic, content []byte, opts diagnosticOptions) diagnosticJSON {
	abs := uriToPath(uri)
	rel := abs
	if root := s.getRoot(); root != "" {
		if r, err := filepath.Rel(root, abs); err == nil {
			rel = r
		}
	}

	item := diagnosticJSON{
		File:      rel,
		Severity:  severityLabel(d.Severity),
		Code:      d.Code,
		Source:    d.Source,
		Message:   d.Message,
		StartLine: d.Range.Start.Line + 1,
		StartCol:  d.Range.Start.Character + 1,
		EndLine:   d.Range.End.Line + 1,
		EndCol:    d.Range.End.Character + 1,
	}

	if content == nil {
		if data, err := os.ReadFile(abs); err == nil {
			content = data
		}
	}
	if content == nil {
		return item
	}

	item.Text = sliceRangeText(content, item.StartLine, item.StartCol, item.EndLine, item.EndCol, maxRangeTextChars)
	item.Context = surroundingLines(content, item.StartLine, item.EndLine, opts.contextLines())

	lang := s.languageForFile(abs)
	if lang != "" {
		if node, err := symbols.EnclosingStructureNode(lang, content, item.StartLine, item.StartCol); err == nil && node != nil {
			item.EnclosingNode = &nodeInfo{
				Type:          node.Type,
				Name:          node.Name,
				StartLine:     node.StartLine,
				StartCol:      node.StartCol,
				EndLine:       node.EndLine,
				EndCol:        node.EndCol,
				NameStartLine: node.NameStartLine,
				NameStartCol:  node.NameStartCol,
				NameEndLine:   node.NameEndLine,
				NameEndCol:    node.NameEndCol,
			}
		}
	}

	// References: only when the diagnostic range looks like a single
	// identifier token. Multi-token ranges (statement-level errors)
	// would dump unrelated lexical hits.
	if idx := s.getIndex(); idx != nil {
		if name := identifierFromRange(item.Text); name != "" {
			sites := idx.Lookup(name)
			refs := make([]siteJSON, 0, len(sites))
			refLimit := opts.referenceLimit()
			dropped := 0
			for _, site := range sites {
				if len(refs) >= refLimit {
					dropped++
					continue
				}
				refs = append(refs, siteJSON{
					Name:       name,
					File:       relPath(site.File, s.getRoot()),
					Line:       site.Line,
					Col:        site.Col,
					Language:   site.Language,
					Confidence: confidenceLabel(site.Confidence),
				})
			}
			item.References = refs
			if dropped > 0 {
				if item.Truncated == nil {
					item.Truncated = &truncatedInfo{}
				}
				item.Truncated.References = dropped
			}
		}
	}

	return item
}

// identRe matches a single bare identifier token. Used to decide
// whether a diagnostic range is an identifier (worth a references
// lookup) or a longer span (skip the lookup).
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func identifierFromRange(text string) string {
	if identRe.MatchString(text) {
		return text
	}
	return ""
}

// sliceRangeText extracts the bytes between (startLine, startCol) and
// (endLine, endCol) — 1-based, end-exclusive — and caps at maxChars
// via mid-ellipsis ("first… last"). Returns empty when positions are
// out of range so the agent doesn't get a misleading snippet.
func sliceRangeText(content []byte, startLine, startCol, endLine, endCol, maxChars int) string {
	start, ok := lineColOffset(content, startLine, startCol)
	if !ok {
		return ""
	}
	end, ok := lineColOffset(content, endLine, endCol)
	if !ok || end < start {
		return ""
	}
	if end > len(content) {
		end = len(content)
	}
	raw := string(content[start:end])
	if maxChars <= 0 || len(raw) <= maxChars {
		return raw
	}
	half := (maxChars - 1) / 2
	return raw[:half] + "…" + raw[len(raw)-half:]
}

// surroundingLines returns N lines before startLine and N lines after
// endLine, plus the lines spanned by [startLine, endLine] themselves.
// All 1-based; trailing whitespace stripped to keep the wire compact.
func surroundingLines(content []byte, startLine, endLine, n int) []contextLine {
	if n < 0 {
		n = 0
	}
	lines := splitLines(content)
	first := startLine - n
	if first < 1 {
		first = 1
	}
	last := endLine + n
	if last > len(lines) {
		last = len(lines)
	}
	out := make([]contextLine, 0, last-first+1)
	for i := first; i <= last; i++ {
		out = append(out, contextLine{
			Line: i,
			Text: strings.TrimRight(lines[i-1], " \t"),
		})
	}
	return out
}

// splitLines splits content on \n, dropping trailing \r so we don't
// get \r-suffixed lines on CRLF files.
func splitLines(content []byte) []string {
	raw := strings.Split(string(content), "\n")
	for i, l := range raw {
		raw[i] = strings.TrimSuffix(l, "\r")
	}
	return raw
}

// lineColOffset converts a 1-based (line, col) to a byte offset.
// Mirrors lineColToByteOffset in tools.go but is local to this file
// to avoid the cycle.
func lineColOffset(content []byte, line, col int) (int, bool) {
	if line < 1 || col < 1 {
		return 0, false
	}
	curLine := 1
	off := 0
	for off < len(content) && curLine < line {
		if content[off] == '\n' {
			curLine++
		}
		off++
	}
	if curLine != line {
		return 0, false
	}
	for i := 1; i < col; i++ {
		if off >= len(content) || content[off] == '\n' {
			return off, true
		}
		off++
	}
	return off, true
}

func severityLabel(sev int) string {
	switch sev {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "info"
	case 4:
		return "hint"
	}
	return "info"
}

// notifyChildOfEdit sends didOpen on first edit for the URI, didChange
// + didSave on subsequent edits. Versions monotonically increase per
// LSP spec. Best-effort: log on failure but don't bubble up — a
// failing notification just means we won't get diagnostics, not that
// the edit failed.
func (s *Server) notifyChildOfEdit(child *multiplex.Child, uri string, content []byte) {
	s.openDocsMu.Lock()
	version, opened := s.openDocs[uri]
	if !opened {
		s.openDocs[uri] = 1
		version = 1
	} else {
		version++
		s.openDocs[uri] = version
	}
	s.openDocsMu.Unlock()

	languageID := s.languageIDForURI(uri)

	if !opened {
		if err := child.Notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri":        uri,
				"languageId": languageID,
				"version":    version,
				"text":       string(content),
			},
		}); err != nil {
			log.Printf("mcp didOpen %s: %v", uri, err)
			return
		}
	} else {
		if err := child.Notify("textDocument/didChange", map[string]any{
			"textDocument": map[string]any{
				"uri":     uri,
				"version": version,
			},
			"contentChanges": []map[string]any{
				{"text": string(content)},
			},
		}); err != nil {
			log.Printf("mcp didChange %s: %v", uri, err)
			return
		}
	}
	if err := child.Notify("textDocument/didSave", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"text":         string(content),
	}); err != nil {
		log.Printf("mcp didSave %s: %v", uri, err)
	}
}

// languageIDForURI returns the LSP languageId for a file's extension.
// Defaults to the language registry's Name; "plaintext" if unknown.
func (s *Server) languageIDForURI(uri string) string {
	path := uriToPath(uri)
	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	if lang := s.registry.LookupByExt(ext); lang != nil {
		return lang.Name
	}
	return "plaintext"
}

// pathToURI converts an absolute filesystem path into a file:// URI.
// POSIX-only; matches the LSP-side helper.
func pathToURI(path string) string {
	return "file://" + path
}

// uriToPath converts a file:// URI to a filesystem path. Returns ""
// for non-file URIs so callers can skip them.
func uriToPath(rawURI string) string {
	if !strings.HasPrefix(rawURI, "file://") {
		return ""
	}
	u, err := url.Parse(rawURI)
	if err != nil {
		return ""
	}
	return u.Path
}

