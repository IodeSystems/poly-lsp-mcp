package mcp

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// ApplyUnifiedDiff applies a unified-diff patch to `original` and
// returns the new bytes.
//
// Context matching is FUZZY on hunk position, strict on content: each
// hunk's leading context + removed lines are treated as a pattern and
// located in `original` — the hunk header's `@@ -OldStart,OldCount
// @@` numbers are only a HINT used to disambiguate, not a required
// exact anchor. This matters because LLM-generated patches routinely
// get the header numbers wrong (off-by-one, stale after an earlier
// hunk shifted line counts) while getting the context text right;
// real `patch`/`git apply` tolerate this and so do we.
//
//   - If the pattern matches exactly one location at/after the
//     previous hunk's applied position, apply there (regardless of
//     whether it agrees with OldStart).
//   - If it matches nowhere, error with the context that wasn't
//     found.
//   - If it matches multiple locations, OldStart is used to
//     disambiguate (a match exactly at OldStart wins); if none of the
//     matches sits at OldStart, that's a genuine ambiguity and we
//     error rather than guess.
//
// Hunks must apply in increasing-position order — a later hunk can't
// match before an earlier hunk's applied position — which keeps
// pattern search from mis-firing on a duplicated block earlier in the
// file.
//
// The diff header (`--- a/...`, `+++ b/...`) is optional and
// ignored: we only need the hunks. Line endings normalize to LF on
// output; CRLF input is accepted (each line's trailing `\r` is
// stripped before comparison).
//
// This is deliberately the minimal applier needed for the
// node_edit{file, diff} surface — LLM-generated patches almost
// always use canonical unified diff with context. If you need
// three-way merge, binary patches, or rename detection, use git
// apply.
func ApplyUnifiedDiff(original []byte, diff string) ([]byte, error) {
	hunks, err := parseUnifiedDiff(diff)
	if err != nil {
		return nil, err
	}
	if len(hunks) == 0 {
		return nil, fmt.Errorf("diff contains no hunks")
	}

	// Normalize original into lines for positional addressing.
	origLines := splitDiffLines(original)

	// Build output by interleaving unchanged prefixes with hunk
	// replacements. cursor = next line in origLines we haven't
	// emitted yet (1-based to match diff convention).
	var out []string
	cursor := 1
	for hi, h := range hunks {
		entries, err := normalizeHunkLines(h.Lines)
		if err != nil {
			return nil, fmt.Errorf("hunk %d: %w", hi+1, err)
		}

		// The pattern is the sequence of context (' ') + removed
		// ('-') lines, in order — what must exist in `original` for
		// this hunk to apply. '+' lines aren't part of the pattern;
		// they're pure insertions interleaved during the walk below.
		pattern := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.Tag == ' ' || e.Tag == '-' {
				pattern = append(pattern, e.Body)
			}
		}

		minPos := cursor - 1 // 0-based floor: can't match before prior hunk.
		hint := h.OldStart - 1
		anchor, err := locateHunkAnchor(origLines, pattern, hint, minPos)
		if err != nil {
			return nil, fmt.Errorf("hunk %d: %w", hi+1, err)
		}

		// Emit unchanged lines up to the hunk's located start.
		for cursor-1 < anchor {
			out = append(out, origLines[cursor-1])
			cursor++
		}

		// Walk the hunk lines, consuming context + removed lines from
		// `original` (already verified to match by locateHunkAnchor)
		// and emitting context + added lines.
		for _, e := range entries {
			switch e.Tag {
			case ' ':
				if cursor-1 >= len(origLines) || origLines[cursor-1] != e.Body {
					return nil, fmt.Errorf("hunk %d: internal error: context mismatch at line %d (want %q got %q)",
						hi+1, cursor, e.Body, safeLine(origLines, cursor-1))
				}
				out = append(out, e.Body)
				cursor++
			case '-':
				if cursor-1 >= len(origLines) || origLines[cursor-1] != e.Body {
					return nil, fmt.Errorf("hunk %d: internal error: removed-line mismatch at line %d (want %q got %q)",
						hi+1, cursor, e.Body, safeLine(origLines, cursor-1))
				}
				cursor++
			case '+':
				out = append(out, e.Body)
			}
		}
	}
	// Emit any tail past the last hunk.
	for cursor-1 < len(origLines) {
		out = append(out, origLines[cursor-1])
		cursor++
	}

	// Preserve trailing-newline shape from the original: if the
	// original ended with \n, ours should too. Splitting by \n
	// gives an empty trailing entry when the file ended with \n;
	// we strip it during split, then re-add here.
	joined := strings.Join(out, "\n")
	if endsWithNewline(original) {
		joined += "\n"
	}
	return []byte(joined), nil
}

// hunkEntry is one normalized line from a hunk body: its diff tag
// (' ' context, '-' removed, '+' added) and the line text with the
// tag stripped.
type hunkEntry struct {
	Tag  byte
	Body string
}

// normalizeHunkLines converts a hunk's raw body lines (as scanned,
// each still carrying its leading tag byte) into hunkEntrys. A raw
// line with zero length is treated as a blank-line context row (some
// generators emit a bare empty line instead of a lone space for
// blank context). `\ No newline at end of file` marker lines are
// dropped — trailing-newline shape is decided once, from the
// original file, not per-hunk.
func normalizeHunkLines(raw []string) ([]hunkEntry, error) {
	entries := make([]hunkEntry, 0, len(raw))
	for _, line := range raw {
		if len(line) == 0 {
			entries = append(entries, hunkEntry{Tag: ' ', Body: ""})
			continue
		}
		tag, body := line[0], line[1:]
		switch tag {
		case ' ', '-', '+':
			entries = append(entries, hunkEntry{Tag: tag, Body: body})
		case '\\':
			continue
		default:
			return nil, fmt.Errorf("unrecognized line prefix %q", string(tag))
		}
	}
	return entries, nil
}

// locateHunkAnchor finds the 0-based index into origLines where
// `pattern` (the hunk's context+removed lines, in order) begins.
// Search is restricted to origLines[minPos:] to preserve hunk order.
// `hint` (OldStart-1, possibly stale/wrong) only matters when the
// pattern isn't uniquely located — it's the tiebreaker, not the
// primary anchor.
//
// A hunk with no context/removed lines (pure insertion — only '+'
// lines) has an empty pattern; there's nothing to locate, so it
// anchors directly at the hint (clamped into range).
func locateHunkAnchor(origLines, pattern []string, hint, minPos int) (int, error) {
	if minPos < 0 {
		minPos = 0
	}
	if len(pattern) == 0 {
		anchor := hint
		if anchor < minPos {
			anchor = minPos
		}
		if anchor > len(origLines) {
			anchor = len(origLines)
		}
		return anchor, nil
	}

	var matches []int
	for i := minPos; i+len(pattern) <= len(origLines); i++ {
		if linesEqual(origLines[i:i+len(pattern)], pattern) {
			matches = append(matches, i)
		}
	}

	switch len(matches) {
	case 0:
		near := minPos
		if hint >= minPos && hint < len(origLines) {
			near = hint
		}
		return -1, fmt.Errorf("context not found: want %q (%d line(s)) at/after line %d; near line %d got %q",
			pattern[0], len(pattern), minPos+1, near+1, safeLine(origLines, near))
	case 1:
		return matches[0], nil
	default:
		for _, m := range matches {
			if m == hint {
				return m, nil
			}
		}
		lines := formatMatchLines(matches)
		return -1, fmt.Errorf(
			"context matches %d locations (lines %s); ambiguous — refusing to guess which one you mean. "+
				"Re-issue as a precise range edit instead of a diff hunk: node_edit{file, startLine:<one of %s>, startCol, endLine, endCol, newText} targeting the exact location — range mode needs no line-arithmetic guessing, unlike diff hunk headers.",
			len(matches), lines, lines)
	}
}

// linesEqual reports whether two line slices are elementwise equal.
func linesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// formatMatchLines renders 0-based match indices as a human-readable
// 1-based list for error messages, e.g. "12, 47, 103".
func formatMatchLines(matches []int) string {
	parts := make([]string, len(matches))
	for i, m := range matches {
		parts[i] = strconv.Itoa(m + 1)
	}
	return strings.Join(parts, ", ")
}

type diffHunk struct {
	OldStart, OldCount int
	NewStart, NewCount int
	Lines              []string // raw lines including leading " "/"+"/"-"/"\\"
}

// parseUnifiedDiff walks `s` line by line, parsing each hunk header
// `@@ -X,Y +A,B @@` and accumulating the body lines that follow.
// Header lines (`---`, `+++`, anything else outside a hunk) are
// silently dropped.
func parseUnifiedDiff(s string) ([]diffHunk, error) {
	var hunks []diffHunk
	var current *diffHunk

	sc := bufio.NewScanner(strings.NewReader(s))
	// Allow long lines (the default 64KB ceiling truncates large
	// generated patches).
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "@@") {
			h, err := parseHunkHeader(line)
			if err != nil {
				return nil, err
			}
			hunks = append(hunks, h)
			current = &hunks[len(hunks)-1]
			continue
		}
		if current == nil {
			// Outside any hunk — skip header lines silently.
			continue
		}
		current.Lines = append(current.Lines, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan diff: %w", err)
	}
	return hunks, nil
}

// parseHunkHeader parses `@@ -X[,Y] +A[,B] @@ optional-context`.
func parseHunkHeader(line string) (diffHunk, error) {
	// Strip the leading "@@" and the trailing "@@..." segment.
	rest := strings.TrimPrefix(line, "@@")
	close := strings.Index(rest, "@@")
	if close < 0 {
		return diffHunk{}, fmt.Errorf("malformed hunk header: %q", line)
	}
	body := strings.TrimSpace(rest[:close])
	parts := strings.Fields(body)
	if len(parts) < 2 {
		return diffHunk{}, fmt.Errorf("hunk header missing -/+ parts: %q", line)
	}
	oldStart, oldCount, err := parseDiffRange(parts[0], '-')
	if err != nil {
		return diffHunk{}, err
	}
	newStart, newCount, err := parseDiffRange(parts[1], '+')
	if err != nil {
		return diffHunk{}, err
	}
	return diffHunk{
		OldStart: oldStart, OldCount: oldCount,
		NewStart: newStart, NewCount: newCount,
	}, nil
}

// parseDiffRange handles "-X" or "-X,Y" (and the `+` variant).
// Missing count defaults to 1 per the diff spec.
func parseDiffRange(s string, sign byte) (start, count int, err error) {
	if len(s) < 2 || s[0] != sign {
		return 0, 0, fmt.Errorf("range must start with %q: got %q", sign, s)
	}
	s = s[1:]
	if comma := strings.IndexByte(s, ','); comma >= 0 {
		startStr, countStr := s[:comma], s[comma+1:]
		start, err = strconv.Atoi(startStr)
		if err != nil {
			return 0, 0, fmt.Errorf("bad start %q: %v", startStr, err)
		}
		count, err = strconv.Atoi(countStr)
		if err != nil {
			return 0, 0, fmt.Errorf("bad count %q: %v", countStr, err)
		}
		return start, count, nil
	}
	start, err = strconv.Atoi(s)
	if err != nil {
		return 0, 0, fmt.Errorf("bad start %q: %v", s, err)
	}
	return start, 1, nil
}

// splitDiffLines splits content on \n, drops the trailing empty
// entry produced by a final newline, and strips \r from CRLF inputs
// so the comparison against the diff (which is LF) succeeds.
func splitDiffLines(content []byte) []string {
	lines := strings.Split(string(content), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

func endsWithNewline(content []byte) bool {
	return len(content) > 0 && content[len(content)-1] == '\n'
}

// safeLine returns the line at index i or "<EOF>" if out of range.
// Keeps the error message non-panicky.
func safeLine(lines []string, i int) string {
	if i < 0 || i >= len(lines) {
		return "<EOF>"
	}
	return lines[i]
}
