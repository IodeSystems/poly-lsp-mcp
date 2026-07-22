package mcp

import (
	"fmt"
	"os"
	"sort"
	"time"
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
	Skipped      string   // "" | "no-lsp" | "lsp-timeout" — safety not provable

	// Batch (commit:false transaction) signals — set only when an edit
	// runs inside an open batch (see editBatch).
	Staged         bool // applied to the open batch, NOT validated, NOT committed
	BatchCommitted bool // this edit closed the batch (validate the union then persist/revert)
	Pending        int  // edits in the batch (staged count, or the number just committed)
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
	validate  bool
	baseline  map[string]int    // workspace error fingerprint at open (if validating)
	originals map[string][]byte // uri → pre-batch bytes (first write wins) — for revert
	count     int
}

func (s *Server) openBatch(opts diagnosticOptions) *editBatch {
	b := &editBatch{
		s:         s,
		validate:  (opts.Validate || s.validateEdits) && s.manager != nil,
		originals: map[string][]byte{},
	}
	if b.validate {
		b.baseline = s.errorFingerprintAll()
	}
	return b
}

// stage writes one edit into the batch: record the file's pre-batch bytes
// (first write wins), write to disk, refresh the index, and feed the child
// LSP — but do NOT validate. Later edits and resolves see it on disk.
func (b *editBatch) stage(abs string, orig, out []byte, mode os.FileMode, opts diagnosticOptions) error {
	uri := pathToURI(abs)
	if _, seen := b.originals[uri]; !seen {
		b.originals[uri] = orig
	}
	if err := b.s.atomicWrite(abs, out, mode); err != nil {
		return err
	}
	b.s.refreshFileInIndex(abs, out)
	// Feed the child LSP the staged content (so the commit's union
	// validation sees it) only when we're actually validating — no manager
	// means nothing to feed.
	if b.validate {
		b.s.collectDiagnostics([]string{uri}, map[string][]byte{uri: out}, opts)
	}
	b.count++
	return nil
}

// commit validates the whole batch: if the union introduced no new error
// the staged bytes stand; otherwise EVERY touched file reverts to its
// pre-batch bytes (all-or-nothing). Validation off / no LSP → persist as-is
// (fail-open, flagged via Skipped).
func (b *editBatch) commit() editOutcome {
	oc := editOutcome{BatchCommitted: true, Pending: b.count}
	if !b.validate {
		return oc // nothing to prove; staged bytes stand
	}
	introduced := fingerprintMinus(b.s.settleErrorFingerprint(3*time.Second), b.baseline)
	if len(introduced) == 0 {
		oc.Validated = true
		return oc
	}
	oc.Rejected, oc.NewErrors = true, introduced
	b.revertAll(&oc)
	return oc
}

// revertAll restores every touched file to its pre-batch bytes.
func (b *editBatch) revertAll(oc *editOutcome) {
	for uri, orig := range b.originals {
		abs := uriToPath(uri)
		mode := os.FileMode(0o644)
		if info, err := os.Stat(abs); err == nil {
			mode = info.Mode().Perm()
		}
		if err := b.s.atomicWrite(abs, orig, mode); err != nil {
			oc.RevertFailed, oc.RevertErr = true, err.Error()
			continue
		}
		b.s.refreshFileInIndex(abs, orig)
		if b.s.manager != nil {
			if child := b.s.manager.RouteByURI(uri); child != nil {
				b.s.notifyChildOfEdit(child, uri, orig)
			}
		}
	}
	oc.Reverted = !oc.RevertFailed
}

// applyBytes writes `out` to abs (atomic temp+rename), refreshes the index, and
// collects diagnostics. Under opts.Validate it fingerprints the pre-edit error
// set first and, if the edit introduces a NEW error, restores `orig` and
// reports a rejection. orig==nil means a CREATE (revert = delete the new file).
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
			s.closeBatch()
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
		baseline = s.errorFingerprintAll()
	}

	if err := s.atomicWrite(abs, out, mode); err != nil {
		return editOutcome{}, err
	}
	s.refreshFileInIndex(abs, out)

	diags := s.collectDiagnostics([]string{uri}, map[string][]byte{uri: out}, opts)
	oc := editOutcome{Diags: diags}
	if !validate {
		return oc, nil
	}
	if !diags.Available || diags.TimedOut {
		// Cannot PROVE safety — apply, but flag it loudly (fail-open).
		oc.Skipped = "no-lsp"
		if diags.TimedOut {
			oc.Skipped = "lsp-timeout"
		}
		return oc, nil
	}
	introduced := fingerprintMinus(s.settleErrorFingerprint(3*time.Second), baseline)
	if len(introduced) == 0 {
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
		return oc, nil
	}
	oc.Reverted = true
	s.refreshFileInIndex(abs, orig)
	if child := s.manager.RouteByURI(uri); child != nil {
		s.notifyChildOfEdit(child, uri, orig) // tell the LSP we rolled back
	}
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
}

func (s *Server) beginValidationTxn(opts diagnosticOptions) *validationTxn {
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
	if !t.active {
		return
	}
	if t.baseline == nil {
		// Workspace-wide baseline, captured ONCE before the first write — so a
		// cross-file break introduced by ANY of the refactor's edits is caught.
		t.baseline = t.s.errorFingerprintAll()
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
	if !t.active {
		return oc
	}
	if !diags.Available || diags.TimedOut {
		oc.Skipped = "no-lsp"
		if diags.TimedOut {
			oc.Skipped = "lsp-timeout"
		}
		return oc
	}
	introduced := fingerprintMinus(t.s.settleErrorFingerprint(3*time.Second), t.baseline)
	if len(introduced) == 0 {
		oc.Validated = true
		return oc
	}
	oc.Rejected, oc.NewErrors = true, introduced
	for uri, orig := range t.originals {
		abs := uriToPath(uri)
		mode := os.FileMode(0o644)
		if info, err := os.Stat(abs); err == nil {
			mode = info.Mode().Perm()
		}
		if err := t.s.atomicWrite(abs, orig, mode); err != nil {
			oc.RevertFailed, oc.RevertErr = true, err.Error()
			continue
		}
		t.s.refreshFileInIndex(abs, orig)
		if child := t.s.manager.RouteByURI(uri); child != nil {
			t.s.notifyChildOfEdit(child, uri, orig)
		}
	}
	oc.Reverted = !oc.RevertFailed
	return oc
}

// currentBatch / closeBatch access the open batch. The node_edit handler
// holds editMu for the whole operation, so these are unlocked reads/writes
// of state only it touches.
func (s *Server) currentBatch() *editBatch { return s.editBatch }
func (s *Server) closeBatch()              { s.editBatch = nil }

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
		return jsonContent(batchCommitPayload(oc)), oc.Rejected, true
	}
	return nil, false, false
}

func batchCommitPayload(oc editOutcome) map[string]any {
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
		"newErrors": oc.NewErrors,
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
	m := map[string]int{}
	if s.manager == nil {
		return m
	}
	for uri, diags := range s.manager.Diagnostics().Snapshot() {
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
// diagnostic store quiesces. gopls republishes a whole package on a change, but
// per-file publishes can lag the edited file — so a cross-file (sibling) error
// may not be in the store the instant collectDiagnostics returns (which only
// waits on the EDITED file). This polls until the error set is unchanged for a
// short window, bounded by max. Fast for a clean edit (stable immediately);
// waits only while errors are still arriving.
func (s *Server) settleErrorFingerprint(max time.Duration) map[string]int {
	const step = 100 * time.Millisecond
	const stableNeeded = 250 * time.Millisecond
	prev := s.errorFingerprintAll()
	var stable time.Duration
	for waited := time.Duration(0); waited < max; waited += step {
		time.Sleep(step)
		cur := s.errorFingerprintAll()
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
	}
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
	}
}

// rejection builds the REJECTED payload after a revert. label names the edit
// target (node addr or file) for the note.
func (oc editOutcome) rejection(label string) map[string]any {
	p := map[string]any{
		"rejected":    true,
		"newErrors":   oc.NewErrors,
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
	}
	return p
}
