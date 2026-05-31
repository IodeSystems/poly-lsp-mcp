package mcp

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// ApplyUnifiedDiff applies a unified-diff patch to `original` and
// returns the new bytes. Strict context matching — every context
// line in the diff must match the original verbatim; mismatches
// surface as an error (no fuzzy / offset healing).
//
// The diff header (`--- a/...`, `+++ b/...`) is optional and
// ignored: we only need the hunks. Hunks must appear in
// increasing-line order. Line endings normalize to LF on output;
// CRLF input is accepted (each line's trailing `\r` is stripped
// before comparison).
//
// This is deliberately the minimal applier needed for the
// node_edit{file, diff} surface — LLM-generated patches almost
// always use canonical unified diff with context. If you need
// fuzzy matching, three-way merge, binary patches, or rename
// detection, use git apply.
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
		if h.OldStart < cursor {
			return nil, fmt.Errorf("hunk %d: starts at line %d which is before cursor %d (hunks out of order?)",
				hi+1, h.OldStart, cursor)
		}
		// Emit unchanged lines up to the hunk's start.
		for cursor < h.OldStart && cursor-1 < len(origLines) {
			out = append(out, origLines[cursor-1])
			cursor++
		}
		// Walk the hunk lines, verifying context + removed lines
		// match the original at `cursor`, and emitting context +
		// added lines.
		for _, line := range h.Lines {
			if len(line) == 0 {
				// Empty line in the hunk body — treat as context
				// blank line.
				if cursor-1 < len(origLines) && origLines[cursor-1] == "" {
					out = append(out, "")
					cursor++
					continue
				}
				return nil, fmt.Errorf("hunk %d: blank-line context didn't match original at line %d", hi+1, cursor)
			}
			tag, body := line[0], line[1:]
			switch tag {
			case ' ':
				if cursor-1 >= len(origLines) || origLines[cursor-1] != body {
					return nil, fmt.Errorf("hunk %d: context mismatch at line %d: want %q got %q",
						hi+1, cursor, body, safeLine(origLines, cursor-1))
				}
				out = append(out, body)
				cursor++
			case '-':
				if cursor-1 >= len(origLines) || origLines[cursor-1] != body {
					return nil, fmt.Errorf("hunk %d: removed-line mismatch at line %d: want %q got %q",
						hi+1, cursor, body, safeLine(origLines, cursor-1))
				}
				cursor++
			case '+':
				out = append(out, body)
			case '\\':
				// `\ No newline at end of file` marker. Acknowledge
				// by not appending a trailing newline; handled by
				// the joiner below via trailingNewline bookkeeping.
				continue
			default:
				return nil, fmt.Errorf("hunk %d: unrecognized line prefix %q", hi+1, string(tag))
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
