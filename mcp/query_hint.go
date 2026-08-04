package mcp

import (
	"fmt"
	"path/filepath"
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
	e.openHintBudget()

	// Order is by CERTAINTY, not by the plan's listing order. A path that
	// names no file is a fact about the workspace; a chain that is already
	// empty halfway along makes every later clause irrelevant; a relaxation
	// is a deduction from one probe. State facts first.
	if h := e.hintDeadPath(cx); h != "" {
		return h
	}
	if h := e.hintDeadPrefix(cx); h != "" {
		return h
	}
	if h := e.hintDropTag(cx); h != "" {
		return h
	}
	if h := e.hintNearName(cx); h != "" {
		return h
	}
	return e.hintDropLastAttr(cx)
}

// probeSafe reports whether a complex is one a probe may re-run.
//
// Only one STRUCTURAL exclusion is left: a parenthesized group has no
// single subject compound to relax, and cloneComplex has no compound to
// copy. Edges and generated elements used to be excluded here on the
// grounds that re-running them would spend child-LSP round-trips — that
// is now enforced where it actually happens (probeBlocked in query.go /
// precision.go), which is both stricter and less blunt: an edge chain
// whose refs the caller's own query already materialized probes for free,
// and one that would need new edge work is discarded rather than guessed.
func probeSafe(cx selComplex) bool {
	for i := range cx.elems {
		if cx.elems[i].comp == nil {
			return false // a parenthesized group
		}
	}
	return len(cx.elems) > 0
}

// The allowance shared by ALL probes for one zero result. A hint must not
// become a second query the caller pays for.
//
// Ops alone were not enough, and the case that proved it is worth keeping:
// on this repo `interface method name=Zzzz` answered in 4µs — the planner
// seeds an exact-name tip straight from the INDEX, and `Zzzz` occurs
// nowhere, so nothing is walked. Every relaxation of it DROPS that anchor
// and falls back to a full forward walk: 415 ms of hint to explain 4 µs of
// query. The ops cap did not catch it because one `spend(1)` can hide a
// tree-sitter parse. So relaxing a selector can cost far more than the
// selector, and the budget has to be wall-clock to see that.
//
// The clock is not enough either — it cannot bound a tight in-memory loop
// deterministically — so both apply, and either one running out ends
// probing. A hint is best-effort by construction: on a workspace where
// explaining the result is slow, the result stays as silent as it was
// before, rather than becoming slow.
const (
	hintProbeOps = 50_000 // ~1% of the deterministic cap
	hintProbeMin = 10 * time.Millisecond
	hintProbeMax = 50 * time.Millisecond
)

// openHintBudget opens the allowance for one zero result: a hint may take
// about as long as the query it explains, floored so an instant query can
// still be explained and capped so a slow one cannot be doubled.
func (e *engine) openHintBudget() {
	// min(…, workLeft): a caller who asked for 1000ops asked for a cheap
	// answer, and spending 50× that to explain it would be answering a
	// question they declined to pay for.
	e.hintOpsLeft = min(hintProbeOps, e.workLeft)
	e.hintDeadline = time.Now().Add(
		min(max(time.Since(e.startedAt), hintProbeMin), hintProbeMax))
}

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

	lspCapReady bool
	probing     bool

	recursiveUnconfirmed  bool
	implementsUnavailable bool
}

// probe evaluates a relaxed selector on its own small budget.
//
// ok=false means INCONCLUSIVE — the probe ran out of ops, or reached edge
// work it is not allowed to do. It is not "no match", and no hint may be
// built on it: "nothing matches X" and "I stopped looking" are different
// claims, and only one of them is safe to tell a caller who is deciding
// whether the code exists.
func (e *engine) probe(list selectorList) (hit *treeNode, ok bool) {
	ops := min(e.hintOpsLeft, e.workLeft)
	if ops <= 0 || !time.Now().Before(e.hintDeadline) {
		return nil, false // the allowance is gone; a hint is not worth borrowing against
	}
	saved := e.probeSnapshot()
	// The probe's own world: the remaining hint allowance, throwaway
	// accumulators so nothing it counts can leak into what the caller is
	// told, and a SPENT LSP cap so no path — including one added later —
	// can turn a hint into a round-trip. lspCapReady must be forced too, or
	// ensureLSPCap would refill it.
	e.workLeft, e.workExceeded, e.timedOut, e.spendTick = ops, false, false, 0
	e.deadline = e.hintDeadline
	e.cap, e.capHit = 0, false
	e.costStack, e.hopCounted = nil, nil
	e.lspCapReady, e.lspLeft = true, 0
	e.probing, e.probeDegraded = true, false

	// need=1: stop at the first match. Every hint names ONE alternative,
	// so a second match would be work spent on something never said.
	rows, _ := e.evaluateCapped(list, 1)
	conclusive := !e.workExceeded && !e.probeDegraded
	e.hintOpsLeft -= min(ops, ops-e.workLeft) // never credit a negative overspend

	e.probeDegraded = false
	e.probeRestore(saved)

	if !conclusive {
		return nil, false
	}
	if len(rows) == 0 {
		return nil, true
	}
	return rows[0], true
	// elemCost is not restored: every probe runs on CLONED elements, so
	// its entries are keyed by pointers costTrace(list) never looks up.
}

// found is probe for the relaxation hints, which only ever act on a hit.
func (e *engine) probeFound(list selectorList) *treeNode {
	hit, _ := e.probe(list)
	return hit
}

func (e *engine) probeSnapshot() probeState {
	return probeState{
		workLeft: e.workLeft, workExceeded: e.workExceeded, timedOut: e.timedOut,
		spendTick: e.spendTick, deadline: e.deadline, cap: e.cap, capHit: e.capHit,
		costStack: e.costStack, blownElem: e.blownElem,
		lspLeft: e.lspLeft, lspAsked: e.lspAsked, lspResolved: e.lspResolved,
		maxHopReached: e.maxHopReached, unsettledFromHop: e.unsettledFromHop,
		transUnsettled: e.transUnsettled, hopCounted: e.hopCounted,
		lspCapReady: e.lspCapReady, probing: e.probing,
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
	e.lspCapReady, e.probing = s.lspCapReady, s.probing
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
		// The expression is a TREE the probes rewrite (hintDropLastAttr drops
		// a leaf); sharing it would edit the caller's own selector, which is
		// exactly what this function exists to prevent.
		c.attrExpr = cloneAttrExpr(c.attrExpr)
		out.elems[i].comp = &c
	}
	return out
}

// hasAttrOr reports whether an expression contains an OR anywhere.
func hasAttrOr(x *attrExpr) bool {
	if x == nil {
		return false
	}
	if x.op == attrExprOr {
		return true
	}
	for _, k := range x.kids {
		if hasAttrOr(k) {
			return true
		}
	}
	return false
}

// cloneAttrExpr deep-copies an attribute expression tree.
func cloneAttrExpr(x *attrExpr) *attrExpr {
	if x == nil {
		return nil
	}
	out := *x
	if len(x.kids) > 0 {
		out.kids = make([]*attrExpr, len(x.kids))
		for i, k := range x.kids {
			out.kids[i] = cloneAttrExpr(k)
		}
	}
	return &out
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

// ---------------------------------------------------- probe: dead chain prefix

// hintDeadPrefix finds the SHORTEST prefix of the chain that already
// matches nothing. Everything after it is irrelevant: a chain filters
// progressively, so once a position is empty no later clause can put
// anything back.
//
// This is the probe that matters most for edges, and the one the caller
// is least equipped to see. `#inputStream::out > method` returns ∅ whether
// inputStream has no method callees or inputStream DOES NOT EXIST — and
// the tool's own recipes teach "0 matches = unused", so a dead anchor
// reads as a fact about the code rather than a typo in the question.
//
// Sound only for a top-level chain (relTop): a relative list can carry
// bare position claims, whose truth is judged at a chain position rather
// than filtered through it.
func (e *engine) hintDeadPrefix(cx selComplex) string {
	if cx.rel != relTop || len(cx.elems) < 2 {
		return ""
	}
	for k := 1; k < len(cx.elems); k++ {
		// A prefix shares the caller's compounds — nothing here mutates
		// them, so there is no need to clone.
		pre := selComplex{elems: cx.elems[:k], rel: cx.rel}
		hit, ok := e.probe(selectorList{pre})
		if !ok {
			return "" // inconclusive: say nothing rather than blame this element
		}
		if hit != nil {
			continue // this much of the chain is alive; look further right
		}
		culprit := renderElem(&cx.elems[k-1])
		if k == 1 {
			return fmt.Sprintf("%s matches nothing, so the rest of the chain never ran — "+
				"an empty result here says nothing about what follows it. Fix that element first",
				culprit)
		}
		return fmt.Sprintf("the chain is already empty at %s (element %d of %d), so nothing "+
			"after it could match. Fix that element first", culprit, k, len(cx.elems))
	}
	return ""
}

// ------------------------------------------------------- probe: drop the tag

// hintDropTag re-runs the selector with the subject's TAG removed. This
// is the `method name~=newInputStream` case outright: the filters are
// right, the tag is wrong, and the caller had to know which of func /
// method / field a symbol is BEFORE it could ask.
//
// An edge subject has the same failure in its own spelling — the KIND
// class. `#X::in.call` and `#X::in.type` are the same guess-before-you-ask
// about how a name is USED, and a wrong guess returns the same ∅ as no
// edges at all.
//
// Reporting the found node's real tag and address makes the answer
// actionable in one step — the address feeds straight into node_read.
func (e *engine) hintDropTag(cx selComplex) string {
	last := len(cx.elems) - 1
	sub := cx.elems[last].comp

	if sub.isRef {
		if len(sub.refClasses) == 0 {
			return "" // a bare ::in/::out has no kind to drop
		}
		relaxed := cloneComplex(cx)
		c := relaxed.elems[last].comp
		c.refClasses = nil
		hit := e.probeFound(selectorList{relaxed})
		if hit == nil {
			return ""
		}
		return fmt.Sprintf("no ::%s%s edge here — the KIND class is what emptied it, not the "+
			"anchor. Without it there is a %s edge at %s. Retry with that kind, or with a bare ::%s",
			sub.refDir, refClassSuffix(sub.refClasses), refTypeLabel(hit), hit.addr(), sub.refDir)
	}

	if sub.isGenerated() || sub.anyType || sub.class == "" {
		return "" // nothing written to drop
	}
	relaxed := cloneComplex(cx)
	c := relaxed.elems[last].comp
	c.class, c.anyType, c.implied = "", true, false
	hit := e.probeFound(selectorList{relaxed})
	if hit == nil {
		return ""
	}
	return fmt.Sprintf("no %s matches — the TAG is what emptied it, not the filters. "+
		"Same query without it matches %s #'%s'. Retry with that tag, or with `*`",
		sub.class, hit.class, hit.addr())
}

// refClassSuffix spells a ref compound's kind classes back (".call").
func refClassSuffix(classes []string) string {
	var b strings.Builder
	for _, c := range classes {
		b.WriteString("." + c)
	}
	return b.String()
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
	if idx == nil || !time.Now().Before(e.hintDeadline) {
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
		near := nearestName(a.value, nameNeighbours(idx.Names(), a.value))
		if near == "" || near == a.value {
			continue
		}
		// Through probe(), so the tree lookup draws on the same ops and
		// clock allowance as every other probe. Doing it directly (via
		// declsNamed) parses every file the name occurs in, unbudgeted —
		// measured at 67 ms on this repo, against a 10 ms allowance.
		hit := e.probeFound(selectorList{nameSelector(near)})
		if hit == nil {
			continue // an occurrence, not a declaration — nothing to retry with
		}
		return fmt.Sprintf("nothing is named %q — the nearest thing that IS declared is "+
			"%s #'%s'. Retry with %q", a.value, hit.class, hit.addr(), near)
	}
	return ""
}

// nameNeighbours narrows the candidates nearestName scores. A near miss is
// a typo or a case slip, and neither changes a name's length by much — so
// the length filter costs nothing in recall and takes the edit-distance
// scan off the full name list (5,043 names on this repo).
func nameNeighbours(names []string, want string) []string {
	out := make([]string, 0, len(names)/2)
	for _, c := range names {
		if d := len(c) - len(want); d <= nameNeighbourSlack && d >= -nameNeighbourSlack {
			out = append(out, c)
		}
	}
	return out
}

const nameNeighbourSlack = 3

// nameSelector builds `[name=<n>]` directly, without going through the
// parser — the value is a workspace name, not caller text, so quoting it
// into a selector string only to parse it back would be a round trip
// through an escaping problem that need not exist.
func nameSelector(n string) selComplex {
	comp := &selCompound{
		anyType: true, implied: true,
		attrs: []selAttr{{axis: attrName, op: selExact, value: n}},
	}
	return selComplex{rel: relTop, elems: []selElem{{comp: comp, min: 1, max: 1}}}
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
	// Only a pure AND can have "one clause" dropped: under an OR no single
	// leaf is what emptied the result, and relaxing one would describe a
	// query the caller never wrote.
	if hasAttrOr(cx.elems[elem].comp.attrExpr) {
		return ""
	}
	dropped := cx.elems[elem].comp.attrs[attr]
	relaxed := cloneComplex(cx)
	c := relaxed.elems[elem].comp
	c.attrs = append(c.attrs[:attr], c.attrs[attr+1:]...)
	c.setAttrsAsAnd(c.attrs)
	hit := e.probeFound(selectorList{relaxed})
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

// ------------------------------------------- inert containment

// inertContainmentNote names a clause that CANNOT contribute, and says what
// the caller probably meant instead.
//
// The shape: two elements joined by CONTAINMENT, each pinned to a different
// exact path. Containment is the file tree — project > dir > file > symbols —
// so nothing at path B is ever inside something at path A unless A is a
// directory above B. `path=a.go path=b.go` is empty for ANY depth and any
// {m,n}; no tuning makes it work.
//
// It is worth its own check because the failure is not always visible as
// emptiness. `path=a.go *{0,3} path=b.go` returns a confident 129: a bare
// attribute after a space attaches to the element before it (deliberate — see
// parseComplex), so that reads as `*[path=b.go]{0,3}`, and {0,…} lets the
// whole clause VANISH. The prefix alone answers, and nothing says the rest of
// the selector did no work. Naming it is the difference between a wrong answer
// and a corrected one.
//
// Reported for any result count, unlike zeroResultHint — a non-empty wrong
// answer is the more dangerous of the two.
func inertContainmentNote(list selectorList) string {
	for _, cx := range list {
		// One compound, two different exact paths. This is what plain
		// `path=a.go path=b.go` actually parses to — the bare attribute after
		// a space attaches to the element before it rather than starting a
		// new one — so it is not a containment pair at all but a single node
		// required to live in two files at once.
		for i := range cx.elems {
			if a, b, ok := conflictingPaths(cx.elems[i].comp); ok {
				return fmt.Sprintf(
					"`path=%s` and `path=%s` are attached to the SAME element, so this asks for one "+
						"node living in two files — always empty. A bare `path=` after a space FILTERS "+
						"the element before it instead of descending. To link two files you want the "+
						"reference GRAPH: path=%s ::out.call{1,3} > path=%s",
					a, b, a, b)
			}
		}
		for i := 1; i < len(cx.elems); i++ {
			el, prev := &cx.elems[i], &cx.elems[i-1]
			if el.comb != selDescendant && el.comb != selChild {
				continue // only containment; edges legitimately cross files
			}
			outer, ok := exactPath(prev.comp)
			if !ok {
				continue
			}
			inner, ok := exactPath(el.comp)
			if !ok || pathCanContain(outer, inner) {
				continue
			}
			clause := baseClause(el.comp) + "[path=" + inner + "]"
			why := fmt.Sprintf(
				"`%s` can never match: it is joined to `path=%s` by CONTAINMENT, "+
					"which is the file tree — nothing at %s is inside %s, at any depth or {m,n}",
				clause, outer, inner, outer)
			if el.min == 0 {
				why += ". Every result here came from the SKIP path ({0,…} lets the clause vanish), " +
					"so the answer is just the part before it"
			}
			return why + fmt.Sprintf(
				". To link two files you want the reference GRAPH, not containment: "+
					"path=%s ::out.call{1,3} > path=%s (::in for the reverse, drop .call for every edge kind)",
				outer, inner)
		}
	}
	return ""
}

// exactPath returns the compound's `path=` value when it is pinned to one
// literal path. Patterns (^= *= ~=) are left alone: they can match across
// directories, so no static claim about containment is safe.
func exactPath(c *selCompound) (string, bool) {
	if c == nil {
		return "", false
	}
	for _, a := range c.attrs {
		if a.axis == attrPath && a.op == selExact {
			return a.value, true
		}
	}
	return "", false
}

// pathCanContain reports whether something at path outer could contain
// something at path inner. Equal paths qualify: a file and its symbols share
// the file's path, so `path=a.go path=a.go` is satisfiable and must not be
// reported. Otherwise outer has to be a directory above inner.
func pathCanContain(outer, inner string) bool {
	return outer == inner || strings.HasPrefix(inner, strings.TrimSuffix(outer, "/")+"/")
}

// conflictingPaths returns two exact `path=` values on the same compound when
// they cannot both hold. A node has one path, so two different literals is a
// contradiction — unless one is a directory above the other, which never
// happens on a single node either (a node's path is one string, so even
// "mcp" and "mcp/tools.go" together are unsatisfiable). Patterns are skipped:
// only exact literals support a static claim.
func conflictingPaths(c *selCompound) (string, string, bool) {
	if c == nil {
		return "", "", false
	}
	seen := ""
	for _, a := range c.attrs {
		if a.axis != attrPath || a.op != selExact {
			continue
		}
		if seen == "" {
			seen = a.value
			continue
		}
		if seen != a.value {
			return seen, a.value, true
		}
	}
	return "", "", false
}
