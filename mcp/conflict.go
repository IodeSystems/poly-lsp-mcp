package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
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
