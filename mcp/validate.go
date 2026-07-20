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
}

// applyBytes writes `out` to abs (atomic temp+rename), refreshes the index, and
// collects diagnostics. Under opts.Validate it fingerprints the pre-edit error
// set first and, if the edit introduces a NEW error, restores `orig` and
// reports a rejection. orig==nil means a CREATE (revert = delete the new file).
func (s *Server) applyBytes(abs string, orig, out []byte, mode os.FileMode, opts diagnosticOptions) (editOutcome, error) {
	uri := pathToURI(abs)
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
	}
	return p
}
