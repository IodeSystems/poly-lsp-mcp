package mcp

import (
	"encoding/json"
	"fmt"
	"github.com/iodesystems/poly-lsp-mcp/internal/git"
	"os"
	"sort"
	"strings"

	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// Merge-conflict awareness.
//
// A conflicted file is the one input where the index is confidently wrong.
// tree-sitter's error recovery walks straight past the markers and produces a
// symbol table in which BOTH sides coexist as peers — measured on a two-line
// conflict, `func Ours` and `func Theirs` both indexed as ordinary top-level
// funcs, at adjacent lines, with nothing marking either. So node_query answers
// about a version of the file that exists in no commit and on no branch, and
// node_edit will happily rewrite a declaration that is half of an unresolved
// choice.
//
// The markers are therefore parsed BEFORE the tree, not derived from it: by
// the time tree-sitter is done the two sides have been flattened into one
// namespace and the information is gone.
const (
	conflictOursMarker  = "<<<<<<<"
	conflictBaseMarker  = "|||||||"
	conflictSplitMarker = "======="
	conflictEndMarker   = ">>>>>>>"
)

// conflictSide is one alternative in a conflict: its lines, the span they
// occupy in the FILE (1-based, inclusive), and the label git wrote on the
// marker — a ref name, a commit hash, or "HEAD".
type conflictSide struct {
	label string
	lines []string
	at    [2]int // 0,0 when the side is empty
}

func (s conflictSide) text() string { return strings.Join(s.lines, "\n") }

// conflictRegion is one `<<<<<<< … ======= … >>>>>>>` block, spanning from
// the opening marker line to the closing one so that replacing the span
// resolves the conflict markers and all.
//
// base is the common ancestor, present only under diff3-style output
// (merge.conflictStyle=diff3), which inserts `||||||| <base>` between the
// sides. It is kept because it is what makes a three-way choice possible;
// without it "accept theirs" is the only honest option a tool can offer
// beyond hand-editing.
type conflictRegion struct {
	at     [2]int
	ours   conflictSide
	theirs conflictSide
	base   *conflictSide
}

// parseConflicts finds every conflict block in a file.
//
// Deliberately textual and forgiving: it keys on the 7-character marker
// prefixes at line start, exactly as git writes them, and ignores anything it
// cannot pair. A stray `=======` in prose (a Markdown rule, an ASCII table) is
// not a conflict unless an opener preceded it, and an opener with no closer is
// dropped rather than swallowing the rest of the file — the failure mode to
// avoid is claiming a conflict that is not there, which would make a clean
// file unreadable.
func parseConflicts(content []byte) []conflictRegion {
	if !strings.Contains(string(content), conflictOursMarker) {
		return nil // the overwhelmingly common case, at one scan
	}
	lines := splitNodeReadLines(content)
	var out []conflictRegion

	for i := 0; i < len(lines); i++ {
		if !isConflictMarker(lines[i], conflictOursMarker) {
			continue
		}
		start := i
		ours := conflictSide{label: markerLabel(lines[i], conflictOursMarker)}
		var base *conflictSide
		var theirs conflictSide
		cur := &ours
		curStart := i + 1
		end := -1

		for j := i + 1; j < len(lines); j++ {
			switch {
			case isConflictMarker(lines[j], conflictOursMarker):
				// A nested opener means the block never closed; abandon this
				// one and let the outer loop restart at the new opener.
				j = len(lines)
			case isConflictMarker(lines[j], conflictBaseMarker):
				closeSide(cur, lines, curStart, j)
				b := conflictSide{label: markerLabel(lines[j], conflictBaseMarker)}
				base, cur, curStart = &b, &b, j+1
			case isConflictMarker(lines[j], conflictSplitMarker):
				closeSide(cur, lines, curStart, j)
				theirs.label = ""
				cur, curStart = &theirs, j+1
			case isConflictMarker(lines[j], conflictEndMarker):
				closeSide(cur, lines, curStart, j)
				theirs.label = markerLabel(lines[j], conflictEndMarker)
				end = j
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			continue // unterminated: not a conflict we can act on
		}
		out = append(out, conflictRegion{
			at:     [2]int{start + 1, end + 1},
			ours:   ours,
			theirs: theirs,
			base:   base,
		})
		i = end
	}
	return out
}

// closeSide records the lines a side occupied, from `from` up to (not
// including) the marker line at `to`. Both indices are 0-based.
func closeSide(s *conflictSide, lines []string, from, to int) {
	if to <= from {
		return // an empty side — one alternative deletes what the other adds
	}
	s.lines = lines[from:to]
	s.at = [2]int{from + 1, to}
}

// isConflictMarker reports whether a line opens/splits/closes a conflict.
// Git writes exactly seven marker characters followed by end-of-line or a
// space and a label; requiring that boundary keeps `========` (a prose rule)
// and `>>>>>>>>` from reading as markers.
func isConflictMarker(line, marker string) bool {
	if !strings.HasPrefix(line, marker) {
		return false
	}
	rest := line[len(marker):]
	return rest == "" || strings.HasPrefix(rest, " ")
}

// markerLabel is the ref/hash git wrote after the marker: "HEAD",
// "feature/x", or "e0cfa59 (feat(worktree): …)".
func markerLabel(line, marker string) string {
	return strings.TrimSpace(strings.TrimPrefix(line, marker))
}

// ---------------------------------------------------------- resolution

// applyConflictAccept resolves every conflict inside the addressed node by
// keeping one side and dropping the markers.
//
// The node can be the whole FILE ("resolve this file the same way
// throughout") or any narrower span, which is what makes a per-region
// decision possible: conflicts rarely all go the same way, and a tool that
// could only do the whole file would be used once and then abandoned for a
// text editor.
//
// Regions are rewritten LAST-FIRST so that each replacement cannot move the
// spans of the ones not yet applied.
func (s *Server) applyConflictAccept(rn *modernNode, side string, opts diagnosticOptions) ([]Content, bool, error) {
	want, err := normalizeConflictSide(side)
	if err != nil {
		return nil, true, err
	}
	abs, err := s.resolveFileArg(rn.file)
	if err != nil {
		return nil, true, err
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", rn.file, err)
	}
	all := parseConflicts(content)
	if len(all) == 0 {
		return nil, true, fmt.Errorf("%s has no merge conflict to accept — it holds no <<<<<<< markers", rn.file)
	}
	lo, hi := 1, len(splitNodeReadLines(content))
	if !rn.wholeFile() {
		lo, hi = rn.decl.StartLine, rn.decl.EndLine
	}
	var in []conflictRegion
	for _, c := range all {
		if c.at[0] >= lo && c.at[1] <= hi {
			in = append(in, c)
		}
	}
	if len(in) == 0 {
		return nil, true, fmt.Errorf(
			"no conflict lies inside %s (lines %d-%d); the file's conflicts are at %s — address one of those, or the file",
			rn.addr, lo, hi, conflictSpans(all))
	}

	lines := splitNodeReadLines(content)
	for i := len(in) - 1; i >= 0; i-- {
		c := in[i]
		keep := c.ours
		if want == "theirs" {
			keep = c.theirs
		}
		// c.at is 1-based inclusive over marker lines; replace the whole
		// block, which is what drops the markers with it.
		tail := append([]string{}, lines[c.at[1]:]...)
		lines = append(lines[:c.at[0]-1], append(append([]string{}, keep.lines...), tail...)...)
	}
	out := strings.Join(lines, "\n")
	if endsWithNewline(content) && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	c, isErr, err := s.applyWholeFileWrite(rn.file, out, opts)
	if isErr || err != nil {
		return c, isErr, err
	}
	// Resolving the TEXT does not resolve the MERGE: git still has the
	// conflict staged, so `git status` keeps calling the file unmerged and
	// the merge stays blocked. Staging is not this tool's call to make —
	// it is outward-facing git state — but finishing silently would leave a
	// caller believing the job is done. Say what is left.
	left := len(all) - len(in)
	return withNote(c, resolvedNote(rn.file, len(in), left)), false, nil
}

// resolvedNote states what was resolved and what the caller still owes git.
func resolvedNote(file string, done, left int) string {
	n := fmt.Sprintf("resolved %d conflict(s) in %s", done, file)
	if left > 0 {
		n += fmt.Sprintf("; %d still unresolved in this file", left)
		return n
	}
	return n + " — the FILE is clean but git still has it staged as unmerged; `git add " + file + "` to finish the merge"
}

// withNote folds a note into an already-rendered JSON tool result.
func withNote(c []Content, note string) []Content {
	if len(c) == 0 {
		return c
	}
	var m map[string]any
	if json.Unmarshal([]byte(c[0].Text), &m) != nil {
		return c
	}
	if prev, ok := m["note"].(string); ok && prev != "" {
		m["note"] = note + ". " + prev
	} else {
		m["note"] = note
	}
	return jsonContent(m)
}

// normalizeConflictSide maps the spellings a caller reaches for onto the two
// real answers. "mine"/"ours" are the same side; both are accepted because
// git says "ours" and humans say "mine".
func normalizeConflictSide(side string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "mine", "ours", "head":
		return "ours", nil
	case "theirs", "incoming":
		return "theirs", nil
	}
	return "", fmt.Errorf("accept must be \"mine\" (a.k.a. ours) or \"theirs\"; got %q", side)
}

// conflictSpans lists a file's conflicts as addresses, so the error that says
// "not in range" also says what IS in range.
func conflictSpans(cs []conflictRegion) string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, fmt.Sprintf("@%d-%d", c.at[0], c.at[1]))
	}
	return strings.Join(out, ", ")
}

// ------------------------------------------------- conflict as nodes

// parseConflictElement parses ::conflict / ::mine / ::theirs / ::base.
//
// Like ::comment and ::body these take no kind, id or attribute — they ARE
// the region and its sides — but they accept trailing filter pseudos, so
// `::theirs:contains('panic')` works.
func (p *modSelParser) parseConflictElement(name string) (selCompound, error) {
	comp := selCompound{isConflict: true, class: name}
	if name != "conflict" {
		comp.conflictSide = name
	}
	for {
		switch p.peek() {
		case ':':
			if p.peekIsPseudoElement() {
				return comp, nil // chained: ::mine::grep is a new element
			}
			if err := p.parsePseudo(&comp); err != nil {
				return comp, err
			}
		case '.', '#', '[':
			return comp, fmt.Errorf("::%s has no kind, id or attr — it IS the conflict %s; filter it with :contains('…')",
				name, map[bool]string{true: "region", false: "side"}[name == "conflict"])
		default:
			return comp, nil
		}
	}
}

// conflictMatches materializes conflict nodes.
//
// Two shapes, keyed by what was asked for:
//
//   - `::conflict` on a FILE yields ONE NODE PER REGION — a file can have
//     many, and collapsing them would make per-region resolution
//     unaddressable, which is the thing that matters most (conflicts rarely
//     all go the same way).
//   - `::mine` / `::theirs` / `::base` yield that side. Written against a
//     conflict node they read it directly; written against a FILE they imply
//     the region, so `path=f.go ::theirs` is every incoming side in the file
//     without naming each block first.
func (e *engine) conflictMatches(hosts map[*treeNode]bool, comp *selCompound, relaxed bool) map[*treeNode]bool {
	out := map[*treeNode]bool{}
	for _, h := range ordered(hosts) {
		for _, g := range e.conflictNodesOf(h, comp.conflictSide) {
			for n := range e.selectOrdered(map[*treeNode]bool{g: true}, comp, relaxed) {
				out[n] = true
			}
		}
	}
	return out
}

// conflictNodesOf returns the conflict nodes a host yields. side == "" asks
// for the regions themselves.
func (e *engine) conflictNodesOf(h *treeNode, side string) []*treeNode {
	if h.class == "conflict" {
		if side == "" || h.conflict == nil {
			return nil // a region has no nested regions
		}
		if s := sideOf(h.conflict, side); s != nil {
			return []*treeNode{e.conflictSideNode(h, side, *s)}
		}
		return nil
	}
	if h.class != "file" {
		return nil
	}
	content, err := os.ReadFile(h.abs)
	if err != nil {
		return nil
	}
	regions := parseConflicts(content)
	out := make([]*treeNode, 0, len(regions))
	for i := range regions {
		r := regions[i]
		region := &treeNode{
			class: "conflict",
			file:  h.file, abs: h.abs, at: r.at,
			parent: h, depth: h.depth + 1,
			fileOrd: h.fileOrd, symOrd: h.symOrd,
			genText:  conflictText(content, r),
			conflict: &r,
		}
		if side == "" {
			out = append(out, region)
			continue
		}
		if s := sideOf(&r, side); s != nil {
			out = append(out, e.conflictSideNode(region, side, *s))
		}
	}
	return out
}

// conflictSideNode builds the node for one side of a region.
//
// An EMPTY side (one alternative deletes what the other adds) still gets a
// node, spanning the marker it sits between. "this side is empty" is a real
// answer to "what does theirs say here", and returning nothing instead would
// read as "there is no conflict".
func (e *engine) conflictSideNode(region *treeNode, side string, s conflictSide) *treeNode {
	at := s.at
	if at[0] == 0 { // empty side: pin it inside the region so the span is valid
		at = [2]int{region.at[0], region.at[0]}
	}
	return &treeNode{
		class: side,
		file:  region.file, abs: region.abs, at: at,
		parent: region, depth: region.depth + 1,
		fileOrd: region.fileOrd, symOrd: region.symOrd,
		genText:       s.text(),
		conflictLabel: s.label,
	}
}

// sideOf maps a selector name onto the region's side. "mine" is git's "ours".
func sideOf(r *conflictRegion, side string) *conflictSide {
	switch side {
	case "mine":
		return &r.ours
	case "theirs":
		return &r.theirs
	case "base":
		return r.base // nil unless diff3 wrote one
	}
	return nil
}

// conflictText is the region's source, markers included, so a ::conflict node
// reads back byte-for-byte what node_edit would replace.
func conflictText(content []byte, r conflictRegion) string {
	lines := splitNodeReadLines(content)
	if r.at[0] < 1 || r.at[1] > len(lines) {
		return ""
	}
	return strings.Join(lines[r.at[0]-1:r.at[1]], "\n")
}

// ------------------------------------------------- chimera protection

// conflictOverlap returns the conflict regions an arbitrary span straddles.
//
// This is the sharp edge of an unresolved merge. tree-sitter recovers past
// the markers, so it happily builds symbols out of BOTH sides at once:
// measured on a conflict opening in one function and closing in another, the
// index reported `func B[1]` spanning ours' header, a `=======` marker, and
// theirs' tail — a declaration from no commit and no branch, and not valid
// source in any of them.
//
// Reading one is merely misleading. EDITING one writes across a marker and
// corrupts the merge itself, which is why node_edit refuses rather than
// warns: there is no correct oldText for a span that is half of each side.
func conflictOverlap(content []byte, from, to int) []conflictRegion {
	var out []conflictRegion
	for _, c := range parseConflicts(content) {
		// Straddling, not containing: a node that WRAPS whole conflicts
		// (a function holding a conflicted statement, or the file) is a
		// legitimate target — accept: and a whole-node rewrite both work on
		// it. What cannot be edited is a span that starts or ends INSIDE a
		// region, because that span is part-ours-part-theirs.
		startsInside := from > c.at[0] && from <= c.at[1]
		endsInside := to >= c.at[0] && to < c.at[1]
		if startsInside || endsInside {
			out = append(out, c)
		}
	}
	return out
}

// conflictChimeraErr explains a refused edit and names every way forward.
func conflictChimeraErr(file, addr string, from, to int, cs []conflictRegion) error {
	return fmt.Errorf(
		"%s (lines %d-%d) straddles an unresolved merge conflict at %s, so its text is part MINE and part THEIRS — "+
			"a declaration that exists on neither side. Editing it would write across the markers and corrupt the merge. "+
			"Resolve first: node_edit(node:%q, accept:\"mine\"|\"theirs\"), or read the versions apart with ::mine / ::theirs",
		addr, from, to, conflictSpans(cs), conflictAddr(file, cs[0]))
}

// conflictAddr is a region's span address, file included so the suggestion
// is runnable as printed.
func conflictAddr(file string, c conflictRegion) string {
	return fmt.Sprintf("%s@%d-%d", file, c.at[0], c.at[1])
}

// ------------------------------------------------- two-version reconstruction

// sideContent rebuilds the WHOLE FILE as it would read if `side` won every
// conflict in it.
//
// Whole file, not the region's lines: a side taken alone is usually a
// syntactic fragment — a conflict that opens in one function and closes in
// another leaves each side holding half a declaration — while the file WITH
// that side is exactly what git would write on `--ours`/`--theirs`, and
// parses. That is the difference between a version and a snippet.
//
// Every region resolves the same way, because a partial reconstruction is
// not a version of anything: it is a fourth file, which is the problem being
// fixed rather than a new flavour of it.
func sideContent(content []byte, side string) []byte {
	regions := parseConflicts(content)
	if len(regions) == 0 {
		return content
	}
	lines := splitNodeReadLines(content)
	for i := len(regions) - 1; i >= 0; i-- {
		r := regions[i]
		keep := r.ours
		switch side {
		case "theirs":
			keep = r.theirs
		case "base":
			if r.base == nil {
				keep = conflictSide{} // no ancestor recorded: the side is empty
			} else {
				keep = *r.base
			}
		}
		tail := append([]string{}, lines[r.at[1]:]...)
		lines = append(lines[:r.at[0]-1], append(append([]string{}, keep.lines...), tail...)...)
	}
	out := strings.Join(lines, "\n")
	if endsWithNewline(content) && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return []byte(out)
}

// sideParse is one reconstructed version and whether it can be trusted as a
// parse tree.
type sideParse struct {
	content []byte
	clean   bool
}

// reconstructSides builds both versions and reports whether each parses.
// BEST EFFORT: a side that parses is usable even if the other does not, which
// is the common case when one branch is mid-edit.
func (s *Server) reconstructSides(file string, content []byte) (ours, theirs sideParse) {
	lang := s.languageForFile(file)
	ours.content = sideContent(content, "mine")
	theirs.content = sideContent(content, "theirs")
	ours.clean = symbols.ParsesCleanly(lang, ours.content)
	theirs.clean = symbols.ParsesCleanly(lang, theirs.content)
	return ours, theirs
}

// conflictView is what a caller is told about a region: the versions when
// they can be parsed, and a DIFF when they cannot.
type conflictView struct {
	MineParses   bool   `json:"mineParses"`
	TheirsParses bool   `json:"theirsParses"`
	Diff         string `json:"diff,omitempty"`
	Note         string `json:"note,omitempty"`
}

// viewOf decides how to present a conflict.
//
// When at least one side reconstructs into parseable source, the structural
// view is meaningful and the caller gets it. When NEITHER does — a conflict
// inside a string literal, a half-written branch, a language with no grammar
// — there is no honest tree to show, and a structural answer would be
// invented. Fall back to what is always true: the two texts, and their
// difference.
func (s *Server) viewOf(file string, content []byte, r conflictRegion) conflictView {
	ours, theirs := s.reconstructSides(file, content)
	v := conflictView{MineParses: ours.clean, TheirsParses: theirs.clean}
	if ours.clean || theirs.clean {
		if !ours.clean {
			v.Note = "mine does not parse — treat its symbols as unreliable"
		} else if !theirs.clean {
			v.Note = "theirs does not parse — treat its symbols as unreliable"
		}
		return v
	}
	v.Diff = lineDiff(r.ours.lines, r.theirs.lines)
	v.Note = "neither side reconstructs into parseable source, so this is shown as TEXT: no symbol view of this conflict would be real"
	return v
}

// lineDiff renders a minimal line diff of two blocks: "-" is mine, "+" is
// theirs, " " is common. Longest-common-subsequence, so unchanged lines stay
// unchanged instead of the whole block reading as replaced.
func lineDiff(mine, theirs []string) string {
	n, m := len(mine), len(theirs)
	// lcs[i][j] = length of the LCS of mine[i:] and theirs[j:]
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if mine[i] == theirs[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var b strings.Builder
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case mine[i] == theirs[j]:
			fmt.Fprintf(&b, "  %s\n", mine[i])
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			fmt.Fprintf(&b, "- %s\n", mine[i])
			i++
		default:
			fmt.Fprintf(&b, "+ %s\n", theirs[j])
			j++
		}
	}
	for ; i < n; i++ {
		fmt.Fprintf(&b, "- %s\n", mine[i])
	}
	for ; j < m; j++ {
		fmt.Fprintf(&b, "+ %s\n", theirs[j])
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// ------------------------------------------------- warning on read/query

// maxNamedConflictFiles bounds the names in a warning. A note that lists
// twenty files is a result set wearing a warning's clothes, and callers learn
// to skip it.
const maxNamedConflictFiles = 3

// conflictWarning reports that returned rows come from files with unresolved
// conflicts.
//
// It no longer names straddling rows: dropConflictChimeras removes them
// before this runs, and the withheld-count note explains that separately.
// What is left to warn about is subtler and still real — a declaration that
// merely CONTAINS a conflict is listed, is genuine, and has a different body
// depending on the side.
//
// Scoped to the files actually in the result rather than the workspace: it
// costs one read per distinct file in the page (bounded by limit), the scan
// short-circuits on a single Contains for the overwhelmingly common clean
// case, and — more importantly — a warning about a file nobody asked about is
// noise, while one about the symbols just handed back is the answer.
//
// This is the poll-shaped substitute for a server→client event. It arrives
// late (on the next query rather than when the conflict appears) but it needs
// no push channel, and it is right where the damage would otherwise be done:
// attached to the symbols a caller is about to act on.
func (s *Server) conflictWarning(rows []*treeNode) string {
	seen := map[string][]conflictRegion{}
	var files []string
	for _, n := range rows {
		if n.file == "" || n.abs == "" {
			continue
		}
		cs, ok := seen[n.file]
		if !ok {
			content, err := os.ReadFile(n.abs)
			if err != nil {
				seen[n.file] = nil
				continue
			}
			cs = parseConflicts(content)
			seen[n.file] = cs
			if len(cs) > 0 {
				files = append(files, n.file)
			}
		}
	}
	if len(files) == 0 {
		return ""
	}
	sort.Strings(files)
	named := files
	extra := ""
	if len(named) > maxNamedConflictFiles {
		named, extra = named[:maxNamedConflictFiles], fmt.Sprintf(" (+%d more)", len(files)-maxNamedConflictFiles)
	}
	msg := fmt.Sprintf(
		"UNRESOLVED merge conflict in %s%s — tree-sitter recovers past the markers, so symbols from these files can combine BOTH sides. "+
			"`#'%s'::conflict` shows the regions; node_edit accept:\"mine\"|\"theirs\" resolves them",
		strings.Join(named, ", "), extra, files[0])
	return msg
}

// straddlesAny reports whether a span starts or ends inside any region.
func straddlesAny(cs []conflictRegion, from, to int) bool {
	for _, c := range cs {
		if (from > c.at[0] && from <= c.at[1]) || (to >= c.at[0] && to < c.at[1]) {
			return true
		}
	}
	return false
}

// ------------------------------------------------- unsolicited notice

// noteConflictTransition tells the client when a watched file GAINS or LOSES
// conflict markers.
//
// On the transition only. A conflicted file is saved many times while it is
// being resolved, and re-announcing on every write would train a reader to
// ignore the one message that mattered — the first.
//
// Fired from the watcher goroutine; Server.send takes writeMu, so this cannot
// interleave with a tool response on the same stdout.
func (s *Server) noteConflictTransition(path string, content []byte) {
	s.rootMu.RLock()
	root := s.root
	s.rootMu.RUnlock()
	rel := relPath(path, root)
	now := len(parseConflicts(content)) > 0

	s.conflictMu.Lock()
	if s.conflicted == nil {
		s.conflicted = map[string]bool{}
	}
	before := s.conflicted[rel]
	if before == now {
		s.conflictMu.Unlock()
		return
	}
	if now {
		s.conflicted[rel] = true
	} else {
		delete(s.conflicted, rel)
	}
	s.conflictMu.Unlock()

	msg := fmt.Sprintf("merge conflict RESOLVED in %s", rel)
	level := "info"
	if now {
		level = "warning"
		msg = fmt.Sprintf(
			"UNRESOLVED merge conflict appeared in %s — symbols there may combine BOTH sides. "+
				"`#'%s'::conflict` to see the regions; node_edit accept:\"mine\"|\"theirs\" to resolve",
			rel, rel)
	}
	s.sendNotification("notifications/message", map[string]any{
		"level":  level,
		"logger": "poly-lsp-mcp/conflict",
		"data":   msg,
	})
}

// sideSymbols lists what a side DECLARES, from its whole-file
// reconstruction.
//
// Names and classes only, deliberately no addresses. A symbol found in the
// reconstruction has a span in RECONSTRUCTED coordinates, and the file on
// disk still has markers in it — so an address like "f.go#Greet" minted here
// would resolve against different bytes than the ones it was computed from.
// That is the silent-wrong-address failure this codebase has spent a lot of
// effort removing; a name is the most that can be said honestly.
//
// Returns nil when the reconstruction does not parse: symbols recovered from
// broken source are the chimeras all over again.
func (s *Server) sideSymbols(file string, content []byte, side string) []map[string]any {
	lang := s.languageForFile(file)
	rebuilt := sideContent(content, side)
	if !symbols.ParsesCleanly(lang, rebuilt) {
		return nil
	}
	syms, err := symbols.FileSymbols(lang, rebuilt)
	if err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(syms))
	for _, sym := range syms {
		if outlineSkipClasses[sym.Class] {
			continue // signature detail, not content — same rule as the outline
		}
		out = append(out, map[string]any{"sym": sym.Sym, "class": sym.Class})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// dropConflictChimeras removes rows whose span STRADDLES a conflict marker.
//
// Those declarations exist on neither side — tree-sitter stitched them out of
// both — so listing them is not a partial answer, it is a wrong one. A symbol
// wholly INSIDE a region is left alone: it is real on one side, and which one
// is what ::mine/::theirs answer.
//
// Removing them also repairs the names of the real symbols around them.
// Ordinal disambiguation counts what the index holds, so a phantom taking
// `B[1]` pushed the genuine function to `B[2]`; with the phantom gone the
// ordinals are recomputed over real declarations only.
func (s *Server) dropConflictChimeras(rows []*treeNode) ([]*treeNode, int, string) {
	byFile := map[string][]conflictRegion{}
	out := rows[:0:0]
	dropped := 0
	var where string
	for _, n := range rows {
		if n.abs == "" || n.class == "conflict" || n.class == "mine" ||
			n.class == "theirs" || n.class == "base" {
			out = append(out, n)
			continue
		}
		cs, ok := byFile[n.abs]
		if !ok {
			content, err := os.ReadFile(n.abs)
			if err == nil {
				cs = parseConflicts(content)
			}
			byFile[n.abs] = cs
		}
		if len(cs) > 0 && straddlesAny(cs, n.at[0], n.at[1]) {
			dropped++
			if where == "" {
				where = n.file
			}
			continue
		}
		out = append(out, n)
	}
	return out, dropped, where
}

// seedConflicted asks git which files are UNMERGED and records them.
//
// The set was previously filled only by the watcher, so it knew about a
// conflict that APPEARED while the server ran and nothing about one that was
// already there — which is the ordinary case, since the agent is started after
// the merge that needs untangling. Everything keyed off the set was therefore
// silent exactly when a human would most expect it to speak.
func (s *Server) seedConflicted(root string) {
	repo, err := git.FromCWD(root)
	if err != nil {
		return
	}
	paths := repo.UnmergedPaths()
	s.conflictMu.Lock()
	defer s.conflictMu.Unlock()
	if s.conflicted == nil {
		s.conflicted = map[string]bool{}
	}
	for _, p := range paths {
		s.conflicted[p] = true
	}
	if len(paths) > 0 {
		s.logf("index: %d file(s) hold unresolved merge conflicts: %s",
			len(paths), strings.Join(paths, ", "))
	}
}

// conflictedFiles returns the unmerged paths known right now.
func (s *Server) conflictedFiles() []string {
	s.conflictMu.Lock()
	defer s.conflictMu.Unlock()
	out := make([]string, 0, len(s.conflicted))
	for f, yes := range s.conflicted {
		if yes {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// zeroResultConflictNote warns that "not found" is not trustworthy while a
// conflict is open.
//
// tree-sitter recovers past conflict markers by SWALLOWING what follows: in a
// file conflicted at one function, the declaration above the markers absorbed
// every declaration below it, so those symbols were not withheld — they were
// never parsed. A caller asking for one of them got totalMatches:0 and no
// note, and "0" reads as "that code does not exist" rather than "the file it
// lives in is mid-merge".
func (s *Server) zeroResultConflictNote() string {
	files := s.conflictedFiles()
	if len(files) == 0 {
		return ""
	}
	shown := files
	if len(shown) > 5 {
		shown = shown[:5]
	}
	more := ""
	if len(files) > len(shown) {
		more = fmt.Sprintf(" (and %d more)", len(files)-len(shown))
	}
	subject := fmt.Sprintf("%s holds an UNRESOLVED merge conflict", shown[0])
	if len(shown) > 1 {
		subject = fmt.Sprintf("%s hold UNRESOLVED merge conflicts", strings.Join(shown, ", "))
	}
	return fmt.Sprintf(
		"0 matches, but this is NOT a reliable \"not found\": %s%s, and a declaration below the "+
			"markers is often swallowed by the one above it rather than indexed. "+
			"`#'%s'::conflict` shows the regions and `::mine` / `::theirs` what each side declares; "+
			"node_edit accept:\"mine\"|\"theirs\" resolves them, after which this query is worth retrying",
		subject, more, shown[0])
}
