package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/multiplex"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
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

	// Unconfirmed: an edited URI's result came from a publish with no
	// document version, from a child not configured to be trusted for
	// one (config.LSP.TrustUnversionedDiagnostics; sourcekit-lsp, any
	// server not configured so). It may revert an edit but never
	// validates one: such a server can publish a clean result for text it
	// has not fully analysed (sourcekit-lsp before its build settings
	// load), and nothing in the publish says so. With no new errors the
	// edit is reported not validated (validationSkipped: lsp-unconfirmed).
	Unconfirmed bool `json:"-"`
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

	// Validate makes an edit that introduces a NEW error REVERT instead of
	// landing (revert-on-new-diagnostics). Set server-wide by --validate, or
	// per-call. No-op without a child LSP — reported (validated:false), never
	// a silent pass. See validate.go.
	Validate bool `json:"validate,omitempty"`

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

	// commitBatch is set INTERNALLY by the node_edit handler (not JSON): the
	// edit that closes an open commit:false batch — stage it, then validate
	// and commit the union. See editBatch.
	commitBatch bool
}

const (
	defaultDiagnosticLimit = 25
	defaultReferenceLimit  = 15
	defaultContextLines    = 3
	maxRangeTextChars      = 256

	// maxDiagnosticMessageChars bounds a single diagnostic message so a
	// few verbose ones can't dominate the payload (clampMessage).
	maxDiagnosticMessageChars = 400

	// maxDiagnosticsBytes is the total serialized-JSON budget for the
	// editDiagnostics payload, enforced by capBytes so a response can
	// never exceed the MCP tool-result token cap. Generous for the
	// common case; only pathological floods (e.g. editing a module
	// outside the workspace root) hit it.
	maxDiagnosticsBytes = 16384
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
	s.manager.StatusIndex = func() any { return s.IndexCost() }
	if s.lspRootsAuto {
		s.manager.Roots = s.lspRoots(langs)
		s.manager.RootsMax = s.lspRootsLimit()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.manager.Start(ctx, s.getRoot(), pathToURI(s.getRoot()), langs); err != nil {
		log.Printf("mcp initialize: manager.Start: %v", err)
		return
	}
	if s.proactiveOpen {
		s.kickProactiveOpen()
	}
}

// kickProactiveOpen launches an async walk of the workspace that
// sends textDocument/didOpen to the matching child LSP for every
// file with a registered language. This is what makes the
// poly-lsp-mcp://diagnostics resource useful before any edits —
// gopls (and most LSPs) only publish diagnostics after didOpen /
// didSave on a URI.
//
// Runs in a goroutine so initialize stays fast. A fresh
// proactiveOpenDone channel is installed before the goroutine
// starts, then closed when the walk finishes. Tests use
// WaitForProactiveOpen to observe completion.
func (s *Server) kickProactiveOpen() {
	done := make(chan struct{})
	s.proactiveOpenDoneMu.Lock()
	s.proactiveOpenDone = done
	s.proactiveOpenDoneMu.Unlock()

	go func() {
		defer close(done)
		s.runProactiveOpen()
	}()
}

// runProactiveOpen walks the workspace root and sends didOpen for
// every file whose extension routes to a running child LSP. Skip
// rules mirror symbols.Build: skipDirs for noise dirs, file-size cap
// matching the index's own limit. Best effort throughout — failures
// are logged but don't fail the walk.
func (s *Server) runProactiveOpen() {
	root := s.getRoot()
	if root == "" {
		return
	}
	ig := s.getIgnores()
	shadow := s.registry.NewShadowChecker()
	var opened int
	start := time.Now()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// Gitignored paths are skipped: a vendored toolchain or build tree
		// can hold thousands of files, and every didOpen'd file is one
		// gopls keeps fully type-checked in memory. (dun's ignored go/
		// tree — 9,355 files — drove one gopls to 20 GB.) Edits and
		// lookups still open an ignored file on demand.
		if d.IsDir() {
			if skipProactiveOpenDir(d.Name()) || ig.DirIgnored(root, path) {
				return fs.SkipDir
			}
			return nil
		}
		if ig.KnownIgnored(root, path) {
			return nil
		}
		// foo.js beside foo.ts: the child LSP already has the source.
		if _, ok := shadow.Shadowed(path); ok {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > proactiveMaxFileSize {
			return nil
		}
		uri := pathToURI(path)
		var child *multiplex.Child
		if s.lspRootsAuto {
			// RouteByURI would start every project root's child, and past
			// --lsp-roots-max stop them again: they start on first use.
			child = s.manager.Running(uri)
		} else {
			child = s.manager.RouteByURI(uri)
		}
		if child == nil {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		s.notifyChildOfOpen(child, uri, content)
		opened++
		return nil
	})
	if err != nil {
		log.Printf("mcp proactive open: walk %s: %v", root, err)
	}
	log.Printf("mcp proactive open: didOpen %d file(s) in %s", opened, time.Since(start))
}

const proactiveMaxFileSize = 1 << 20 // 1 MiB, matches symbols.Build

// skipProactiveOpenDir mirrors the structure/index skip list so we
// don't pour vendor/node_modules/.git into the child LSPs (which
// would re-parse them anyway, wasting startup time).
func skipProactiveOpenDir(name string) bool {
	return symbols.IsSkipDir(name) || name == ".poly-lsp-mcp"
}

// notifyChildOfOpen is a leaner sibling of notifyChildOfEdit: just
// didOpen, no didSave. Idempotent — if the URI is already opened in
// this session, returns immediately. Used by the proactive walk so
// follow-up edits still know whether to emit didOpen or didChange.
func (s *Server) notifyChildOfOpen(child *multiplex.Child, uri string, content []byte) {
	s.openDocsMu.Lock()
	if _, opened := s.openDocs[uri]; opened {
		s.openDocsMu.Unlock()
		return
	}
	s.openDocs[uri] = 1
	s.openGen[uri] = s.storeGen(uri)
	s.openDocsMu.Unlock()
	s.recordSent(uri, content)

	languageID := s.languageIDForURI(uri)
	if err := child.Notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": languageID,
			"version":    1,
			"text":       string(content),
		},
	}); err != nil {
		log.Printf("mcp proactive didOpen %s: %v", uri, err)
	}
}

// replayOpenDocs is the manager's OnStart hook. A restarted child (crash
// restart, or lazy start after a lifecycle stop) knows no documents, but
// openDocs says they are open — the next edit would send didChange for a
// URI the child never saw. Re-send didOpen for every open document of
// lang, from disk (edits land on disk before they are pushed). Documents
// gone from disk are forgotten.
func (s *Server) replayOpenDocs(lang string, child *multiplex.Child) {
	start := time.Now()
	s.openDocsMu.Lock()
	uris := make([]string, 0, len(s.openDocs))
	for uri := range s.openDocs {
		if s.languageIDForURI(uri) == lang {
			uris = append(uris, uri)
		}
	}
	s.openDocsMu.Unlock()
	// Only child's own documents: under --lsp-roots auto a language has a
	// child per project root, and a document from another root would make
	// this one report errors for a module it does not hold. The manager
	// has registered child before calling OnStart, so Running finds it.
	own := uris[:0]
	for _, uri := range uris {
		if s.manager == nil || s.manager.Running(uri) == child {
			own = append(own, uri)
		}
	}
	uris = own

	replayed := 0
	for _, uri := range uris {
		content, err := os.ReadFile(uriToPath(uri))
		s.openDocsMu.Lock()
		if err != nil {
			delete(s.openDocs, uri)
			delete(s.sentDocs, uri)
			delete(s.openGen, uri)
			s.openDocsMu.Unlock()
			continue
		}
		s.openDocs[uri] = 1
		s.openGen[uri] = s.storeGen(uri)
		s.openDocsMu.Unlock()
		s.recordSent(uri, content)
		if err := child.Notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri":        uri,
				"languageId": lang,
				"version":    1,
				"text":       string(content),
			},
		}); err != nil {
			log.Printf("mcp replay didOpen %s: %v", uri, err)
			return
		}
		replayed++
	}
	log.Printf("mcp: %s started; replayed didOpen for %d file(s) in %s", lang, replayed, time.Since(start).Round(time.Millisecond))
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
//     TimedOut indicates the deadline was hit before the picture was
//     complete — either an edited URI never got a fresh publish, or the
//     sibling rollup ran out of time waiting for the publish stream to
//     go quiet. Either way the snapshot may be a partial or prior
//     state, and the flag says so rather than passing it off as final.
//
// waitForDiagnosticsToSettle blocks until the child LSPs stop
// publishing for one `quiet` interval, or ctx expires. Returns false
// when it gave up on the deadline rather than on quiet.
//
// A debounce is the honest primitive here: LSP has no "analysis
// finished" signal a client can wait on, so the only portable evidence
// that the fallout is complete is the server going silent.
func waitForDiagnosticsToSettle(ctx context.Context, store *multiplex.DiagnosticStore, quiet time.Duration) bool {
	if quiet <= 0 {
		return true
	}
	for {
		if ctx.Err() != nil {
			return false
		}
		since := store.TotalGen()
		if _, gotMore := store.WaitAnyAfter(ctx, since, quiet); !gotMore {
			// Nothing new for a full quiet window: the burst is over.
			return ctx.Err() == nil
		}
	}
}

// waitForEditedText waits for uri's diagnostics of the text just sent as
// document version `version`, and reports false when ctx ends first. A
// publish arriving after the edit is not enough: it can describe the
// text before it.
//
//   - A publish carrying a document version (gopls, clangd, pylsp) is the
//     edited text's when that version is at least `version`; an older
//     one is skipped. Only such a result can validate an edit.
//   - A publish without one (sourcekit-lsp, typescript-language-server)
//     is skipped when it is the first since the URI's didOpen. Measured
//     with sourcekit-lsp 6.4.1: a just-opened file is published about
//     1 s later with no semantic errors at all (build settings not
//     loaded yet; an undeclared call reads clean), then again 1-3 s
//     later with them. A later publish is taken once no other arrives
//     for the settle window (diagSettleDuration), flagged unconfirmed
//     unless the child is configured to trust it
//     (config.LSP.TrustUnversionedDiagnostics; off for every default
//     server): its errors can revert the edit, but
//     a clean one does not validate it. sourcekit-lsp computes only syntactic diagnostics while it has
//     no build settings for a file, and its publish does not say so
//     (DiagnosticReportManager, swiftlang/sourcekit-lsp); on
//     macos-15-intel it published [] for a broken edit 2 s after its
//     package had loaded (CI trace, 2026-10-27).
//   - An unversioned publish that arrives while the child has a
//     work-done progress open (Child.Busy) and has no errors is skipped,
//     to give the child the chance to report errors: sourcekit-lsp
//     publishes a file without its semantic errors for as long as
//     "SourceKit-LSP: Reloading Package" is open, and does so again for
//     each change in that time (measured, 6.4.1 under a CPU quota).
//     After a skipped one, if the child's progress ends before it
//     publishes again, the text is sent again (didChange, same text) so
//     it publishes from its loaded state: it may not republish on its own
//     (sourcekit-lsp ends "Indexing" right after publishing a change).
//
// Pull diagnostics (textDocument/diagnostic) do not help. sourcekit-lsp
// 6.4.1 registers it (dynamically) when the client advertises it, then
// stops publishing, and answers a pull made before its build settings
// load at once with a full, empty report for a broken file; a pull
// after its workspace/diagnostic/refresh has the error (measured,
// 2026-10-27). The answer is no more conclusive than a publish.
// gopls 0.24 answers pulls but versions its publishes;
// typescript-language-server 5.1.3, clangd and pylsp 1.15 reject them.
//
// unconfirmed reports a result from an unversioned publish of a child
// not trusted for one.
func (s *Server) waitForEditedText(ctx context.Context, store *multiplex.DiagnosticStore, child *multiplex.Child, uri string, content []byte, since uint64, version int32) (diags []multiplex.Diagnostic, ok, unconfirmed bool) {
	s.openDocsMu.Lock()
	openGen, opened := s.openGen[uri]
	s.openDocsMu.Unlock()
	after := since
	resends := 0
	for {
		store.WaitAfter(ctx, uri, after)
		if ctx.Err() != nil {
			return nil, false, false
		}
		p := store.Latest(uri)
		switch {
		case p.Versioned && p.Version >= int(version):
			return p.Diags, true, false
		case p.Versioned:
			after = p.Gen // for an earlier version of the text
			continue
		case opened && p.Gen == openGen+1:
			after = p.Gen // the first publish since didOpen
			continue
		}
		// Unversioned: take the last of a burst.
		quiet := s.diagSettleDuration()
		for quiet > 0 {
			qctx, cancel := context.WithTimeout(ctx, quiet)
			store.WaitAfter(qctx, uri, p.Gen)
			cancel()
			next := store.Latest(uri)
			if next.Gen == p.Gen {
				break
			}
			p = next
		}
		if p.Busy && !anyError(p.Diags) {
			// Clean while loading: wait for a later one.
			after = p.Gen
			if waitPublishOrIdle(ctx, store, child, uri, after) && resends < maxResends {
				resends++
				s.resendText(child, uri, content)
			}
			continue
		}
		return p.Diags, true, !child.TrustsUnversionedDiagnostics()
	}
}

// maxResends caps the didChange resends of one edit's wait.
const maxResends = 3

// waitPublishOrIdle waits for a publish for uri past gen `after`, or for
// child to have no work-done progress open, or ctx. It reports true when
// the child went idle with no new publish.
func waitPublishOrIdle(ctx context.Context, store *multiplex.DiagnosticStore, child *multiplex.Child, uri string, after uint64) bool {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := child.Idle()
	go func() {
		select {
		case <-idle:
			cancel()
		case <-wctx.Done():
		}
	}()
	store.WaitAfter(wctx, uri, after)
	return store.Gen(uri) == after && ctx.Err() == nil
}

// resendText sends uri's text to child again as a didChange at the next
// version, so the child publishes it again.
func (s *Server) resendText(child *multiplex.Child, uri string, content []byte) {
	s.openDocsMu.Lock()
	version := s.openDocs[uri] + 1
	s.openDocs[uri] = version
	s.openDocsMu.Unlock()
	if err := child.Notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": version},
		"contentChanges": []map[string]any{{"text": string(content)}},
	}); err != nil {
		log.Printf("mcp didChange %s: %v", uri, err)
	}
}

func anyError(diags []multiplex.Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == 1 {
			return true
		}
	}
	return false
}

func (s *Server) collectDiagnostics(uris []string, contents map[string][]byte, opts diagnosticOptions) editDiagnostics {
	if s.manager == nil || len(uris) == 0 {
		return editDiagnostics{Available: false, Items: []diagnosticJSON{}}
	}

	store := s.manager.Diagnostics()
	type captured struct {
		uri     string
		child   *multiplex.Child
		since   uint64
		version int32
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
		version := s.notifyChildOfEdit(child, uri, contents[uri])
		tracked = append(tracked, captured{uri: uri, child: child, since: since, version: version})
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

	// Phase 1: wait for each edited URI's diagnostics OF THE EDITED
	// TEXT (waitForEditedText). Diagnostics for the edited URIs are added
	// first so they show up at the top of the response — the cap-overflow
	// ordering then favors the agent's direct edit over sibling fallout.
	// A URI that got none within the deadline reports none: what the
	// store holds for it describes other text.
	editedDiags := make(map[string][]multiplex.Diagnostic, len(tracked))
	for _, c := range tracked {
		diags, ok, unconfirmed := s.waitForEditedText(ctx, store, c.child, c.uri, contents[c.uri], c.since, c.version)
		if !ok {
			out.TimedOut = true
		}
		if unconfirmed {
			out.Unconfirmed = true
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
		// SETTLE FIRST. Phase 1 returns the instant the EDITED file is
		// published, but the sibling that breaks because of that edit
		// arrives in a SEPARATE publish — gopls re-analyses the package
		// and emits one message per file. Sampling here without waiting
		// is a race that silently reports "no fallout" whenever the
		// sibling's message is a few milliseconds behind, which is
		// exactly the failure this feature exists to prevent.
		//
		// Measured before this: TestSiblingDiagnosticsRollup failed
		// roughly 1 run in 3, always by returning [] in ~1.2s while its
		// 12s budget went unused — the wait was never the problem, the
		// missing wait was.
		if !waitForDiagnosticsToSettle(ctx, store, s.diagSettleDuration()) {
			out.TimedOut = true
		}
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
	return out.capBytes(maxDiagnosticsBytes)
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
	abs := s.rootSpelling(uriToPath(uri))
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
		Message:   clampMessage(d.Message),
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
		// No content to measure against, so the columns stay as the
		// server reported them — UTF-16, and flagged as such rather
		// than silently mixed with byte columns elsewhere.
		return item
	}

	// The server counts `character` in UTF-16 code units; every column
	// this tool reports, and every consumer below (sliceRangeText,
	// enclosing-node lookup), works in 1-based BYTES. Convert once here
	// so the two conventions never meet.
	if c, ok := utf16ColToByteColumn(content, item.StartLine, item.StartCol); ok {
		item.StartCol = c
	}
	if c, ok := utf16ColToByteColumn(content, item.EndLine, item.EndCol); ok {
		item.EndCol = c
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
			sites := idx.LookupExisting(name)
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
// the edit failed. Returns the document version sent.
func (s *Server) notifyChildOfEdit(child *multiplex.Child, uri string, content []byte) int32 {
	s.openDocsMu.Lock()
	version, opened := s.openDocs[uri]
	if !opened {
		s.openDocs[uri] = 1
		s.openGen[uri] = s.storeGen(uri)
		version = 1
	} else {
		version++
		s.openDocs[uri] = version
	}
	s.openDocsMu.Unlock()
	s.recordSent(uri, content)

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
			return version
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
			return version
		}
	}
	if err := child.Notify("textDocument/didSave", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"text":         string(content),
	}); err != nil {
		log.Printf("mcp didSave %s: %v", uri, err)
	}
	return version
}

// storeGen is the diagnostic store's generation for uri, 0 without a
// manager.
func (s *Server) storeGen(uri string) uint64 {
	if s.manager == nil {
		return 0
	}
	return s.manager.Diagnostics().Gen(uri)
}

// notifyChildOfExternalChange tells the matching child LSP that a file
// changed on disk OUTSIDE the tool's own edits — a `git checkout` or
// `rebase`, a formatter, a second agent, the user's editor. Called from
// the file watcher.
//
// This is not optional bookkeeping, it is the difference between
// truthful diagnostics and confident lies. A proactively-didOpen'd file
// is an OVERLAY as far as the server is concerned: per LSP the client's
// copy is authoritative for an open document, and the server will never
// consult the disk for it again. Nothing here reopens that gap on its
// own — no didChangeWatchedFiles is registered, and a server like gopls
// does not watch the filesystem itself. So a rebase that rewrites a
// file leaves the child type-checking against a version that no longer
// exists anywhere, and it says so with total confidence: the dogfood
// case reported `too many arguments … want 1` against a signature that
// had taken three arguments on disk for twenty minutes, while `go
// build` passed. Re-pushing the bytes is what closes it.
//
// Sending didChange (rather than didChangeWatchedFiles) is deliberate:
// it works whether or not the URI is already open, needs no capability
// negotiation, and reuses the exact path an edit takes.
//
// The hash guard matters as much as the notification. fsnotify fires on
// the tool's OWN writes too, ~200ms behind each node_edit, and echoing
// those back would spend a round-trip per edit and dribble extra
// publishes into the next call's settle window. Content the child
// already has is dropped here.
func (s *Server) notifyChildOfExternalChange(path string, content []byte) {
	if s.manager == nil {
		return
	}
	uri := pathToURI(path)
	// Running, not RouteByURI: a stopped child needs no push — it
	// re-reads open documents from disk when it starts (replayOpenDocs).
	child := s.manager.Running(uri)
	if child == nil {
		return
	}
	if s.sentContentMatches(uri, content) {
		return // the child already has exactly these bytes
	}
	s.notifyChildOfEdit(child, uri, content)
}

// recordSent remembers the text just pushed to the child for uri. Caller
// must NOT hold openDocsMu.
func (s *Server) recordSent(uri string, content []byte) {
	sum := sha256.Sum256(content)
	s.openDocsMu.Lock()
	s.sentDocs[uri] = sum
	s.openDocsMu.Unlock()
}

// sentContentMatches reports whether the child already holds exactly these
// bytes for uri — false when the URI was never pushed at all.
func (s *Server) sentContentMatches(uri string, content []byte) bool {
	sum := sha256.Sum256(content)
	s.openDocsMu.Lock()
	defer s.openDocsMu.Unlock()
	known, seen := s.sentDocs[uri]
	return seen && known == sum
}

// notifyChildOfExternalDelete closes a URI the watcher saw disappear, so
// the child stops reporting against a file that no longer exists and a
// later re-create is announced as a fresh didOpen rather than a
// didChange on a version the child never saw.
func (s *Server) notifyChildOfExternalDelete(path string) {
	if s.manager == nil {
		return
	}
	uri := pathToURI(path)
	child := s.manager.Running(uri)
	if child == nil {
		return // replayOpenDocs drops it when the child starts
	}
	s.openDocsMu.Lock()
	_, opened := s.openDocs[uri]
	delete(s.openDocs, uri)
	delete(s.sentDocs, uri)
	delete(s.openGen, uri)
	s.openDocsMu.Unlock()
	if !opened {
		return
	}
	if err := child.Notify("textDocument/didClose", map[string]any{
		"textDocument": map[string]any{"uri": uri},
	}); err != nil {
		log.Printf("mcp didClose %s: %v", uri, err)
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

// clampMessage truncates an over-long diagnostic message so a handful
// of verbose ones (gopls' multi-line "not in your workspace" hint, for
// instance) can't blow the tool-result token budget. Rune-safe; marks
// the cut with an ellipsis.
func clampMessage(s string) string {
	r := []rune(s)
	if len(r) <= maxDiagnosticMessageChars {
		return s
	}
	return string(r[:maxDiagnosticMessageChars]) + "…"
}

// jsonLen returns the marshaled byte length of v, or 0 if it can't be
// marshaled (treated as "fits" — never the cause of a drop).
func jsonLen(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// capBytes enforces a total serialized-size budget on the payload so a
// response can never exceed the MCP tool-result token cap. Pass 1 strips
// enrichment (text / context / enclosing node / references) from the
// tail until the JSON fits; pass 2, only if still over, drops whole
// items from the tail and counts them in DroppedDiagnostics. Core fields
// (file, severity, message, position) always survive — the agent still
// learns what broke and where.
func (ed editDiagnostics) capBytes(budget int) editDiagnostics {
	for i := len(ed.Items) - 1; i >= 0 && jsonLen(ed) > budget; i-- {
		ed.Items[i].Text = ""
		ed.Items[i].Context = nil
		ed.Items[i].EnclosingNode = nil
		ed.Items[i].References = nil
		ed.Items[i].Truncated = nil
	}
	for len(ed.Items) > 0 && jsonLen(ed) > budget {
		ed.Items = ed.Items[:len(ed.Items)-1]
		ed.DroppedDiagnostics++
	}
	return ed
}
