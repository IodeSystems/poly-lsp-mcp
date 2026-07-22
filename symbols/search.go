package symbols

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// maxHitLineBytes caps how much of a single matched line a hit carries.
// A hit's Text is the WHOLE matched line, and one line in a generated
// bundle (minified JS, a gql document) can be tens of thousands of bytes
// — so 100 hits could dump megabytes and blow the tool-result token
// budget, a hard stop mid-task. Past this width the line is elided around
// the match with a "(+N chars)" marker, which keeps the match visible
// while bounding the payload.
const maxHitLineBytes = 500

// maxSearchLineBytes is the "this file is generated" threshold. A source
// file whose LONGEST line exceeds this is a minified/generated bundle
// (hand-written code effectively never has a 5000-byte line), and its
// matches are noise, not navigation targets. Search skips such files and
// reports the count, so the skip is loud, never silent.
const maxSearchLineBytes = 5000

// CapHitLine returns a rune-safe rendering of `line` no longer than
// maxHitLineBytes, centred on the match span [matchStart,matchEnd) (byte
// offsets into line), with a "(+N chars)" marker standing in for the
// elided head and/or tail. A line already within the cap is returned
// unchanged. Pass (0,0) for a context line with no match — it keeps the
// head. This is the per-line half of the budget guard; a generated file
// is skipped whole by Search, but a merely long line in a real file is
// trimmed here so the match still shows.
func CapHitLine(line string, matchStart, matchEnd int) string {
	n := len(line)
	if n <= maxHitLineBytes {
		return line
	}
	if matchStart < 0 || matchStart > n {
		matchStart = 0
	}
	if matchEnd < matchStart || matchEnd > n {
		matchEnd = matchStart
	}
	var start, end int
	if mlen := matchEnd - matchStart; mlen >= maxHitLineBytes {
		start, end = matchStart, matchStart+maxHitLineBytes // match alone overflows; show its head
	} else {
		slack := maxHitLineBytes - mlen
		start, end = matchStart-slack/2, matchEnd+(slack-slack/2)
		if start < 0 {
			end -= start
			start = 0
		}
		if end > n {
			start -= end - n
			end = n
		}
		if start < 0 {
			start = 0
		}
	}
	// Snap the window inward to rune boundaries so a cut never lands
	// mid-rune (which would emit invalid UTF-8).
	for start > 0 && start < n && !utf8.RuneStart(line[start]) {
		start++
	}
	for end < n && !utf8.RuneStart(line[end]) {
		end++
	}
	var b strings.Builder
	if start > 0 {
		fmt.Fprintf(&b, "(+%d chars)", start)
	}
	b.WriteString(line[start:end])
	if end < n {
		fmt.Fprintf(&b, "(+%d chars)", n-end)
	}
	return b.String()
}

// exceedsLineBudget reports whether any line in content is longer than
// limit bytes — the generated-file heuristic, computed in one pass over
// the newline offsets without allocating the split.
func exceedsLineBudget(content []byte, limit int) bool {
	start := 0
	for i, b := range content {
		if b == '\n' {
			if i-start > limit {
				return true
			}
			start = i + 1
		}
	}
	return len(content)-start > limit
}

// SearchHit is one regex match inside a workspace file. Position is
// 1-based — Line counts from 1, Col is the byte offset (not rune
// offset) of the first byte of the match within Line. MatchEndCol is
// the byte offset one past the match. Text is the entire line
// containing the match (without trailing newline), letting the
// caller render context without re-reading the file.
type SearchHit struct {
	File        string
	Line        int
	Col         int
	MatchEndCol int
	Text        string

	// Before / After are the N lines preceding and following the
	// matched line, when the caller asked for context. Empty
	// otherwise.
	Before []string
	After  []string
}

// SearchOptions tunes Search's behavior. Zero values mean "use
// defaults" — Limit=0 means unbounded, ContextLines=0 means no
// context lines.
type SearchOptions struct {
	// Glob is a filepath.Match pattern matched against each file's
	// basename. Empty matches every file. Supports `?`, `*`,
	// character classes — no `**` recursion (which is implicit
	// from the workspace walk anyway).
	Glob string
	// Limit caps the total number of hits returned. Excess hits
	// are counted in the dropped return value but not emitted.
	// Zero means unbounded; pass a positive integer for any real
	// workload.
	Limit int
	// ContextLines is the number of lines before AND after each
	// matched line to include. Useful for previewing without a
	// follow-up node_read.
	ContextLines int
	// IncludeGenerated disables the "skip files with a pathologically
	// long line" heuristic. Off by default: generated/minified bundles
	// are noise and their giant lines blow the token budget. Set it only
	// when the caller genuinely wants to grep generated output.
	IncludeGenerated bool
}

// Search walks `root` recursively and returns every regex hit in
// file contents. Skips the same noise directories as Build
// (.git / node_modules / vendor / __pycache__ / etc.) and the same
// per-file size cap (1 MiB; bigger files are silently dropped to
// keep memory bounded on huge generated blobs).
//
// Returns the matching hits (sorted by file then line then column),
// the count of hits dropped past Limit (zero when Limit is zero or
// unreached), and any I/O error from the walk itself. Per-file read
// errors are silently skipped so one unreadable file doesn't abort
// the search.
//
// pattern is required. Pass `(?i)…` for case-insensitive matching
// (Go's regexp syntax supports inline flags natively — no separate
// option needed).
func Search(root string, pattern *regexp.Regexp, opts SearchOptions) ([]SearchHit, int, int, error) {
	if pattern == nil {
		return nil, 0, 0, fmt.Errorf("search: pattern is required")
	}
	var hits []SearchHit
	dropped := 0
	skippedGenerated := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if opts.Glob != "" {
			match, err := filepath.Match(opts.Glob, d.Name())
			if err != nil {
				return fmt.Errorf("invalid glob %q: %w", opts.Glob, err)
			}
			if !match {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxFileSize {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		// Quick heuristic: skip files that look binary. Real text
		// files don't have null bytes in the first 8 KB.
		probe := content
		if len(probe) > 8192 {
			probe = probe[:8192]
		}
		if bytes.IndexByte(probe, 0) >= 0 {
			return nil
		}
		// A file with a pathologically long line is generated/minified;
		// its matches are noise. Skip it whole and count it (reported,
		// never silent).
		if !opts.IncludeGenerated && exceedsLineBudget(content, maxSearchLineBytes) {
			skippedGenerated++
			return nil
		}

		fileHits := searchFile(path, content, pattern, opts.ContextLines)
		for _, h := range fileHits {
			if opts.Limit > 0 && len(hits) >= opts.Limit {
				dropped++
				continue
			}
			hits = append(hits, h)
		}
		return nil
	})
	if err != nil {
		return nil, 0, 0, err
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].File != hits[j].File {
			return hits[i].File < hits[j].File
		}
		if hits[i].Line != hits[j].Line {
			return hits[i].Line < hits[j].Line
		}
		return hits[i].Col < hits[j].Col
	})
	return hits, dropped, skippedGenerated, nil
}

// searchFile scans content line by line and produces one SearchHit
// per match. Lines are split on \n; trailing \r stripped from each
// line so Text matches the editor view of CRLF files.
func searchFile(path string, content []byte, pattern *regexp.Regexp, contextLines int) []SearchHit {
	lines := strings.Split(string(content), "\n")
	// Drop trailing empty entry from a final \n so line numbers
	// match editor reckoning.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}

	var out []SearchHit
	for lineIdx, line := range lines {
		spans := pattern.FindAllStringIndex(line, -1)
		if len(spans) == 0 {
			continue
		}
		for _, span := range spans {
			out = append(out, SearchHit{
				File:        path,
				Line:        lineIdx + 1,
				Col:         span[0] + 1,
				MatchEndCol: span[1] + 1,
				// Text is display-only; Col/MatchEndCol keep the true
				// file offsets. A long line is elided around the match so
				// one generated line can't blow the token budget.
				Text:   CapHitLine(line, span[0], span[1]),
				Before: capContextLines(contextBefore(lines, lineIdx, contextLines)),
				After:  capContextLines(contextAfter(lines, lineIdx, contextLines)),
			})
		}
	}
	return out
}

// capContextLines trims each context line to the per-line budget. A
// context line has no match to centre on, so it keeps the head.
func capContextLines(lines []string) []string {
	for i, l := range lines {
		lines[i] = CapHitLine(l, 0, 0)
	}
	return lines
}

func contextBefore(lines []string, idx, n int) []string {
	if n <= 0 || idx == 0 {
		return nil
	}
	start := idx - n
	if start < 0 {
		start = 0
	}
	out := make([]string, idx-start)
	copy(out, lines[start:idx])
	return out
}

func contextAfter(lines []string, idx, n int) []string {
	if n <= 0 {
		return nil
	}
	end := idx + 1 + n
	if end > len(lines) {
		end = len(lines)
	}
	if idx+1 >= end {
		return nil
	}
	out := make([]string, end-(idx+1))
	copy(out, lines[idx+1:end])
	return out
}
