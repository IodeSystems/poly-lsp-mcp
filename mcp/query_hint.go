package mcp

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A silent no-op is the one outcome a filter must never have. The parser
// already holds that line (a literal `|` is refused rather than matched
// against nothing) and so does the resolver (a missing #id errors with
// nearestSyms). The door still open is the one in the middle: a
// syntactically PERFECT selector that matches nothing returns
// {"matches":[],"returned":0} and says nothing at all.
//
// The measured cost, from a dun session that fixed a real data race with
// these tools (2026-08-02): `method name~=newInputStream` returned ∅ and
// `func name~=newInputStream` — the very next call — returned 2. The
// symbol was a func, the caller guessed method, and a wrong guess about
// the TAG is byte-identical to the symbol not existing. Silence cannot be
// told from absence, so one more unlucky guess ends with the model
// concluding the code is not there and going somewhere else.
//
// zeroResultHint answers "which clause emptied it" with ONE line naming
// ONE alternative. That bound is load-bearing: the moment a hint lists
// five candidates it is a search result wearing an error's clothes, and
// callers read hints instead of narrowing selectors.
//
// It never GUESSES. Every probe below either finds a concrete node and
// names it, or stays quiet.
func (e *engine) zeroResultHint(list selectorList) string {
	// A budget blow already explains the emptiness, and blaming the
	// selector for the clock would be a lie.
	if e.workExceeded {
		return ""
	}
	if len(list) != 1 {
		return "" // a union's emptiness has no single clause to name
	}
	cx := list[0]
	if !probeSafe(cx) {
		return ""
	}

	// Order is by CERTAINTY, not by the plan's listing order. A path that
	// names no file is a fact about the workspace; a relaxation is a
	// deduction from one probe. State facts first.
	if h := e.hintDeadPath(cx); h != "" {
		return h
	}
	if h := e.hintDropType(cx); h != "" {
		return h
	}
	if h := e.hintNearName(cx); h != "" {
		return h
	}
	return e.hintDropLastAttr(cx)
}

// probeSafe reports whether a complex is one a probe may re-run.
//
// The exclusions are about COST and MEANING, not correctness of the
// snapshot in probe(): a ref element would spend real child-LSP
// round-trips to explain an empty result, a generated element (::grep /
// ::comment / ::signature) mints nodes rather than filtering them so
// "which clause emptied it" has no answer in the containment tree, and a
// group has no single subject compound to relax.
func probeSafe(cx selComplex) bool {
	for i := range cx.elems {
		c := cx.elems[i].comp
		if c == nil {
			return false // a parenthesized group
		}
		if c.isRef || c.isGenerated() {
			return false
		}
		for _, ps := range c.pseudos {
			if ps.kind == pseudoRecursive {
				return false // :recursive asks a child LSP
			}
		}
	}
	return len(cx.elems) > 0
}

// hintProbeOps bounds ONE probe. A hint must not become a second query
// the caller pays for, so a probe gets ~1% of the deterministic cap and —
// because it stops at its FIRST match (need=1) — almost never approaches
// it. A probe that runs out returns nothing: on a workspace big enough to
// exhaust this, the result stays as silent as it is today rather than
// becoming slow.
//
// The ACTUAL allowance is min(this, what the caller's own budget has
// left). A caller who asked for 1000ops asked for a cheap answer, and
// spending 50× that to explain it would be answering a question they
// declined to pay for — so a tight budget buys a cheap probe, and a
// spent one buys none.
const hintProbeOps = 50_000

// probeState is every engine field a probe run can disturb.
//
// A probe must be INVISIBLE: the note, cost trace and edge legibility a
// caller reads describe THEIR query, not the relaxed one run to explain
// it. Probes reuse the LIVE engine on purpose — a fresh one would re-walk
// the workspace and re-parse every file it touched, which is the opposite
// of cheap — so every field the evaluator writes and the payload later
// reads is listed here. Add to this list, not around it.
type probeState struct {
	workLeft     int
	workExceeded bool
	timedOut     bool
	spendTick    int
	deadline     time.Time
	cap          int
	capHit       bool
	costStack    []costFrame
	blownElem    *selElem

	lspLeft     int
	lspAsked    int
	lspResolved int

	maxHopReached    int
	unsettledFromHop int
	transUnsettled   int
	hopCounted       map[*treeNode]bool

	recursiveUnconfirmed  bool
	implementsUnavailable bool
}

// probe evaluates a relaxed selector on its own small budget and returns
// its first match, or nil if it matched nothing / ran out of budget.
//
// The two are deliberately indistinguishable to the caller: an
// inconclusive probe and an empty one both mean "nothing to say", and a
// hint that hedged ("possibly, but I stopped looking") would be worse
// than silence.
func (e *engine) probe(list selectorList) *treeNode {
	ops := min(hintProbeOps, e.workLeft)
	if ops <= 0 {
		return nil // the caller's budget is spent; a hint is not worth borrowing against
	}
	saved := e.probeSnapshot()
	// The probe's own world: a bounded ops budget, no clock (a hint must be
	// reproducible run to run), and throwaway accumulators so nothing it
	// counts can leak into what the caller is told.
	e.workLeft, e.workExceeded, e.timedOut, e.spendTick = ops, false, false, 0
	e.deadline = time.Time{}
	e.cap, e.capHit = 0, false
	e.costStack, e.hopCounted = nil, nil

	// need=1: stop at the first match. Every hint names ONE alternative,
	// so a second match would be work spent on something never said.
	rows, _ := e.evaluateCapped(list, 1)
	blown := e.workExceeded

	e.probeRestore(saved)

	if blown || len(rows) == 0 {
		return nil
	}
	return rows[0]
	// elemCost is not restored: every probe runs on CLONED elements, so
	// its entries are keyed by pointers costTrace(list) never looks up.
}

func (e *engine) probeSnapshot() probeState {
	return probeState{
		workLeft: e.workLeft, workExceeded: e.workExceeded, timedOut: e.timedOut,
		spendTick: e.spendTick, deadline: e.deadline, cap: e.cap, capHit: e.capHit,
		costStack: e.costStack, blownElem: e.blownElem,
		lspLeft: e.lspLeft, lspAsked: e.lspAsked, lspResolved: e.lspResolved,
		maxHopReached: e.maxHopReached, unsettledFromHop: e.unsettledFromHop,
		transUnsettled: e.transUnsettled, hopCounted: e.hopCounted,
		recursiveUnconfirmed:  e.recursiveUnconfirmed,
		implementsUnavailable: e.implementsUnavailable,
	}
}

func (e *engine) probeRestore(s probeState) {
	e.workLeft, e.workExceeded, e.timedOut, e.spendTick =
		s.workLeft, s.workExceeded, s.timedOut, s.spendTick
	e.deadline, e.cap, e.capHit = s.deadline, s.cap, s.capHit
	e.costStack, e.blownElem = s.costStack, s.blownElem
	e.lspLeft, e.lspAsked, e.lspResolved = s.lspLeft, s.lspAsked, s.lspResolved
	e.maxHopReached, e.unsettledFromHop = s.maxHopReached, s.unsettledFromHop
	e.transUnsettled, e.hopCounted = s.transUnsettled, s.hopCounted
	e.recursiveUnconfirmed = s.recursiveUnconfirmed
	e.implementsUnavailable = s.implementsUnavailable
}

// cloneComplex copies a complex deeply enough that a probe can rewrite a
// compound's tag or attrs without touching the caller's parsed selector.
// Only those two fields are ever mutated, so the pseudos/group slices are
// shared by design.
func cloneComplex(cx selComplex) selComplex {
	out := cx
	out.elems = make([]selElem, len(cx.elems))
	copy(out.elems, cx.elems)
	for i := range out.elems {
		c := *out.elems[i].comp
		c.attrs = append([]selAttr(nil), c.attrs...)
		out.elems[i].comp = &c
	}
	return out
}

// ------------------------------------------------------- probe: dead path

// hintDeadPath reports a [path] filter that names no file or dir in the
// workspace. Nothing downstream of it could ever match, so this is the
// whole answer — and unlike the relaxation probes it needs no evaluation
// at all, just the file/dir nodes the tree walk already built.
//
// The measured shape: `path=cmd/dun/inputstream.go`, where the model
// guessed the filename from a TEST name and the symbol lived in main.go.
func (e *engine) hintDeadPath(cx selComplex) string {
	paths := e.workspacePaths()
	for i := range cx.elems {
		for _, a := range cx.elems[i].comp.attrs {
			if a.axis != attrPath || anyPathMatches(paths, a) {
				continue
			}
			clause := "path" + selOpSpelling(a.op) + a.value
			// A guess is only offered where the DIRECTORY is real: the
			// caller was in the right place and invented a filename, which
			// is a correction. Nearest-path across the whole workspace
			// would be a shot in the dark dressed as an answer.
			if a.op == selExact {
				if near := nearestInDir(paths, a.value); near != "" {
					return fmt.Sprintf("no file or dir is at %s — nothing downstream of that filter "+
						"can match. Nearest real path in that directory: %s", clause, near)
				}
			}
			return fmt.Sprintf("no file or dir matches %s — nothing downstream of that filter "+
				"can match. Paths are workspace-relative; check it against `:root > *`", clause)
		}
	}
	return ""
}

// workspacePaths lists every dir and file path in the tree. It walks
// `children` directly rather than kids(), so a file's symbols are never
// parsed just to answer a question about paths.
func (e *engine) workspacePaths() []string {
	var out []string
	var walk func(n *treeNode)
	walk = func(n *treeNode) {
		for _, c := range n.children {
			if c.class != "dir" && c.class != "file" {
				continue
			}
			out = append(out, c.file)
			walk(c)
		}
	}
	walk(e.project)
	return out
}

func anyPathMatches(paths []string, a selAttr) bool {
	for _, p := range paths {
		if a.op == selRegex {
			if a.re != nil && a.re.MatchString(p) {
				return true
			}
			continue
		}
		if matchAttrOp(p, a.op, a.value) {
			return true
		}
	}
	return false
}

// nearestInDir finds the closest real path sharing want's directory. The
// directory must exist as written — "you are in the right folder, that
// file is not in it" is a correction; "here is a similarly-spelled file
// somewhere else" is noise.
func nearestInDir(paths []string, want string) string {
	dir := filepath.Dir(want)
	if dir == "." || dir == string(filepath.Separator) {
		return "" // a bare filename names no directory to be right about
	}
	var siblings []string
	for _, p := range paths {
		if filepath.Dir(p) == dir {
			siblings = append(siblings, filepath.Base(p))
		}
	}
	if len(siblings) == 0 {
		return "" // the directory itself is wrong, not just the filename
	}
	if near := nearestName(filepath.Base(want), siblings); near != "" {
		return filepath.Join(dir, near)
	}
	return ""
}

// ------------------------------------------------------- probe: drop the tag

// hintDropType re-runs the selector with the subject's TAG removed. This
// is the `method name~=newInputStream` case outright: the filters are
// right, the tag is wrong, and the caller had to know which of func /
// method / field a symbol is BEFORE it could ask.
//
// Reporting the found node's real tag and address makes the answer
// actionable in one step — the address feeds straight into node_read.
func (e *engine) hintDropType(cx selComplex) string {
	sub := cx.elems[len(cx.elems)-1].comp
	if sub.anyType || sub.class == "" {
		return "" // nothing written to drop
	}
	relaxed := cloneComplex(cx)
	c := relaxed.elems[len(relaxed.elems)-1].comp
	c.class, c.anyType, c.implied = "", true, false
	hit := e.probe(selectorList{relaxed})
	if hit == nil {
		return ""
	}
	return fmt.Sprintf("no %s matches — the TAG is what emptied it, not the filters. "+
		"Same query without it matches %s #'%s'. Retry with that tag, or with `*`",
		sub.class, hit.class, hit.addr())
}

// ------------------------------------------------------ probe: near-miss name

// hintNearName answers an exact name that is nearly right — a typo or a
// case slip — with the name the workspace actually has.
//
// The candidate comes from the index (every occurrence, cheap) but the
// ANSWER is a declaration: the nearest spelling is looked up in the tree
// and reported only if it is really there. Suggesting a name off the
// index alone would happily propose `Println`, whose declaration is in
// the stdlib — a retry that returns zero again, which is worse than the
// silence it replaced.
//
// This runs BEFORE the drop-last-attribute probe because for a selector
// whose only clause IS the name, dropping it says "without your one
// filter, everything matches", which is true and useless.
func (e *engine) hintNearName(cx selComplex) string {
	idx := e.s.getIndex()
	if idx == nil {
		return ""
	}
	sub := cx.elems[len(cx.elems)-1].comp
	for _, a := range sub.attrs {
		if a.op != selExact || a.axis == attrPath {
			continue
		}
		// A dotted path or an address is a different question (nearestSyms
		// answers that one at resolution time); this probe is for a leaf
		// name the caller misspelled.
		if a.value == "" || strings.ContainsAny(a.value, ".#/") {
			continue
		}
		if idx.NameFreq(a.value) > 0 {
			continue // the name exists; something ELSE emptied the result
		}
		near := nearestName(a.value, idx.Names())
		if near == "" || near == a.value {
			continue
		}
		// Index-seeded: only files that actually contain `near` are walked,
		// never the workspace.
		hits := e.declsNamed(near)
		if len(hits) == 0 {
			continue // an occurrence, not a declaration — nothing to retry with
		}
		sort.Slice(hits, func(i, j int) bool { return nodeLess(hits[i], hits[j]) })
		return fmt.Sprintf("nothing is named %q — the nearest thing that IS declared is "+
			"%s #'%s'. Retry with %q", a.value, hits[0].class, hits[0].addr(), near)
	}
	return ""
}

// -------------------------------------------------- probe: drop the last clause

// hintDropLastAttr is the general form: drop the LAST attribute written
// and, if that matches, name it as the clause that emptied the set.
//
// It only fires where something NARROWING survives the drop — another
// attribute, or a preceding chain element. A tag does not count: relaxing
// `func name^=Zzz` to `func` reports that some unrelated func exists,
// which turns "your filter emptied it" into "your filter filtered".
// Measured on this repo while writing it, and silence is the honest
// answer there — the caller wrote one filter, it matched nothing, and
// there is no clause to blame but the one they can already see.
func (e *engine) hintDropLastAttr(cx selComplex) string {
	elem, attr := -1, -1
	narrowing := len(cx.elems) - 1
	for i := range cx.elems {
		c := cx.elems[i].comp
		narrowing += len(c.attrs)
		if len(c.attrs) > 0 {
			elem, attr = i, len(c.attrs)-1
		}
	}
	if elem < 0 || narrowing < 2 {
		return ""
	}
	dropped := cx.elems[elem].comp.attrs[attr]
	relaxed := cloneComplex(cx)
	c := relaxed.elems[elem].comp
	c.attrs = append(c.attrs[:attr], c.attrs[attr+1:]...)
	hit := e.probe(selectorList{relaxed})
	if hit == nil {
		return ""
	}
	return fmt.Sprintf("the %s filter is what emptied it: without that one clause, "+
		"%s #'%s' matches", renderAttr(dropped), hit.class, hit.addr())
}

// renderAttr spells one attribute back the way it was written — the
// clause a hint or cost trace has to name.
func renderAttr(a selAttr) string {
	if a.axis == attrID && a.op == selExact {
		return "#" + a.value
	}
	axis := "name"
	if a.axis == attrPath {
		axis = "path"
	}
	return "[" + axis + selOpSpelling(a.op) + a.value + "]"
}
