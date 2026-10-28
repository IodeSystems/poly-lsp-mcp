package mcp

import (
	"crypto/sha256"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// Edit validation: revert-on-new-diagnostics. When opts.Validate is set (the
// `--validate` server flag, or a per-call validate:true), an edit that
// introduces a NEW error is rolled back instead of landing — turning node_edit
// from "reports breakage" into "can't leave the file broken". No-op without a
// child LSP: it is REPORTED (validated:false, validationSkipped), never a
// silent pass.
//
// Scope: the WRITE paths (range / whole-file / diff) and the multi-file refactor
// paths (rename / signature) via validationTxn. Validation is WORKSPACE-WIDE —
// the baseline+diff is over every file's errors, so an edit that breaks an
// importer (cross-file) is caught, not just same-file breakage. A settle-wait
// (settleErrorFingerprint) lets gopls's package-level republish land before the
// post-edit diff. Delete-introduced breakage in unopened importers is the
// remaining gap (the file must be analyzed for its error to appear).

// editOutcome is the result of a validated write. On success attach() the
// diagnostics to your payload; on Rejected return rejection() — disk was
// rolled back.
type editOutcome struct {
	Diags        editDiagnostics
	Rejected     bool     // Validate on and the edit introduced new errors → reverted
	Reverted     bool     // prior bytes restored
	RevertFailed bool     // rejected AND revert failed — disk may be inconsistent
	RevertErr    string   // revert failure detail
	NewErrors    []string // the error diagnostics THIS edit added (code∷message)
	Validated    bool     // proven clean: LSP answered, no new errors
	Skipped      string   // "" | "no-lsp" | "lsp-timeout" | "lsp-unconfirmed" — safety not provable
	SkipNote     string   // why, in words, when Skipped is lsp-timeout or lsp-unconfirmed

	// LostWrites names variables this edit stopped ever ASSIGNING while
	// leaving them referenced — the one breakage class no diagnostic sees.
	// See lostWrites.
	LostWrites []string

	// Batch (commit:false transaction) signals — set only when an edit
	// runs inside an open batch (see editBatch).
	Staged         bool // applied to the open batch, NOT validated, NOT committed
	BatchCommitted bool // this edit closed the batch (validate the union then persist/revert)
	Pending        int  // edits in the batch (staged count, or the number just committed)

	// Conflict signals a commit ABORTED because a staged file changed on
	// disk underneath the batch (another session, the user's editor, a
	// formatter, git checkout) — neither its pre-batch nor its staged bytes.
	// The batch is left OPEN and untouched: never silently overwrite the
	// outside change, never silently revert it away. Conflicts names them.
	Conflict  bool
	Conflicts []string
}

// editBatch is the commit:false transaction: staged edits accumulate on
// disk (each written normally so later edits and resolves see them), and
// the WHOLE batch validates as one unit at commit — the union's error
// fingerprint vs the baseline captured when the batch opened. Any new
// error reverts EVERY touched file to its pre-batch bytes; rollback:true
// discards the same way. This is how a multi-step refactor that passes
// through a broken intermediate (change a signature AND its body) commits
// atomically without the per-edit --validate reverting the first half.
//
// On-disk (not an in-memory buffer): the staged intermediate is briefly on
// disk between calls. That is deliberate — every read/resolve/parse then
// sees the staged state for free, node_edit fires no PostToolUse hooks, and
// the file-watch WANTS to see it; the intermediate is reverted on rollback
// or a failed commit.
type editBatch struct {
	s         *Server
	sess      sessionID // owning session (for per-file claims)
	validate  bool
	baseline  map[string]int             // workspace error fingerprint at open (with a child LSP)
	originals map[string][]byte          // uri → pre-batch bytes (first write wins) — for revert
	staged    map[string][32]byte        // uri → hash of the LAST bytes we staged — for commit-time conflict detection
	diags     map[string]editDiagnostics // uri → the child's result for the LAST bytes we staged (if validating)
	count     int
}

func (s *Server) openBatch(opts diagnosticOptions) *editBatch {
	b := &editBatch{
		s:         s,
		sess:      s.activeSession,
		validate:  (opts.Validate || s.validateEdits) && s.manager != nil,
		originals: map[string][]byte{},
		staged:    map[string][32]byte{},
		diags:     map[string]editDiagnostics{},
	}
	if s.manager != nil {
		// Taken without validation too: a rollback whose revert gets no
		// diagnostics in time hands it on (awaitRevert).
		b.baseline = s.editBaseline()
	}
	return b
}

// stage writes one edit into the batch: record the file's pre-batch bytes
// (first write wins), write to disk, refresh the index, and feed the child
// LSP — but do NOT validate. Later edits and resolves see it on disk.
func (b *editBatch) stage(abs string, orig, out []byte, mode os.FileMode, opts diagnosticOptions) error {
	uri := pathToURI(abs)
	// Claim the file for this session before touching disk. A file already
	// staged by ANOTHER session is refused, so a later revert can't restore
	// this session's original over the other's staged edit (silent loss).
	if holder, ok := b.s.claimFile(uri, b.sess); !ok {
		return fmt.Errorf("%s is staged by another client session (%q) — commit or roll back that batch, or edit a different file", relPath(abs, b.s.getRoot()), string(holder))
	}
	if _, seen := b.originals[uri]; !seen {
		b.originals[uri] = orig
	}
	if err := b.s.atomicWrite(abs, out, mode); err != nil {
		return err
	}
	b.staged[uri] = sha256.Sum256(out)
	b.s.refreshFileInIndex(abs, out)
	// Feed the child LSP the staged content (so the commit's union
	// validation sees it) only when we're actually validating — no manager
	// means nothing to feed.
	if b.validate {
		b.diags[uri] = b.s.collectDiagnostics([]string{uri}, map[string][]byte{uri: out}, opts)
	}
	return nil
}

// commit validates the whole batch: if the union introduced no new error
// the staged bytes stand; otherwise EVERY touched file reverts to its
// pre-batch bytes (all-or-nothing). Validation off / no LSP → persist as-is
// (fail-open, flagged via Skipped).
func (b *editBatch) commit() editOutcome {
	oc := editOutcome{BatchCommitted: true, Pending: b.count}
	// Before anything, check nobody wrote a staged file underneath us. If the
	// on-disk bytes are neither our staged bytes nor the pre-batch original,
	// an outside writer (another session, the editor, a formatter, git) got
	// there — abort WITHOUT validating or reverting, leaving the batch open so
	// the outside change is neither overwritten nor silently discarded.
	if conflicts := b.externalWrites(); len(conflicts) > 0 {
		oc.Conflict, oc.Conflicts = true, conflicts
		return oc
	}
	if !b.validate {
		return oc // nothing to prove; staged bytes stand
	}
	edited := map[string]editedResult{}
	for _, d := range b.diags {
		for uri, r := range d.edited {
			edited[uri] = r
		}
	}
	introduced := fingerprintMinus(b.s.settleErrorFingerprint(3*time.Second, edited), b.baseline)
	if len(introduced) == 0 {
		oc.Skipped, oc.SkipNote = b.unproven()
		oc.Validated = oc.Skipped == ""
		return oc
	}
	oc.Rejected, oc.NewErrors = true, introduced
	b.revertAll(&oc)
	return oc
}

// unproven names why the batch's union cannot be validated: a staged
// file whose last result had no child LSP, timed out, or was unconfirmed
// (see skipped, unconfirmed). Empty when every result can validate.
func (b *editBatch) unproven() (reason, note string) {
	uris := make([]string, 0, len(b.diags))
	for uri := range b.diags {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	for _, uri := range uris {
		d := b.diags[uri]
		switch {
		case !d.Available || d.TimedOut:
			return b.s.skipped(d)
		case d.Unconfirmed:
			return unconfirmed()
		}
	}
	return "", ""
}

// externalWrites returns the batch's files whose current on-disk bytes are
// neither what we staged nor the pre-batch original — evidence something
// outside this batch wrote them since we staged. A missing/unreadable staged
// file also counts (it was removed underneath us). Sorted, workspace-relative.
func (b *editBatch) externalWrites() []string {
	var out []string
	for uri, wantHash := range b.staged {
		abs := uriToPath(uri)
		cur, err := os.ReadFile(abs)
		if err != nil {
			out = append(out, relPath(abs, b.s.getRoot()))
			continue
		}
		h := sha256.Sum256(cur)
		if h == wantHash {
			continue // still our staged bytes
		}
		if orig, ok := b.originals[uri]; ok && sha256.Sum256(orig) == h {
			continue // reverted to the pre-batch original — not a foreign write
		}
		out = append(out, relPath(abs, b.s.getRoot()))
	}
	sort.Strings(out)
	return out
}

// revertAll restores every touched file to its pre-batch bytes, and
// waits for the child's diagnostics of them (awaitRevert).
func (b *editBatch) revertAll(oc *editOutcome) {
	b.s.revertFiles(b.originals, b.baseline, oc)
}

// revertFiles writes each uri's original bytes back, tells the child LSP,
// and waits for its diagnostics of them (awaitRevert, base the error
// fingerprint before the reverted edit).
func (s *Server) revertFiles(originals map[string][]byte, base map[string]int, oc *editOutcome) {
	sent := make(map[string][]byte, len(originals))
	for uri, orig := range originals {
		abs := uriToPath(uri)
		mode := os.FileMode(0o644)
		if info, err := os.Stat(abs); err == nil {
			mode = info.Mode().Perm()
		}
		if err := s.atomicWrite(abs, orig, mode); err != nil {
			oc.RevertFailed, oc.RevertErr = true, err.Error()
			continue
		}
		s.refreshFileInIndex(abs, orig)
		sent[uri] = orig
	}
	oc.Reverted = !oc.RevertFailed
	if oc.RevertFailed {
		s.markBaselineUnknown(base) // the child holds text that is not on disk
	}
	s.awaitRevert(sent, base)
}

// awaitRevert sends the reverted contents to the child LSP and waits for
// its diagnostics of them by an edit's rules (collectDiagnostics: the
// reverted text's version or later; unversioned, waitForEditedText's
// rules; then the sibling settle), capped by --validate-wait. The next
// edit's error baseline is read from the store: without the wait it held
// the reverted error, and the same broken edit sent again at once was
// kept, validated (gopls 0.24: 5 of 5 retries, 2026-10-27).
//
// When the wait ends without them, the store may still hold the reverted
// error: markBaselineUnknown(base), base the fingerprint the reverted
// edit was judged against.
func (s *Server) awaitRevert(contents map[string][]byte, base map[string]int) {
	if s.manager == nil || len(contents) == 0 {
		return
	}
	uris := make([]string, 0, len(contents))
	for uri := range contents {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	if d := s.collectDiagnostics(uris, contents, diagnosticOptions{}); d.TimedOut {
		s.markBaselineUnknown(base)
	}
}

// markBaselineUnknown records that the store may hold errors of text no
// longer on disk (a revert got no diagnostics of the reverted text in
// time), base the error fingerprint taken before the reverted edit. The
// next edit's baseline is then the minimum of the store's and base's
// count per error (editBaseline): an error counts as pre-existing only if
// it was there before the reverted edit too. An error the reverted edit
// added is new again, so the same broken edit is not validated. The cost
// is a false revert: an error present before the reverted edit, fixed
// outside poly-lsp-mcp (an editor, git) before the next edit, counts as
// new if that edit brings it back. A false revert is reported and can be
// retried; a false validated:true is the failure validation exists to
// prevent.
// A nil base (no baseline was taken: no child LSP) does nothing.
func (s *Server) markBaselineUnknown(base map[string]int) {
	if base == nil {
		return
	}
	s.staleMu.Lock()
	defer s.staleMu.Unlock()
	if s.staleBaseline != nil {
		base = fingerprintMin(base, s.staleBaseline)
	}
	s.staleBaseline = base
}

// editBaseline is the error fingerprint an edit (or a batch, or a
// refactor) is judged against: errorFingerprintAll, and after a revert
// whose diagnostics did not arrive (markBaselineUnknown), its minimum
// with the fingerprint before the reverted edit. It takes that state:
// the edit's own revert, if any, records it again.
func (s *Server) editBaseline() map[string]int {
	cur := s.errorFingerprintAll()
	s.staleMu.Lock()
	stale := s.staleBaseline
	s.staleBaseline = nil
	s.staleMu.Unlock()
	if stale == nil {
		return cur
	}
	return fingerprintMin(cur, stale)
}

// fingerprintMin is the multiset intersection of a and b.
func fingerprintMin(a, b map[string]int) map[string]int {
	out := make(map[string]int, len(a))
	for k, n := range a {
		if m := b[k]; m < n {
			n = m
		}
		if n > 0 {
			out[k] = n
		}
	}
	return out
}

// applyBytes writes `out` to abs (atomic temp+rename), refreshes the index, and
// collects diagnostics. Under opts.Validate it fingerprints the pre-edit error
// set first and, if the edit introduces a NEW error, restores `orig` and
// reports a rejection. orig==nil means a CREATE (revert = delete the new file).
// lostWrites names the variables this edit stopped ever ASSIGNING, while
// leaving them still referenced.
//
// This is the one failure class no diagnostic can see. Measured on the volcycle
// bench: four independent agent runs deleted
//
//	mVirtualControlKeyDown = down;
//
// leaving the field declared and still read by readControlKey(), so Ctrl became
// permanently false on a phone with no hardware keyboard. javac is happy — the
// field IS still read — and the bench's textual checks scored it 5/6 while it
// shipped. Two prompt framings and the presence or absence of the reference
// graph changed nothing; only noticing at EDIT time does.
//
// Scope, deliberately narrow so it can be trusted:
//   - one file, comparing before against after. A name written from another
//     file is not consulted, so this can warn about an edit that is fine.
//   - syntactic assignment only, no aliasing or pointer writes.
//   - it REPORTS. Removing the last write is legitimate (deleting dead state,
//     replacing a field with a constant), and a refusal would be wrong far
//     more often than a note is annoying.
//
// Still referenced is what makes it worth saying: dropping the write AND every
// read is just deleting something, and says nothing.
func (s *Server) lostWrites(abs string, orig, out []byte) []string {
	lang := s.languageForFile(abs)
	if lang == "" {
		return nil
	}
	before, _ := symbols.NameUses(lang, orig)
	if len(before) == 0 {
		return nil
	}
	afterWrites, afterReads := symbols.NameUses(lang, out)
	var lost []string
	for name, n := range before {
		if n == 0 || afterWrites[name] > 0 {
			continue
		}
		// Only interesting while something still READS it. A surviving
		// declaration is not a read: dropping the write and every reader is
		// just deleting dead state, and says nothing.
		if afterReads[name] > 0 {
			lost = append(lost, name)
		}
	}
	sort.Strings(lost)
	return lost
}

func (s *Server) applyBytes(abs string, orig, out []byte, mode os.FileMode, opts diagnosticOptions) (editOutcome, error) {
	uri := pathToURI(abs)

	// A commit:false batch is open — stage into it instead of validating
	// this edit alone. opts.commitBatch marks the edit that CLOSES the batch
	// (the first without commit:false): stage it, then validate the union.
	if b := s.currentBatch(); b != nil {
		if err := b.stage(abs, orig, out, mode, opts); err != nil {
			return editOutcome{}, err
		}
		if opts.commitBatch {
			oc := b.commit()
			if !oc.Conflict {
				s.closeBatch() // a conflict leaves the batch open to resolve
			}
			return oc, nil
		}
		return editOutcome{Staged: true, Pending: b.count}, nil
	}

	// Server-wide --validate (s.validateEdits) OR a per-call validate:true.
	validate := (opts.Validate || s.validateEdits) && s.manager != nil

	// Pre-edit error baseline — WORKSPACE-WIDE, so an edit that breaks an
	// importer (cross-file) is caught, not just same-file breakage. Reads the
	// store's last-published set (correct when files were already analyzed — the
	// common case: the model read/navigated them first). LIMITATION: a
	// never-analyzed file with pre-existing errors gets no baseline entry, so its
	// first edit could false-revert; a pre-touch analysis would fix it.
	var baseline map[string]int
	if validate {
		baseline = s.editBaseline()
	}

	if err := s.atomicWrite(abs, out, mode); err != nil {
		return editOutcome{}, err
	}
	s.refreshFileInIndex(abs, out)

	diags := s.collectDiagnostics([]string{uri}, map[string][]byte{uri: out}, opts)
	oc := editOutcome{Diags: diags, LostWrites: s.lostWrites(abs, orig, out)}
	if !validate {
		return oc, nil
	}
	if diags.lspOff {
		// The client turned this language's child off: a parse check only.
		lang := s.languageForFile(abs)
		if !parseBroke(lang, orig, out) {
			oc.Skipped, oc.SkipNote = "lsp-off", lspOffNote
			if symbols.LanguageByName(lang) == nil {
				oc.SkipNote = lspOffSkipNote // no grammar: no parse check either
			}
			return oc, nil
		}
		oc.Rejected = true
		oc.NewErrors = []string{uri + "\x00\x00the edit does not parse (tree-sitter, " + lang + "); this client has the child LSP off (lsp-off)"}
		var rerr error
		if orig == nil {
			rerr = os.Remove(abs)
		} else {
			rerr = s.atomicWrite(abs, orig, mode)
		}
		if rerr != nil {
			oc.RevertFailed, oc.RevertErr = true, rerr.Error()
			return oc, nil
		}
		oc.Reverted = true
		if orig != nil {
			s.refreshFileInIndex(abs, orig)
		} else {
			s.removeFileFromIndex(abs)
		}
		return oc, nil
	}
	if !diags.Available || diags.TimedOut {
		// Cannot PROVE safety — apply, but flag it loudly (fail-open).
		oc.Skipped, oc.SkipNote = s.skipped(diags)
		return oc, nil
	}
	introduced := fingerprintMinus(s.settleErrorFingerprint(3*time.Second, diags.edited), baseline)
	if len(introduced) == 0 {
		if diags.Unconfirmed {
			oc.Skipped, oc.SkipNote = unconfirmed()
			return oc, nil
		}
		oc.Validated = true
		return oc, nil
	}

	// Introduced errors → roll back.
	oc.Rejected, oc.NewErrors = true, introduced
	var rerr error
	if orig == nil {
		rerr = os.Remove(abs) // was a create — undo by deleting
	} else {
		rerr = s.atomicWrite(abs, orig, mode)
	}
	if rerr != nil {
		oc.RevertFailed, oc.RevertErr = true, rerr.Error()
		s.markBaselineUnknown(baseline) // the child holds text that is not on disk
		return oc, nil
	}
	oc.Reverted = true
	s.refreshFileInIndex(abs, orig)
	if orig == nil {
		// A create undone: the child is not told, and still holds the
		// removed file's text and errors.
		s.markBaselineUnknown(baseline)
		return oc, nil
	}
	// Tell the LSP we rolled back, and wait for its result for the
	// reverted text: the next edit's baseline is read from the store.
	s.awaitRevert(map[string][]byte{uri: orig}, baseline)
	return oc, nil
}

// validationTxn extends revert-on-new-diagnostics to MULTI-FILE refactors
// (rename / signature): it accumulates the pre-edit bytes + error fingerprint
// of every file the refactor touches, so the whole edit reverts as ONE unit if
// any new error appears. record() before each write; verify() after the
// refactor's collectDiagnostics. Inactive (a pure no-op) when validation is off
// or there's no LSP, so the refactor behaves exactly as before.
type validationTxn struct {
	s         *Server
	active    bool
	baseline  map[string]int
	originals map[string][]byte // uri → pre-edit bytes (first record wins)

	// batch, when set, redirects this txn into the OPEN commit:false batch:
	// record() feeds the batch's originals (so a refactor's files join the
	// batch's all-or-nothing revert) and verify() DEFERS to the batch's
	// commit. committing marks the edit that closes the batch.
	batch      *editBatch
	committing bool
}

func (s *Server) beginValidationTxn(opts diagnosticOptions) *validationTxn {
	// A commit:false batch is open — a refactor run now joins it instead of
	// validating on its own; opts.commitBatch marks the edit that closes it.
	if b := s.currentBatch(); b != nil {
		return &validationTxn{s: s, active: b.validate, batch: b, committing: opts.commitBatch}
	}
	return &validationTxn{
		s:      s,
		active: (opts.Validate || s.validateEdits) && s.manager != nil,
		// baseline is captured lazily on the first record() — workspace-wide,
		// before any file is written.
		originals: map[string][]byte{},
	}
}

// record captures a file's pre-edit bytes + current error set. Call BEFORE
// writing the file; the first record for a URI wins (the true original, never
// an intermediate rewrite).
func (t *validationTxn) record(uri string, orig []byte) {
	if t.batch != nil {
		// Feed the open batch: it owns the baseline (captured at open) and
		// the all-or-nothing revert set.
		if _, seen := t.batch.originals[uri]; !seen {
			t.batch.originals[uri] = orig
		}
		return
	}
	if !t.active {
		return
	}
	if t.baseline == nil {
		// Workspace-wide baseline, captured ONCE before the first write — so a
		// cross-file break introduced by ANY of the refactor's edits is caught.
		t.baseline = t.s.editBaseline()
	}
	if _, seen := t.originals[uri]; seen {
		return
	}
	t.originals[uri] = orig
}

// verify checks the post-edit error set across every recorded file. If the
// refactor introduced a NEW error it restores ALL files (all-or-nothing) and
// returns an editOutcome describing the rejection. A clean edit, validation
// off, or unprovable safety (no LSP / timed out → fail-open) returns
// !Rejected; the caller then attaches Validated/Skipped via annotate().
func (t *validationTxn) verify(diags editDiagnostics) editOutcome {
	oc := editOutcome{Diags: diags}
	// Routed into an open batch: defer to the batch. The committing edit
	// validates the union and closes it; every other edit just stages.
	if t.batch != nil {
		if !t.committing {
			return editOutcome{Diags: diags, Staged: true, Pending: t.batch.count}
		}
		bc := t.batch.commit()
		if !bc.Conflict {
			t.s.closeBatch() // a conflict leaves the batch open to resolve
		}
		return bc
	}
	if !t.active {
		return oc
	}
	if !diags.Available || diags.TimedOut {
		oc.Skipped, oc.SkipNote = t.s.skipped(diags)
		return oc
	}
	introduced := fingerprintMinus(t.s.settleErrorFingerprint(3*time.Second, diags.edited), t.baseline)
	if len(introduced) == 0 {
		if diags.Unconfirmed {
			oc.Skipped, oc.SkipNote = unconfirmed()
			return oc
		}
		oc.Validated = true
		return oc
	}
	oc.Rejected, oc.NewErrors = true, introduced
	t.s.revertFiles(t.originals, t.baseline, &oc)
	return oc
}

// skipped names why an edit could not be validated: no child LSP, or no
// diagnostics of the edited text within the wait (--validate-wait).
func (s *Server) skipped(d editDiagnostics) (reason, note string) {
	if d.lspOff && !d.TimedOut {
		return "lsp-off", lspOffSkipNote
	}
	if !d.TimedOut {
		return "no-lsp", ""
	}
	return "lsp-timeout", fmt.Sprintf("validation timed out after %s: the child LSP published no diagnostics for the edited text, or only while still loading or indexing; edit kept, not validated", s.diagWaitDuration())
}

// unconfirmed is the skip for an edit whose result came from a child not
// trusted for a clean result (editDiagnostics.Unconfirmed) and showed no
// new errors: that result may lack errors the child has not computed yet.
func unconfirmed() (reason, note string) {
	return "lsp-unconfirmed", "the child LSP is not trusted to publish a clean result only after a full analysis of the edited text, so its result (no new errors) does not validate the edit; edit kept, not validated (lsp: trust_clean_diagnostics: true in the language's config lets this server's clean results validate)"
}

// currentBatch / setBatch / closeBatch are session-keyed — see session.go.
// The node_edit handler holds editMu for the whole operation and sets
// s.activeSession, so those accessors read/write only the state it owns.

// stageHelp / rejectHelp are the INSTRUCTIVE strings — the transaction is
// undocumented in the schema and revealed here, at the moment it's needed.
const stageHelp = "staged · not validated · disk holds the intermediate. Add more with commit:false; " +
	"the next node_edit WITHOUT commit:false validates and commits the whole batch atomically; " +
	"rollback:true discards it back to the last committed state."

const rejectHelp = "If this edit is correct but needs COUNTERPART edits to be valid (change a signature AND its " +
	"body/callers together), stage them as one atomic batch: pass commit:false on each (applied but NOT " +
	"validated), then the first node_edit WITHOUT commit:false validates and commits the union; rollback:true reverts."

// batchResponse turns a staged / committed batch outcome into the node_edit
// response. handled=false when the edit wasn't part of a batch (the caller
// then builds its normal response).
func batchResponse(oc editOutcome) (content []Content, isErr, handled bool) {
	if oc.Staged {
		return jsonContent(map[string]any{"staged": true, "pending": oc.Pending, "help": stageHelp}), false, true
	}
	if oc.BatchCommitted {
		return jsonContent(batchCommitPayload(oc)), oc.Rejected || oc.Conflict, true
	}
	return nil, false, false
}

func batchCommitPayload(oc editOutcome) map[string]any {
	if oc.Conflict {
		return map[string]any{
			"committed":    false,
			"conflict":     true,
			"pending":      oc.Pending,
			"changedFiles": oc.Conflicts,
			"note":         fmt.Sprintf("commit ABORTED — %d staged file(s) changed on disk since you staged them; nothing was overwritten or reverted", len(oc.Conflicts)),
			"help":         "the batch is still OPEN. Inspect the outside changes, then either re-stage over them (commit:false) and commit, or rollback:true to discard your staged edits back to the pre-batch state.",
		}
	}
	if !oc.Rejected {
		p := map[string]any{
			"committed": true,
			"edits":     oc.Pending,
			"note":      fmt.Sprintf("committed %d edit(s) atomically", oc.Pending),
		}
		oc.annotate(p)
		return p
	}
	p := map[string]any{
		"committed": false,
		"pending":   oc.Pending,
		"newErrors": readableNewErrors(oc.NewErrors),
	}
	if oc.RevertFailed {
		p["revertFailed"], p["revertError"] = true, oc.RevertErr
		p["note"] = "batch REJECTED and revert FAILED — disk may be inconsistent"
	} else {
		p["reverted"] = true
		p["note"] = fmt.Sprintf("batch REJECTED — the union introduced errors; reverted all %d edit(s)", oc.Pending)
		p["help"] = "keep fixing with commit:false edits then commit, or rollback:true to discard."
	}
	return p
}

// atomicWrite is the temp+rename the apply* paths all share.
func (s *Server) atomicWrite(abs string, data []byte, mode os.FileMode) error {
	tmp := abs + ".poly-lsp-mcp.tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	if err := os.Rename(tmp, abs); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// errorFingerprintAll reads the store's CURRENT error diagnostics across the
// WHOLE workspace as a location-independent multiset keyed by (uri∷code∷message)
// — errors only (multiplex severity 1), line excluded (it shifts under an edit).
//
// Workspace-wide is what makes validation CROSS-FILE: an edit that breaks an
// IMPORTER (e.g. renaming a type its callers use) surfaces as a new error on the
// SIBLING file, which a single-file fingerprint never saw. gopls publishes at
// the package level, so those sibling errors are already in the store after the
// edit's collectDiagnostics (siblings default-on) settled. Empty with no LSP.
func (s *Server) errorFingerprintAll() map[string]int {
	return s.errorFingerprint(nil)
}

// errorFingerprint is errorFingerprintAll with each URI in edited read
// through its result (editedResult.current), not the store's last
// publish for it: that can be for other text (an earlier version, or an
// unversioned publish the edit's wait did not take). The edit's error
// check judges the edited text only.
func (s *Server) errorFingerprint(edited map[string]editedResult) map[string]int {
	m := map[string]int{}
	if s.manager == nil {
		return m
	}
	store := s.manager.Diagnostics()
	snap := store.Snapshot()
	for uri, r := range edited {
		snap[uri] = r.current(store, uri)
	}
	// A language off for the editing client is not its to judge.
	off := s.lspOff(s.activeSession)
	for uri, diags := range snap {
		if s.lspOffURI(off, uri) {
			continue
		}
		for _, d := range diags {
			if d.Severity != 1 { // 1 = Error
				continue
			}
			m[fmt.Sprintf("%s\x00%v\x00%s", uri, d.Code, d.Message)]++
		}
	}
	return m
}

// settleErrorFingerprint returns the workspace error fingerprint AFTER the
// diagnostic store quiesces, the edited URIs read through edited (see
// errorFingerprint). gopls republishes a whole package on a change, but
// per-file publishes can lag the edited file — so a cross-file (sibling)
// error may not be in the store the instant collectDiagnostics returns
// (which only waits on the EDITED file). This polls until the error set is
// unchanged for a short window, bounded by max. Fast for a clean edit
// (stable immediately); waits only while errors are still arriving. An
// error a later publish for the edited version reports in that window
// counts (Xcode's sourcekit-lsp: [] for the version, then its error).
func (s *Server) settleErrorFingerprint(max time.Duration, edited map[string]editedResult) map[string]int {
	const step = 100 * time.Millisecond
	const stableNeeded = 250 * time.Millisecond
	prev := s.errorFingerprint(edited)
	var stable time.Duration
	for waited := time.Duration(0); waited < max; waited += step {
		time.Sleep(step)
		cur := s.errorFingerprint(edited)
		if fingerprintEqual(prev, cur) {
			if stable += step; stable >= stableNeeded {
				return cur
			}
		} else {
			stable = 0
		}
		prev = cur
	}
	return prev
}

func fingerprintEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// fingerprintMinus returns the keys whose post-count exceeds baseline — the
// errors this edit ADDED. Multiset so "had 1, now 2" still trips. Conservative:
// may miss a new error sharing a message with a pre-existing one, but never
// blocks a safe edit.
func fingerprintMinus(post, base map[string]int) []string {
	var out []string
	for k, n := range post {
		for i := 0; i < n-base[k]; i++ {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// attach writes the standard diagnostic fields (+ validation status) into a
// success payload.
func (oc editOutcome) attach(p map[string]any) {
	d := oc.Diags
	p["diagnosticsAvailable"] = d.Available
	p["diagnosticsTimedOut"] = d.TimedOut
	p["diagnostics"] = d.Items
	if d.DroppedDiagnostics > 0 {
		p["droppedDiagnostics"] = d.DroppedDiagnostics
	}
	if oc.Validated {
		p["validated"] = true
	} else if oc.Skipped != "" {
		p["validated"] = false
		p["validationSkipped"] = oc.Skipped
		if oc.SkipNote != "" {
			p["validationNote"] = oc.SkipNote
		}
	}
	if len(oc.LostWrites) > 0 {
		// `alert` sorts before `diagnostics`, for the reason the rejection's
		// `action` field does: a note behind the evidence is a note unread.
		p["lostWrites"] = oc.LostWrites
		p["alert"] = fmt.Sprintf(
			"this edit removed the last ASSIGNMENT to %s, which is still referenced — "+
				"so it now keeps its zero value forever and no compiler will say so. "+
				"If that was not deliberate, restore the assignment; check who depends on it with "+
				"node_query(selector: \"field[name=%s]::in\").",
			strings.Join(quoteAll(oc.LostWrites), ", "), oc.LostWrites[0])
	}
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strconv.Quote(s)
	}
	return out
}

// annotate adds ONLY the validation-status fields (validated /
// validationSkipped) to a payload that already carries its own diagnostics
// fields — the multi-file refactor paths build those manually. No-op on a
// zero-value outcome (validation off / nested).
func (oc editOutcome) annotate(p map[string]any) {
	if oc.Validated {
		p["validated"] = true
	} else if oc.Skipped != "" {
		p["validated"] = false
		p["validationSkipped"] = oc.Skipped
		if oc.SkipNote != "" {
			p["validationNote"] = oc.SkipNote
		}
	}
}

// rejection builds the REJECTED payload after a revert. label names the edit
// target (node addr or file) for the note.
func (oc editOutcome) rejection(label string) map[string]any {
	p := map[string]any{
		"rejected":    true,
		"newErrors":   readableNewErrors(oc.NewErrors),
		"diagnostics": oc.Diags.Items,
	}
	if oc.RevertFailed {
		p["revertFailed"] = true
		p["revertError"] = oc.RevertErr
		p["note"] = "REJECTED — edit introduced errors AND revert FAILED (" + label + "); file may be inconsistent"
	} else {
		p["reverted"] = true
		p["note"] = "REJECTED — reverted; edit introduced errors (" + label + ")"
		p["help"] = rejectHelp
		// `action` sorts BEFORE `diagnostics`, which is the whole point.
		//
		// rejectHelp was already attached here and was already correct, and a
		// dun session on 2026-08-05 still looped on the same rejection twice
		// while adding a --docker-network flag: Go serializes map keys
		// alphabetically, so `diagnostics` came first and buried the recovery
		// at char ~1600 of a 2036-char payload. Payload prose gets skipped the
		// same way description prose does (measured on the volcycle bench:
		// a read hint ignored 44 times out of 44), so the recovery has to
		// arrive as a short imperative in the first field read, not as an
		// explanation after the evidence.
		if act := incompleteEditAction(oc.NewErrors); act != "" {
			p["action"] = act
		}
	}
	return p
}

// incompleteEditErrorCodes are the diagnostics that mean "this edit is HALF
// DONE", not "this edit is wrong": a declaration with no use yet, or a use
// whose declaration is still coming. They are the expected state in the middle
// of any multi-part change, so a rejection carrying only these is a signal to
// stage rather than to rethink.
var incompleteEditErrorCodes = []string{
	"UnusedVar", "UnusedImport", "UndeclaredName", "MissingFieldOrMethod",
	"declared and not used", "imported and not used", "undefined:",
}

// incompleteEditAction returns the imperative to lead the rejection with, when
// every new error looks like a half-finished edit rather than a broken one.
// Empty when any error is something else — claiming "just stage it" about a
// genuine mistake would turn a correct rejection into bad advice.
func incompleteEditAction(newErrors []string) string {
	if len(newErrors) == 0 {
		return ""
	}
	for _, k := range newErrors {
		hit := false
		for _, code := range incompleteEditErrorCodes {
			if strings.Contains(k, code) {
				hit = true
				break
			}
		}
		if !hit {
			return ""
		}
	}
	return "RETRY THIS EXACT CALL WITH commit:false. These errors say the edit is INCOMPLETE, " +
		"not wrong — a declaration with no use yet, or a use whose declaration is still coming. " +
		"commit:false applies without validating; make the counterpart edits the same way; then one " +
		"final node_edit WITHOUT commit:false validates and commits the batch atomically."
}

// readableNewErrors turns the internal error fingerprints into something a
// reader can use.
//
// NewErrors carries errorFingerprintAll's dedup KEYS — "uri\x00code\x00message"
// — and they were serialized into the response as-is, NUL bytes and file://
// URIs included. That is an internal identity leaking out as an explanation.
func readableNewErrors(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		parts := strings.Split(k, "\x00")
		if len(parts) != 3 {
			out = append(out, k)
			continue
		}
		file := strings.TrimPrefix(parts[0], "file://")
		if parts[1] == "" || parts[1] == "<nil>" {
			out = append(out, fmt.Sprintf("%s: %s", file, parts[2]))
			continue
		}
		out = append(out, fmt.Sprintf("%s: %s: %s", file, parts[1], parts[2]))
	}
	return out
}
